// Portions adapted from dontmove-kit, Copyright (c) 2026 zxd (MIT).
// See ThirdParty/dontmove-kit.LICENSE and ThirdParty/ThirdPartyNotices.md.
package at

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/id"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/qdc507"
	"github.com/google/uuid"
)

// Adapter is the conservative V1 AT command adapter. It deliberately keeps
// vendor-specific audio commands out of the control path; voice audio is
// provided by the selected VoiceAudio backend while this type owns calls,
// SMS, and line state.
type Adapter struct {
	client *Client
	mu     sync.Mutex
	active modem.CallID
	peer   string
	closed bool
	capMu  sync.RWMutex
	caps   modem.Capabilities
	events chan modem.ModemEvent
	close  sync.Once
}

func NewAdapter(client *Client) *Adapter {
	adapter := &Adapter{client: client, events: make(chan modem.ModemEvent, 32)}
	if client != nil {
		client.SetLineHandler(adapter.HandleURC)
	}
	return adapter
}

func (a *Adapter) Probe(ctx context.Context) (modem.Capabilities, error) {
	if a.client == nil {
		return modem.Capabilities{}, fmt.Errorf("AT client is unavailable")
	}
	manufacturer, err := firstPayload(a.client.Exchange(ctx, "AT+CGMI"))
	if err != nil {
		return modem.Capabilities{}, err
	}
	model, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMM"))
	revision, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMR"))
	capabilities := modem.Capabilities{
		Vendor:              manufacturer,
		Model:               model,
		Tier:                modem.CapabilitySMSOnly,
		SMS:                 true,
		FirmwareFingerprint: strings.TrimSpace(manufacturer + " " + model + " " + revision),
	}
	if voiceCapabilities, capabilityErr := qdc507.ProbeCapabilities(ctx, a.client, manufacturer, model); capabilityErr == nil {
		capabilities.Voice = voiceCapabilities.Voice
		capabilities.DTMF = voiceCapabilities.DTMF
		capabilities.Audio = voiceCapabilities.Audio
		capabilities.RequiresBootstrap = voiceCapabilities.RequiresBootstrap
		capabilities.Tier = voiceCapabilities.Tier
	}
	a.capMu.Lock()
	a.caps = capabilities
	a.capMu.Unlock()
	return capabilities, nil
}

