// Portions adapted from dontmove-kit, Copyright (c) 2026 zxd (MIT).
// See ThirdParty/dontmove-kit.LICENSE and ThirdParty/ThirdPartyNotices.md.
package modem

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// +CLCC <dir> field values. The SIP answer path must know which direction
// it is waiting on: a dialled call is reported dir=0, an answered inbound
// call dir=1.
const (
	CLCCDirOutgoing = 0
	CLCCDirIncoming = 1
	CLCCDirAny      = -1
)

// ActiveCallAdapter translates the modem's opaque CallID into the single
// active-call control surface used by the V1 HTTP state machine. V1 permits
// one SIM/module and one concurrent call, so this mapping is intentionally
// small and is reset on hangup.
type ActiveCallAdapter struct {
	Control      ModemControl
	mu           sync.Mutex
	active       CallID
	logical      CallID
	lastPhysical CallID
	lastLogical  CallID
	events       chan ModemEvent
	close        sync.Once
}

func NewActiveCallAdapter(control ModemControl) *ActiveCallAdapter {
	adapter := &ActiveCallAdapter{Control: control, events: make(chan ModemEvent, 32)}
	go adapter.forwardEvents()
	return adapter
}

func (a *ActiveCallAdapter) SetLogicalCallID(callID string) {
	a.mu.Lock()
	a.logical = CallID(callID)
	a.mu.Unlock()
}

// WaitReady waits until the modem's AT channel answers. Exposed for
// dial-path sequencing (see SIPCallSession.Dial): a NAS reboot leaves
// the module's userspace booting for minutes, and ATD/adb issued
// before it is up only burn deadlines and produce SIP 500s.
func (a *ActiveCallAdapter) WaitReady(ctx context.Context) error {
	return a.Control.WaitReady(ctx)
}

func (a *ActiveCallAdapter) Dial(ctx context.Context, peer string) error {
	callID, err := a.DialWithID(ctx, peer)
	if err != nil && callID != "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanupErr := a.HangupCall(cleanupCtx, callID); cleanupErr != nil {
			return fmt.Errorf("dial failed: %w; release uncertain call: %v", err, cleanupErr)
		}
	}
	return err
}

// DialWithID returns only the leg created by this request, including an
// uncertain AT result. A failed busy dial must never borrow another call's id.
func (a *ActiveCallAdapter) DialWithID(ctx context.Context, peer string) (CallID, error) {
	callID, err := a.Control.Dial(ctx, peer)
	if callID == "" {
		return "", err
	}
	a.mu.Lock()
	if a.lastPhysical != callID {
		a.active = callID
		a.lastPhysical = ""
		a.lastLogical = ""
	}
	a.mu.Unlock()
	return callID, err
}

func (a *ActiveCallAdapter) Answer(ctx context.Context) error {
	return a.withActive(func(callID CallID) error { return a.Control.Answer(ctx, callID) })
}

func (a *ActiveCallAdapter) Hangup(ctx context.Context) error {
	a.mu.Lock()
	callID := a.active
	a.mu.Unlock()
	return a.HangupCall(ctx, callID)
}

func (a *ActiveCallAdapter) HangupCall(ctx context.Context, callID CallID) error {
	a.mu.Lock()
	active, logical := a.active, a.logical
	a.mu.Unlock()
	if callID == "" || callID != active {
		return ErrNoActiveCall
	}
	err := a.Control.Hangup(ctx, callID)
	if err == nil {
		a.mu.Lock()
		// The terminal event may already have cleared active. Preserve its
		// mapping, and never clear a new call which arrived during ATH.
		if a.active == callID {
			a.lastPhysical, a.lastLogical = callID, logical
			a.active, a.logical = "", ""
		}
		a.mu.Unlock()
	}
	return err
}

func (a *ActiveCallAdapter) DTMF(ctx context.Context, digit rune) error {
	return a.withActive(func(callID CallID) error { return a.Control.SendDTMF(ctx, callID, digit) })
}

// WaitActive blocks until the outgoing cellular call is answered. Kept for
// the dial path; inbound calls need WaitActiveDir.
func (a *ActiveCallAdapter) WaitActive(ctx context.Context) (bool, error) {
	return a.WaitActiveDir(ctx, CLCCDirOutgoing)
}

// WaitActiveDir blocks until a cellular call in the given direction is
// answered. dir follows +CLCC's <dir> field (0 outgoing, 1 incoming,
// negative = either). Backends without CLCC support report "answered"
// immediately, matching the previous behaviour.
func (a *ActiveCallAdapter) WaitActiveDir(ctx context.Context, dir int) (bool, error) {
	if waiter, ok := a.Control.(interface {
		WaitActiveDir(context.Context, int) (bool, error)
	}); ok {
		return waiter.WaitActiveDir(ctx, dir)
	}
	if waiter, ok := a.Control.(interface {
		WaitActive(context.Context) (bool, error)
	}); ok {
		return waiter.WaitActive(ctx)
	}
	// Backend without CLCC support: treat as immediately answered.
	return true, nil
}

func (a *ActiveCallAdapter) withActive(action func(CallID) error) error {
	a.mu.Lock()
	callID := a.active
	a.mu.Unlock()
	if callID == "" {
		return ErrNoActiveCall
	}
	return action(callID)
}

func (a *ActiveCallAdapter) Events() <-chan ModemEvent { return a.events }

func (a *ActiveCallAdapter) CallerNumber(ctx context.Context) (string, error) {
	if reader, ok := a.Control.(interface {
		CallerNumber(context.Context) (string, error)
	}); ok {
		return reader.CallerNumber(ctx)
	}
	return "", nil
}

// PhysicalCallID returns the modem-internal id of the call currently in
// progress (empty when no call is up). The SIP layer records it when it sets
// a leg up, so a later modem "ended" event can be matched back to the SIP
// dialog it belongs to — the modem names calls by this id, while an outbound
// SIP session is keyed by the client's own Call-ID.
func (a *ActiveCallAdapter) PhysicalCallID() CallID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

func (a *ActiveCallAdapter) Close() error {
	var err error
	a.close.Do(func() { err = a.Control.Close() })
	return err
}

func (a *ActiveCallAdapter) forwardEvents() {
	defer close(a.events)
	for event := range a.Control.Events() {
		a.mu.Lock()
		physicalID := event.CallID
		event.PhysicalCallID = physicalID
		logicalID := a.logical
		if physicalID == a.lastPhysical && a.lastLogical != "" {
			logicalID = a.lastLogical
		}
		if (event.Kind == "incoming" || event.Kind == "outgoing") && physicalID != "" && a.active == "" {
			a.active = physicalID
			a.lastPhysical, a.lastLogical = "", ""
		}
		if physicalID != "" && (physicalID == a.active || physicalID == a.lastPhysical) && logicalID != "" {
			event.CallID = logicalID
		}
		if event.Kind == "ended" && physicalID == a.active {
			a.lastPhysical, a.lastLogical = physicalID, a.logical
			a.active = ""
			a.logical = ""
		}
		a.mu.Unlock()
		select {
		case a.events <- event:
		default:
		}
	}
}

var ErrNoActiveCall = errorString("no active modem call")

type errorString string

func (e errorString) Error() string { return string(e) }
