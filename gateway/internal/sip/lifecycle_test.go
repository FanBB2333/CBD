package sip

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestRingTimeoutCannotEndAnsweredCall(t *testing.T) {
	for _, state := range []string{"answering", "active", "ended"} {
		sess := newTestSession("call", "inbound", state)
		defer sess.cancel()
		if sess.expireRinging() || sess.State() != state {
			t.Fatalf("ring timeout changed %s call", state)
		}
	}
}

func TestPickupAndRingTimeoutHaveOneWinner(t *testing.T) {
	for i := 0; i < 100; i++ {
		sess := newTestSession("call", "inbound", "init")
		var wins atomic.Int32
		var wg sync.WaitGroup
		for _, claim := range []func() bool{sess.beginAnswer, sess.expireRinging} {
			wg.Add(1)
			go func(f func() bool) {
				defer wg.Done()
				if f() {
					wins.Add(1)
				}
			}(claim)
		}
		wg.Wait()
		sess.cancel()
		if wins.Load() != 1 {
			t.Fatalf("pickup and timeout had %d winners", wins.Load())
		}
	}
}

func TestRepeatedAnswerStillReceivesACK(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := &Server{conn: serverConn}
	resp := "SIP/2.0 200 OK\r\nFrom: <sip:caller@localhost>;tag=a\r\nTo: <sip:phone@localhost>;tag=b\r\nCall-ID: repeated\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"
	for _, state := range []string{"answering", "active"} {
		sess := newTestSession("repeated", "inbound", state)
		defer sess.cancel()
		// No modem/audio attached: a duplicate must never touch either.
		s.acceptInbound(sess, resp, client.LocalAddr().(*net.UDPAddr))
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 2048)
		n, _, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("%s response did not receive ACK: %v", state, err)
		}
		if !strings.HasPrefix(string(buf[:n]), "ACK ") || !strings.Contains(string(buf[:n]), "CSeq: 1 ACK") {
			t.Fatalf("invalid ACK: %s", buf[:n])
		}
	}
}

type lifecycleControl struct {
	modem.ModemControl
	events    chan modem.ModemEvent
	answered  bool
	waitErr   error
	direction int
	hangups   int
}

func (c *lifecycleControl) Events() <-chan modem.ModemEvent { return c.events }
func (c *lifecycleControl) Close() error                    { close(c.events); return nil }
func (c *lifecycleControl) Dial(context.Context, string) (modem.CallID, error) {
	return "physical", nil
}
func (c *lifecycleControl) Answer(context.Context, modem.CallID) error { return nil }
func (c *lifecycleControl) Hangup(context.Context, modem.CallID) error { c.hangups++; return nil }
func (c *lifecycleControl) WaitActiveDir(_ context.Context, dir int) (bool, error) {
	c.direction = dir
	return c.answered, c.waitErr
}

type lifecycleAudio struct {
	modem.VoiceAudio
	prepared, started bool
	stops             int
}

func (a *lifecycleAudio) PrepareRoute(context.Context) error { a.prepared = true; return nil }
func (a *lifecycleAudio) Stop(context.Context) error         { a.stops++; return nil }
func (a *lifecycleAudio) Start(context.Context, modem.CallID) error {
	a.started = true
	return errors.New("unexpected capture start")
}

func TestInboundWaitsForConfirmedIncomingCall(t *testing.T) {
	for _, waitErr := range []error{nil, errors.New("modem disconnected")} {
		ctl := &lifecycleControl{events: make(chan modem.ModemEvent), waitErr: waitErr}
		adapter := modem.NewActiveCallAdapter(ctl)
		_ = adapter.Dial(context.Background(), "fixture")
		audio := &lifecycleAudio{}
		sess := NewSIPCallSession("in-physical", "fixture", "inbound", adapter, audio, nil)
		if err := sess.AnswerInbound(context.Background()); err == nil {
			t.Fatal("unconfirmed call accepted")
		}
		if ctl.direction != modem.CLCCDirIncoming || !audio.prepared || audio.started {
			t.Fatalf("direction=%d prepared=%v capture=%v", ctl.direction, audio.prepared, audio.started)
		}
		_ = sess.Hangup()
		_ = sess.Hangup()
		if ctl.hangups != 1 {
			t.Fatalf("duplicate hangup reached modem %d times", ctl.hangups)
		}
		if audio.stops != 1 {
			t.Fatal("unconfirmed inbound audio preparation was not released")
		}
		_ = adapter.Close()
	}
}