func (a *Adapter) Status(ctx context.Context) (modem.LineStatus, error) {
	if a.client == nil {
		return modem.LineStatus{}, fmt.Errorf("AT client is unavailable")
	}
	simLines, err := a.client.Exchange(ctx, "AT+CPIN?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	registrationLines, err := a.client.Exchange(ctx, "AT+CEREG?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	signalLines, err := a.client.Exchange(ctx, "AT+CSQ")
	if err != nil {
		return modem.LineStatus{}, err
	}
	a.capMu.RLock()
	capabilities := a.caps
	a.capMu.RUnlock()
	if capabilities.Tier == "" {
		capabilities.Tier = modem.CapabilitySMSOnly
	}
	voiceState := "unavailable"
	if capabilities.Voice {
		voiceState = "control_only"
	}
	status := modem.LineStatus{
		SIM:            normalizeSIM(payload(simLines)),
		Registration:   normalizeRegistration(payload(registrationLines)),
		Signal:         modem.Signal{RSSI: parseRSSI(payload(signalLines))},
		CapabilityTier: capabilities.Tier,
		Voice:          voiceState,
		SMS:            "ready",
	}
	status.Signal.Bars = signalBars(status.Signal.RSSI)
	a.mu.Lock()
	if a.active != "" {
		active := a.active
		status.ActiveCallID = &active
	}
	a.mu.Unlock()
	return status, nil
}

// WaitReady polls the AT channel until the modem answers "AT" with
// OK (or the context ends). After a NAS reboot the module's internal
// system re-boots on its own schedule; waiting here turns a dial's
// failure mode from "immediate 500" into "ring while the module
// comes up".
func (a *Adapter) WaitReady(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("AT client is unavailable")
	}
	// Each probe gets a short independent timeout so a hung port does
	// not consume the whole ready budget.
	deadline := 120 * time.Second
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	attempt := 0
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("QDC507 AT channel not ready after %s: %w", deadline, err)
		}
		probeCtx, probeCancel := context.WithTimeout(waitCtx, 2*time.Second)
		_, err := a.client.Exchange(probeCtx, "AT")
		probeCancel()
		if err == nil {
			slog.Info("modem AT channel ready", "after_attempts", attempt+1)
			return nil
		}
		attempt++
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *Adapter) Dial(ctx context.Context, peer string) (modem.CallID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	callID := modem.CallID(uuid.NewString())
	a.mu.Lock()
	if a.active != "" || a.closed {
		a.mu.Unlock()
		return "", modem.ErrActiveCall
	}
	// Reserve the physical call before ATD, so an interleaved terminal URC
	// cannot be lost and a concurrent dial cannot take the same line.
	a.active, a.peer = callID, strings.TrimSpace(peer)
	a.emitLocked(modem.ModemEvent{Kind: "outgoing", CallID: callID, Peer: a.peer})
	a.mu.Unlock()
	// After a NAS reboot the module's internal system boots on its own
	// schedule (USB re-enumeration + Android userspace, which can take
	// minutes). Sending ATD into a not-yet-ready modem just burns the
	// dial deadline and produces a 500 to the SIP client. The dial path
	// calls WaitReady upfront (see SIPCallSession.Dial), so by the time
	// we reach here the AT channel is known to answer; keep ATD's own
	// deadline tight so a mid-dial hang still fails fast.
	if _, err := a.client.Exchange(ctx, "ATD"+strings.TrimSpace(peer)+";"); err != nil {
		if errors.Is(err, ErrCommandFailed) {
			a.finishActive(callID, "dial_failed")
			return "", err
		}
		// A timeout does not prove ATD failed. Keep ownership so the SIP
		// teardown can explicitly release a potentially accepted call.
		return callID, err
	}
	if err := a.validateActive(callID); err != nil {
		return "", fmt.Errorf("call ended while dialing: %w", err)
	}
	return callID, nil
}

func (a *Adapter) Answer(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "ATA")
	return err
}

