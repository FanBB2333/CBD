package sip

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func transactionFixture(t *testing.T) (*Server, *SIPCallSession, *net.UDPConn, *cancelControl) {
	t.Helper()
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	ctl := &cancelControl{events: make(chan modem.ModemEvent)}
	adapter := modem.NewActiveCallAdapter(ctl)
	t.Cleanup(func() { adapter.Close() })
	if err := adapter.Dial(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	s := NewServer("", nil, nil, adapter, nil)
	t.Cleanup(s.cancel)
	s.conn = listen()
	client := listen()
	plan := outboundPlan()
	plan.remote = client.LocalAddr().(*net.UDPAddr)
	sess := s.newCallSession(plan.callID, "fixture", "outbound", nil)
	sess.state = "active"
	sess.SetModemCallID("physical")
	sess.SetLocalTag("cbTag123")
	sess.SetByePlan(plan)
	s.sessions.Store(sess.ID, sess)
	t.Cleanup(func() { sess.Hangup() })
	return s, sess, client, ctl
}

func readTransactionReply(t *testing.T, client *net.UDPConn) string {
	t.Helper()
	client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func fixtureACK() string {
	msg := strings.Replace(clientInvite, "INVITE sip:", "ACK sip:", 1)
	msg = strings.Replace(msg, "CSeq: 1 INVITE", "CSeq: 1 ACK", 1)
	return strings.Replace(msg, "To: <sip:10010@100.64.0.1>", "To: <sip:10010@100.64.0.1>;tag=cbTag123", 1)
}

func TestAnswerRetransmitsUntilMatchingACK(t *testing.T) {
	s, sess, client, ctl := transactionFixture(t)
	remote := client.LocalAddr().(*net.UDPAddr)
	done := make(chan struct{})
	go func() {
		s.answerOutbound(sess, remote, clientInvite, "", "fixture SDP", 5*time.Millisecond)
		close(done)
	}()
	first := readTransactionReply(t, client)
	if !strings.HasPrefix(first, "SIP/2.0 200 OK") {
		t.Fatal(first)
	}
	// Drop first 200, then send unrelated ACKs. They must not suppress retries.
	ack := fixtureACK()
	for _, bad := range []string{
		strings.Replace(ack, "CSeq: 1 ACK", "CSeq: 2 ACK", 1),
		strings.Replace(ack, "cbTag123", "otherTag", 1),
		strings.Replace(ack, "cliTag99", "otherClient", 1),
	} {
		s.handleMessage(bad, remote)
	}
	s.handleMessage(ack, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: remote.Port})
	if again := readTransactionReply(t, client); again != first {
		t.Fatal("200 retransmission changed")
	}
	s.handleMessage(ack, remote)
	awaitCancelSignal(t, done)
	if sess.ctx.Err() != nil || ctl.hangups.Load() != 0 || !sess.answerAcked {
		t.Fatal("ACK released healthy call")
	}
	// Duplicated ACK is harmless.
	s.handleMessage(ack, remote)
}

func TestMissingACKReleasesLineAndDelayedInviteCannotRedial(t *testing.T) {
	s, sess, client, ctl := transactionFixture(t)
	remote := client.LocalAddr().(*net.UDPAddr)
	done := make(chan struct{})
	go func() { s.answerOutbound(sess, remote, clientInvite, "", "fixture SDP", time.Millisecond); close(done) }()
	first := readTransactionReply(t, client)
	sawBye := false
	for !sawBye {
		sawBye = strings.HasPrefix(readTransactionReply(t, client), "BYE ")
	}
	awaitCancelSignal(t, done)
	if ctl.hangups.Load() != 1 || s.modem.PhysicalCallID() != "" {
		t.Fatal("ACK timeout orphaned physical call")
	}
	if _, busy := s.sessions.Load(sess.ID); busy {
		t.Fatal("ACK timeout left line busy")
	}
	// No auth/registrar is attached: only the retained final response can
	// handle this request; reaching the dial path would fail this test.
	s.handleInvite(clientInvite, remote)
	for {
		reply := readTransactionReply(t, client)
		if strings.HasPrefix(reply, "SIP/2.0 200") {
			if reply != first {
				t.Fatal("completed transaction changed response")
			}
			break
		}
	}
	if ctl.dials.Load() != 1 {
		t.Fatal("old INVITE redialed modem")
	}
	if !s.claimSession("next-call", &SIPCallSession{ID: "next-call"}) {
		t.Fatal("next call still busy")
	}
}

func TestHangupStopsAnswerRetries(t *testing.T) {
	s, sess, client, ctl := transactionFixture(t)
	done := make(chan struct{})
	go func() {
		s.answerOutbound(sess, client.LocalAddr().(*net.UDPAddr), clientInvite, "", "", time.Millisecond)
		close(done)
	}()
	readTransactionReply(t, client)
	sess.Hangup()
	awaitCancelSignal(t, done)
	if ctl.hangups.Load() != 1 {
		t.Fatal("hangup was duplicated")
	}
}

func TestStaleNonceChallengeCanBeRetriedWithoutPasswordChange(t *testing.T) {
	s, _, client, _ := transactionFixture(t)
	s.auth = NewAuth("cellbridge")
	s.auth.AddUser("iphone", "test-password")
	remote := client.LocalAddr().(*net.UDPAddr)
	msg := signedFixture("REGISTER", "stale", "before-restart", "test-password", 1)
	if s.authorizeRequest(msg, remote, "iphone") {
		t.Fatal("stale nonce accepted")
	}
	response := readTransactionReply(t, client)
	fields, ok := parseDigest(parseHeader(response, "WWW-Authenticate"))
	if !ok || fields["stale"] != "true" || fields["nonce"] == "before-restart" {
		t.Fatal("missing fresh stale challenge")
	}
	if !s.authorizeRequest(signedFixture("REGISTER", "retry", fields["nonce"], "test-password", 1), remote, "iphone") {
		t.Fatal("refresh with original password failed")
	}
}

type redialControl struct{ *cancelControl }

func (*redialControl) WaitActiveDir(context.Context, int) (bool, error) { return true, nil }
func (c *redialControl) Dial(context.Context, string) (modem.CallID, error) {
	return modem.CallID(fmt.Sprintf("physical-%d", c.dials.Add(1))), nil
}
func (c *redialControl) Hangup(_ context.Context, id modem.CallID) error {
	if id != modem.CallID(fmt.Sprintf("physical-%d", c.dials.Load())) {
		return fmt.Errorf("hangup did not target current physical leg")
	}
	c.hangups.Add(1)
	return nil
}

func TestConsecutiveAuthenticatedCallsWithReusedClientNonce(t *testing.T) {
	s, old, client, _ := transactionFixture(t)
	old.Hangup()
	s.sessions.Delete(old.ID)
	ctl := &redialControl{&cancelControl{events: make(chan modem.ModemEvent)}}
	adapter := modem.NewActiveCallAdapter(ctl)
	defer adapter.Close()
	s.modem, s.audio = adapter, &historyAudio{}
	s.auth, s.registrar = NewAuth("cellbridge"), NewRegistrar()
	s.auth.AddUser("iphone", "test-password")
	remote := client.LocalAddr().(*net.UDPAddr)
	s.registrar.Register("iphone", "<sip:iphone@"+remote.String()+">", "UDP", 300)
	for i := 0; i < 2; i++ {
		callID := fmt.Sprintf("redial-%d", i)
		unauthorized := "INVITE sip:localhost SIP/2.0\r\nFrom: <sip:iphone@localhost>;tag=client\r\nTo: <sip:fixture@localhost>\r\nCall-ID: " + callID + "\r\nCSeq: 1 INVITE\r\n\r\n"
		s.handleMessage(unauthorized, remote)
		challenge := readTransactionReply(t, client)
		fields, ok := parseDigest(parseHeader(challenge, "WWW-Authenticate"))
		if !ok {
			t.Fatal("missing authentication challenge")
		}
		// Each new call restarts nc at one and reuses cnonce=fixture.
		invite := signedFixture("INVITE", callID, fields["nonce"], "test-password", 1)
		s.handleMessage(invite, remote)
		var response string
		for {
			response = readTransactionReply(t, client)
			if strings.HasPrefix(response, "SIP/2.0 1") {
				continue
			}
			if !strings.HasPrefix(response, "SIP/2.0 200 OK") {
				t.Fatalf("call %d: %s", i, response)
			}
			break
		}
		dialog := func(method string, cseq int) string {
			return fmt.Sprintf("%s sip:cellbridge@localhost SIP/2.0\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nContent-Length: 0\r\n\r\n", method, parseHeader(response, "From"), parseHeader(response, "To"), callID, cseq, method)
		}
		s.handleMessage(dialog("ACK", 1), remote)
		s.handleMessage(dialog("BYE", 2), remote)
		for parseHeader(readTransactionReply(t, client), "CSeq") != "2 BYE" {
		}
		if adapter.PhysicalCallID() != "" {
			t.Fatal("hangup retained physical line")
		}
		// Late INVITE after BYE returns its final reply but never dials again.
		s.handleMessage(invite, remote)
		if readTransactionReply(t, client) != response {
			t.Fatal("late retransmission changed final")
		}
	}
	if ctl.dials.Load() != 2 || ctl.hangups.Load() != 2 {
		t.Fatalf("dials=%d hangups=%d", ctl.dials.Load(), ctl.hangups.Load())
	}
}
