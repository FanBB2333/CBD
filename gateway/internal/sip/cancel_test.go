package sip

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type cancelControl struct {
	modem.ModemControl
	events                          chan modem.ModemEvent
	waiting, hanging, releaseHangup chan struct{}
	dialStarted, releaseDial        chan struct{}
	dials, hangups                  atomic.Int32
}

func (c *cancelControl) Events() <-chan modem.ModemEvent { return c.events }
func (c *cancelControl) Close() error                    { close(c.events); return nil }
func (c *cancelControl) WaitReady(context.Context) error { return nil }
func (c *cancelControl) Dial(ctx context.Context, _ string) (modem.CallID, error) {
	c.dials.Add(1)
	if c.dialStarted != nil {
		close(c.dialStarted)
		select {
		case <-c.releaseDial:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "physical", nil
}
func (c *cancelControl) WaitActiveDir(ctx context.Context, _ int) (bool, error) {
	close(c.waiting)
	<-ctx.Done()
	return false, ctx.Err()
}
func (c *cancelControl) Hangup(_ context.Context, id modem.CallID) error {
	if id != "physical" {
		return errors.New("wrong physical call")
	}
	c.hangups.Add(1)
	if c.hanging != nil {
		close(c.hanging)
		<-c.releaseHangup
	}
	return nil
}

func awaitCancelSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("cancel path blocked")
	}
}

func TestCancelAcknowledgedAndReleasesAnswerWaitBeforeHangupCompletes(t *testing.T) {
	ctl := &cancelControl{events: make(chan modem.ModemEvent), waiting: make(chan struct{}), hanging: make(chan struct{}), releaseHangup: make(chan struct{})}
	a := modem.NewActiveCallAdapter(ctl)
	defer a.Close()
	if err := a.Dial(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := NewServer("", nil, nil, a, nil)
	defer s.cancel()
	s.conn = conn
	sess := s.newCallSession("8f1a0b2c3d4e5f60", "fixture", "outbound", nil)
	sess.SetModemCallID("physical")
	sess.state = "dialing"
	sess.SetLocalTag("local")
	plan := outboundPlan()
	plan.remote = client.LocalAddr().(*net.UDPAddr)
	sess.SetByePlan(plan)
	s.sessions.Store(sess.ID, sess)
	waitDone := make(chan error, 1)
	go func() { waitDone <- sess.AwaitBridge(context.Background()) }()
	awaitCancelSignal(t, ctl.waiting)
	msg := strings.ReplaceAll(strings.ReplaceAll(clientInvite, "INVITE sip:", "CANCEL sip:"), "CSeq: 1 INVITE", "CSeq: 1 CANCEL")
	done := make(chan struct{})
	go func() {
		s.handleAckBye(msg, client.LocalAddr().(*net.UDPAddr), "CANCEL sip:fixture SIP/2.0")
		close(done)
	}()
	client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	responses := ""
	for i := 0; i < 2; i++ {
		n, _, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		responses += string(buf[:n])
	}
	if !strings.Contains(responses, "SIP/2.0 200 OK") || !strings.Contains(responses, "CSeq: 1 CANCEL") || !strings.Contains(responses, "SIP/2.0 487 Request Terminated") || !strings.Contains(responses, "CSeq: 1 INVITE") {
		t.Fatalf("cancel responses: %s", responses)
	}
	awaitCancelSignal(t, ctl.hanging) // WaitActive released its lock despite Background above.
	if err := <-waitDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait returned %v", err)
	}
	close(ctl.releaseHangup)
	awaitCancelSignal(t, done)
	if ctl.hangups.Load() != 1 || sess.State() != "ended" {
		t.Fatal("cellular hangup was not completed exactly once")
	}
	if _, ok := s.sessions.Load(sess.ID); ok {
		t.Fatal("cancelled session retained")
	}
}

type cancelPrepareAudio struct {
	modem.VoiceAudio
	prepared chan struct{}
	stops    atomic.Int32
}

func (a *cancelPrepareAudio) Stop(context.Context) error { a.stops.Add(1); return nil }

func (a *cancelPrepareAudio) PrepareRoute(ctx context.Context) error {
	close(a.prepared)
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelDuringPreparationNeverDials(t *testing.T) {
	ctl := &cancelControl{events: make(chan modem.ModemEvent)}
	a := modem.NewActiveCallAdapter(ctl)
	defer a.Close()
	audio := &cancelPrepareAudio{prepared: make(chan struct{})}
	sess := NewSIPCallSession("prepare", "fixture", "outbound", a, audio, nil)
	done := make(chan error, 1)
	go func() { done <- sess.Dial() }()
	awaitCancelSignal(t, audio.prepared)
	if err := sess.Hangup(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial returned %v", err)
	}
	if ctl.dials.Load() != 0 || ctl.hangups.Load() != 0 {
		t.Fatal("cancelled preparation reached cellular dialing")
	}
	if audio.stops.Load() != 1 {
		t.Fatal("cancelled audio preparation was not released")
	}
}

type preparedRouteAudio struct {
	modem.VoiceAudio
	prepared, stops int
}

func (a *preparedRouteAudio) PrepareRoute(context.Context) error { a.prepared++; return nil }
func (a *preparedRouteAudio) Stop(context.Context) error         { a.stops++; return nil }

func TestCancelAfterDialReleasesRouteWithoutStartingBridge(t *testing.T) {
	ctl := &cancelControl{events: make(chan modem.ModemEvent)}
	adapter := modem.NewActiveCallAdapter(ctl)
	defer adapter.Close()
	audio := &preparedRouteAudio{}
	sess := NewSIPCallSession("prepared", "fixture", "outbound", adapter, audio, nil)
	if err := sess.Dial(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Hangup(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Hangup(); err != nil {
		t.Fatal(err)
	}
	if audio.prepared != 1 || audio.stops != 1 {
		t.Fatalf("prepared=%d stops=%d", audio.prepared, audio.stops)
	}
}

func TestCancelDuringATDStillHangsUpOwnedCall(t *testing.T) {
	ctl := &cancelControl{events: make(chan modem.ModemEvent), dialStarted: make(chan struct{}), releaseDial: make(chan struct{})}
	a := modem.NewActiveCallAdapter(ctl)
	defer a.Close()
	sess := NewSIPCallSession("dial", "fixture", "outbound", a, nil, nil)
	dialDone := make(chan error, 1)
	go func() { dialDone <- sess.Dial() }()
	awaitCancelSignal(t, ctl.dialStarted)
	hangupDone := make(chan struct{})
	go func() { sess.Hangup(); close(hangupDone) }()
	awaitCancelSignal(t, sess.ctx.Done())
	close(ctl.releaseDial)
	awaitCancelSignal(t, hangupDone)
	if err := <-dialDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial returned %v", err)
	}
	if ctl.dials.Load() != 1 || ctl.hangups.Load() != 1 || a.PhysicalCallID() != "" {
		t.Fatal("accepted ATD orphaned after cancellation")
	}
}