func (a *Adapter) Hangup(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "AT+CHUP")
	if errors.Is(err, ErrCommandFailed) {
		_, err = a.client.Exchange(ctx, "ATH") // Explicit rejection only, never an unknown timeout.
	}
	if err != nil {
		return err
	}
	// ATH can return OK while configured to ignore voice hangup. Confirm
	// the voice leg has disappeared before publishing a completed call.
	for {
		if a.validateActive(callID) != nil {
			return nil
		}
		lines, err := a.client.Exchange(ctx, "AT+CLCC")
		if err != nil {
			return fmt.Errorf("verify cellular hangup: %w", err)
		}
		if _, found := clccVoice(lines, modem.CLCCDirAny, 0, 1, 2, 3, 4, 5); !found {
			a.finishActive(callID, "local_hangup")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("voice call remains after hangup: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a *Adapter) SendDTMF(ctx context.Context, callID modem.CallID, digit rune) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	if !strings.ContainsRune("0123456789*#ABCD", digit) {
		return fmt.Errorf("invalid DTMF digit %q", digit)
	}
	_, err := a.client.Exchange(ctx, fmt.Sprintf("AT+VTS=\"%c\"", digit))
	return err
}

func (a *Adapter) SendSMS(ctx context.Context, destination string, payload modem.SMSPayload) (modem.SMSID, error) {
	// Use the engine's encoded PDU for short codes as well: text mode with
	// CSCS=GSM corrupts Chinese and loses multipart headers.
	if payload.PDU == "" {
		return "", fmt.Errorf("SMS PDU is required")
	}
	pduBytes := len(strings.TrimSpace(payload.PDU)) / 2
	if pduBytes < 2 {
		return "", fmt.Errorf("invalid SMS PDU length")
	}
	// AT+CMGS takes TPDU octets, excluding the SMSC length octet.
	smscLength := 0
	if value, err := strconv.ParseInt(payload.PDU[:2], 16, 8); err == nil {
		smscLength = int(value)
	}
	tpduLength := pduBytes - 1 - smscLength
	if tpduLength < 1 {
		return "", fmt.Errorf("invalid SMS TPDU length")
	}
	if _, err := a.client.SendPDU(ctx, tpduLength, payload.PDU); err != nil {
		if errors.Is(err, ErrSubmissionResultUnknown) {
			return "", fmt.Errorf("%w: %v", modem.ErrSMSSubmissionUnknown, err)
		}
		return "", err
	}
	messageID, err := id.New("sms_", 16)
	return modem.SMSID(messageID), err
}

func (a *Adapter) ListSMS(ctx context.Context, cursor modem.SMSCursor) ([]modem.RawSMS, modem.SMSCursor, error) {
	// Explicitly select the modem's message storage ("MT" = module phone
	// memory). Without this, CMGL lists the *current preferred* storage
	// (often SM/SR on QDC507), which is empty because inbound SMS lands
	// in MT — so messages are never ingested, pushed, or deleted.
	// Observed 2026-09-06: +CPMS showed "MT",2,23 but CMGL=4 returned
	// nothing; user stopped receiving SMS entirely.
	lines, err := a.client.ExchangeSequence(ctx, "AT+CPMS=\"MT\"", "AT+CMGF=0", "AT+CMGL=4")
	if err != nil {
		return nil, cursor, err
	}
	var result []modem.RawSMS
	var current *modem.RawSMS
	lastIndex := strings.TrimSpace(string(cursor))
	for _, line := range lines {
		if strings.HasPrefix(line, "+CMGL:") {
			current = nil
			fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CMGL:")), ",")
			if len(fields) < 2 || (strings.TrimSpace(fields[1]) != "0" && strings.TrimSpace(fields[1]) != "1") {
				// Leave stored drafts and outgoing messages untouched.
				continue
			}
			index, parseErr := strconv.Atoi(strings.TrimSpace(fields[0]))
			if parseErr != nil {
				continue
			}
			current = &modem.RawSMS{ModemStorage: "MT", ModemIndex: index}
			lastIndex = strconv.Itoa(index)
			continue
		}
		if current != nil && !IsFinal(line) && !strings.HasPrefix(line, "+") {
			pdu, decodeErr := hex.DecodeString(strings.TrimSpace(line))
			if decodeErr == nil {
				current.RawPDU = pdu
				result = append(result, *current)
			}
			current = nil
		}
	}
	return result, modem.SMSCursor(lastIndex), nil
}

func (a *Adapter) DeleteSMS(ctx context.Context, storageIndex string) error {
	// CMGD without an explicit storage deletes from the *current
	// preferred* storage; pin it to MT (same reason as ListSMS) so
	// engine.go's post-ingest cleanup actually removes the message.
	index, err := strconv.Atoi(storageIndex)
	if err != nil || index < 1 {
		return fmt.Errorf("invalid SMS storage index")
	}
	if _, err := a.client.ExchangeSequence(ctx, "AT+CPMS=\"MT\"", fmt.Sprintf("AT+CMGD=%d", index)); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) Events() <-chan modem.ModemEvent { return a.events }

// clccActive reports whether the +CLCC lines describe a call in the given
// direction that has reached state 0 (active). dir < 0 matches either
// direction. Kept separate from the polling loop so the direction matching
// is unit-testable without a live modem.
func clccActive(lines []string, dir int) bool {
	_, found := clccVoice(lines, dir, 0)
	return found
}

// QDC507 also lists empty data legs as active. Only mode=0 is a voice
// call; accepting mode=1 can start capture before the caller has answered.
func clccVoice(lines []string, dir int, states ...int) (string, bool) {
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		// +CLCC: <id>,<dir>,<state>,<mode>,<mpty>,<number>,<type>
		reader := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:"))))
		reader.TrimLeadingSpace = true
		fields, err := reader.Read()
		if err != nil || len(fields) < 5 {
			continue
		}
		callDir, dirErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		state, stateErr := strconv.Atoi(strings.TrimSpace(fields[2]))
		mode, modeErr := strconv.Atoi(strings.TrimSpace(fields[3]))
		if dirErr != nil || stateErr != nil || modeErr != nil || mode != 0 || callDir < 0 || callDir > 1 {
			continue
		}
		for _, wanted := range states {
			if state == wanted && (dir < 0 || callDir == dir) {
				peer := ""
				if len(fields) >= 7 {
					peer = callerNumber(fields[5], fields[6])
				}
				return peer, true
			}
		}
	}
	return "", false
}