type delayedCallerControl struct {
	*lifecycleControl
	queryStarted, releaseQuery, hungup chan struct{}
}

func (c *delayedCallerControl) CallerNumber(ctx context.Context) (string, error) {
	close(c.queryStarted)
	select {
	case <-c.releaseQuery:
		return "13800138000", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (c *delayedCallerControl) Hangup(context.Context, modem.CallID) error {
	close(c.hungup)
	return nil
}

func TestEndDuringCallerLookupDoesNotLeavePhantomRing(t *testing.T) {
	ctl := &delayedCallerControl{lifecycleControl: &lifecycleControl{events: make(chan modem.ModemEvent)}, queryStarted: make(chan struct{}), releaseQuery: make(chan struct{}), hungup: make(chan struct{})}
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
	s := NewServer("127.0.0.1:0", NewRegistrar(), nil, a, nil)
	s.conn = conn
	defer s.cancel()
	events := make(chan modem.ModemEvent, 2)
	s.AttachEvents(events, nil)
	done := make(chan struct{})
	go func() { s.inboundLoop(); close(done) }()
	events <- modem.ModemEvent{Kind: "incoming", CallID: "physical"}
	select {
	case <-ctl.queryStarted:
	case <-time.After(time.Second):
		t.Fatal("lookup did not start")
	}
	events <- modem.ModemEvent{Kind: "ended", CallID: "physical", Raw: "NO CARRIER"}
	close(ctl.releaseQuery)
	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("end overtook caller lookup and left phantom ring")
	}
	if _, live := s.sessions.Load("in-physical"); live {
		t.Fatal("ended invitation retained")
	}
}

type failedHangupControl struct {
	lifecycleControl
	failure bool
}

func (c *failedHangupControl) Hangup(context.Context, modem.CallID) error {
	c.hangups++
	if c.failure {
		return context.DeadlineExceeded
	}
	return nil
}

func TestTerminalEventReleasesFailedHangupSession(t *testing.T) {
	ctl := &failedHangupControl{lifecycleControl: lifecycleControl{events: make(chan modem.ModemEvent)}, failure: true}
	a := modem.NewActiveCallAdapter(ctl)
	defer a.Close()
	if err := a.Dial(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	audio := &lifecycleAudio{}
	sess := NewSIPCallSession("sip-call", "fixture", "outbound", a, audio, nil)
	sess.SetModemCallID("physical")
	sess.routePrepared = true
	s := &Server{}
	s.sessions.Store(sess.ID, sess)
	if err := sess.Hangup(); err == nil || sess.State() != "hangup_failed" {
		t.Fatal("failed hangup falsely completed")
	}
	s.endCallByModem(modem.ModemEvent{Kind: "ended", CallID: "unrelated"})
	if _, ok := s.sessions.Load(sess.ID); !ok {
		t.Fatal("unrelated terminal event released call")
	}
	s.endCallByModem(modem.ModemEvent{Kind: "ended", CallID: "physical"})
	if _, ok := s.sessions.Load(sess.ID); ok || sess.State() != "ended" {
		t.Fatal("terminal event retained failed session")
	}
	if err := sess.Hangup(); err != nil {
		t.Fatal(err)
	}
	if ctl.hangups != 1 || audio.stops != 1 {
		t.Fatalf("repeated cellular/audio teardown: %d/%d", ctl.hangups, audio.stops)
	}
	if !s.claimSession("next", &SIPCallSession{ID: "next"}) {
		t.Fatal("next call still busy")
	}
}

func TestFailedHangupCanRetryWithoutRepeatingAudioCleanup(t *testing.T) {
	ctl := &failedHangupControl{lifecycleControl: lifecycleControl{events: make(chan modem.ModemEvent)}, failure: true}
	a := modem.NewActiveCallAdapter(ctl)
	defer a.Close()
	if err := a.Dial(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	audio := &lifecycleAudio{}
	sess := NewSIPCallSession("sip-call", "fixture", "outbound", a, audio, nil)
	sess.SetModemCallID("physical")
	sess.routePrepared = true
	if sess.Hangup() == nil {
		t.Fatal("expected failure")
	}
	ctl.failure = false
	if err := sess.Hangup(); err != nil || sess.State() != "ended" {
		t.Fatalf("retry failed: %v", err)
	}
	if ctl.hangups != 2 || audio.stops != 1 {
		t.Fatalf("hangups=%d audio stops=%d", ctl.hangups, audio.stops)
	}
}