func callerNumber(number, kind string) string {
	number = strings.TrimSpace(number)
	if number != "" && strings.TrimSpace(kind) == "145" && !strings.HasPrefix(number, "+") {
		number = "+" + number
	}
	return number
}

// CallerNumber is a bounded read-only fallback when RING precedes +CLIP.
func (a *Adapter) CallerNumber(ctx context.Context) (string, error) {
	a.mu.Lock()
	callID, peer := a.active, a.peer
	a.mu.Unlock()
	if callID == "" || peer != "" {
		return peer, nil
	}
	lines, err := a.client.Exchange(ctx, "AT+CLCC")
	if err != nil {
		return "", err
	}
	if peer, found := clccVoice(lines, modem.CLCCDirIncoming, 0, 4, 5); found {
		a.updatePeer(callID, peer)
		return peer, nil
	}
	return "", nil
}

func (a *Adapter) updatePeer(callID modem.CallID, peer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if callID != "" && a.active == callID && peer != "" && peer != a.peer {
		a.peer = peer
		a.emitLocked(modem.ModemEvent{Kind: "caller_id", CallID: callID, Peer: peer})
	}
}

// WaitActive polls AT+CLCC until an outgoing call is answered. Kept for the
// dial path; inbound uses WaitActiveDir with dir=1.
func (a *Adapter) WaitActive(ctx context.Context) (bool, error) {
	return a.WaitActiveDir(ctx, modem.CLCCDirOutgoing)
}

// WaitActiveDir polls AT+CLCC until a call in the given direction reaches
// state 0 (active), or the context ends. Opening UAC capture before the
// cellular leg is active wedges the ALSA ASYNC stream into an XRUN that
// reads silence forever, so the SIP bridge must wait for this before
// starting audio.
//
// Upstream hard-coded dir=0 here. An inbound (mobile terminated) call is
// reported with dir=1, so the answer path could never confirm the pickup:
// it blocked for its entire deadline and only then started the audio
// bridge — by which time the caller had already hung up. Every answered
// inbound call was silent while outbound worked, because outbound really is
// dir=0.
func (a *Adapter) WaitActiveDir(ctx context.Context, dir int) (bool, error) {
	a.mu.Lock()
	callID := a.active
	a.mu.Unlock()
	var lastSeen []string
	for {
		if callID == "" || a.validateActive(callID) != nil {
			return false, modem.ErrNoActiveCall
		}
		select {
		case <-ctx.Done():
			slog.Warn("clcc wait timed out", "dir", dir, "last_clcc", lastSeen)
			return false, ctx.Err()
		default:
		}
		lines, err := a.client.Exchange(ctx, "AT+CLCC")
		if err == nil {
			if peer, active := clccVoice(lines, dir, 0); active {
				a.updatePeer(callID, peer)
				slog.Info("clcc call active", "dir", dir, "lines", lines)
				return true, nil
			}
			lastSeen = lines
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			slog.Warn("clcc wait timed out", "dir", dir, "last_clcc", lastSeen)
			return false, ctx.Err()
		}
	}
}

func (a *Adapter) Run(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("AT client is unavailable")
	}
	return a.client.Run(ctx)
}

// HandleURC is called by a serial reader when a line arrives outside an AT
// command exchange. It is public so a platform-specific reader can preserve
// unsolicited RING/NO CARRIER events without coupling it to the parser.
func (a *Adapter) HandleURC(line string) {
	line = strings.TrimSpace(line)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	callID := a.active
	var event modem.ModemEvent
	switch {
	case line == "RING", strings.HasPrefix(line, "+CLIP:"):
		peer := ""
		if strings.HasPrefix(line, "+CLIP:") {
			reader := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CLIP:"))))
			reader.TrimLeadingSpace = true
			fields, err := reader.Read()
			if err != nil || len(fields) < 2 {
				return
			}
			peer = callerNumber(fields[0], fields[1])
		}
		if callID == "" {
			callID = modem.CallID(uuid.NewString())
			a.active, a.peer = callID, ""
		}
		if peer != "" {
			a.peer = peer
		}
		event = modem.ModemEvent{Kind: "incoming", CallID: callID, Peer: a.peer}
	case strings.Contains(line, "NO CARRIER"), strings.Contains(line, "BUSY"), strings.Contains(line, "NO ANSWER"):
		event = modem.ModemEvent{Kind: "ended", CallID: callID, Raw: line}
		// The modem terminated the call (e.g. an unanswered inbound call).
		// Clear the active marker here, or every later dial fails with
		// ErrActiveCall until the process restarts.
		if a.active == callID && callID != "" {
			a.active, a.peer = "", ""
		}
	default:
		return
	}
	a.emitLocked(event)
}

func (a *Adapter) Close() error {
	var err error
	a.close.Do(func() {
		a.mu.Lock()
		a.closed = true
		close(a.events)
		a.mu.Unlock()
		if a.client != nil {
			err = a.client.Close()
		}
	})
	return err
}

func (a *Adapter) validateActive(callID modem.CallID) error {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active == "" || (callID != "" && active != callID) {
		return fmt.Errorf("%w: active call mismatch", modem.ErrNoActiveCall)
	}
	return nil
}

func (a *Adapter) finishActive(callID modem.CallID, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == callID {
		a.active, a.peer = "", ""
		a.emitLocked(modem.ModemEvent{Kind: "ended", CallID: callID, Raw: reason})
	}
}

func (a *Adapter) emitLocked(event modem.ModemEvent) {
	if a.closed {
		return
	}
	select {
	case a.events <- event:
	default:
		slog.Error("modem event queue full", "kind", event.Kind, "call_id", event.CallID)
	}
}

func firstPayload(lines []string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return payload(lines), nil
}

func payload(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !IsFinal(line) && !strings.HasPrefix(line, "AT") {
			return line
		}
	}
	return ""
}

func normalizeSIM(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.Contains(value, "ready"):
		return "ready"
	case strings.Contains(value, "pin"), strings.Contains(value, "puk"):
		return "locked"
	case strings.Contains(value, "not ready"):
		return "absent"
	default:
		return "unknown"
	}
}

func normalizeRegistration(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, ","); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	switch value {
	case "1", "5":
		return "registered"
	case "2":
		return "searching"
	case "3":
		return "denied"
	default:
		return "unknown"
	}
}

func parseRSSI(value string) int {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, ":"); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	if index := strings.Index(value, ","); index >= 0 {
		value = value[:index]
	}
	rssi, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + rssi*2
}

func signalBars(rssi int) int {
	switch {
	case rssi >= -75:
		return 4
	case rssi >= -90:
		return 3
	case rssi >= -105:
		return 2
	case rssi > -113:
		return 1
	default:
		return 0
	}
}

var _ modem.ModemControl = (*Adapter)(nil)
