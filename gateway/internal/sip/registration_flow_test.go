package sip

import (
	"context"
	"crypto/md5"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestAuthenticatedRegisterRoutesInviteCancelAndSMSToReceivedFlow(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	phone, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	remote := phone.LocalAddr().(*net.UDPAddr)
	auth := NewAuth("cellbridge")
	auth.AddUser("iphone", "fixture")
	s := NewServer("", NewRegistrar(), auth, nil, nil)
	s.conn = conn
	defer s.cancel()
	h := func(v string) string { return fmt.Sprintf("%x", md5.Sum([]byte(v))) }
	nonce := auth.Nonce()
	digest := h(h("iphone:cellbridge:fixture") + ":" + nonce + ":00000001:fixture:auth:" + h("REGISTER:sip:localhost"))
	req := fmt.Sprintf("REGISTER sip:localhost SIP/2.0\r\nFrom: <sip:iphone@localhost>;tag=a\r\nTo: <sip:iphone@localhost>\r\nCall-ID: registration\r\nCSeq: 1 REGISTER\r\nContact: <sip:iphone@198.18.0.1:49938>;expires=300\r\nAuthorization: Digest username=\"iphone\", realm=\"cellbridge\", nonce=\"%s\", uri=\"sip:localhost\", qop=auth, nc=00000001, cnonce=\"fixture\", response=\"%s\"\r\nContent-Length: 0\r\n\r\n", nonce, digest)
	read := func(prefix string) string {
		t.Helper()
		phone.SetReadDeadline(time.Now().Add(time.Second))
		b := make([]byte, 8192)
		n, _, err := phone.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		msg := string(b[:n])
		if !strings.HasPrefix(msg, prefix) {
			t.Fatalf("want %s, got %s", prefix, msg)
		}
		return msg
	}
	s.handleRegister(req, remote)
	read("SIP/2.0 200")
	reg, ok := s.registrar.Get("iphone")
	if !ok || reg.RemoteAddr().String() != remote.String() || !strings.Contains(reg.Contact, "198.18.0.1") {
		t.Fatal("authenticated flow not preserved")
	}
	s.ringClients(modem.ModemEvent{Kind: "incoming", CallID: "physical", Peer: "13800138000"})
	invite := read("INVITE ")
	if !strings.Contains(invite, "c=IN IP4 127.0.0.1") {
		t.Fatal("SDP did not advertise listener address")
	}
	s.endCallByModem(modem.ModemEvent{Kind: "ended", CallID: "physical"})
	cancel := read("CANCEL ")
	if parseHeader(invite, "Via") != parseHeader(cancel, "Via") {
		t.Fatal("CANCEL changed transaction")
	}
	read("CANCEL ") // Teardown deliberately sends two identical UDP copies.
	done := make(chan error, 1)
	go func() { done <- s.deliverSMS(context.Background(), reg, "fixture", "10086", "fixture message") }()
	message := read("MESSAGE ")
	s.handleSMSResponse(parseHeader(message, "Call-ID"), 200, remote)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOldFlowUnregisterDoesNotRemoveReplacement(t *testing.T) {
	r := NewRegistrar()
	old := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10000}
	current := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10001}
	contact := "<sip:iphone@198.18.0.1:5060>"
	r.RegisterFrom("iphone", contact, "UDP", 300, old)
	r.RegisterFrom("iphone", contact, "UDP", 300, current)
	r.RegisterFrom("iphone", contact, "UDP", 0, old)
	if reg, ok := r.Get("iphone"); !ok || reg.RemoteAddr().String() != current.String() {
		t.Fatal("old transport deleted live registration")
	}
	r.RegisterFrom("iphone", contact, "UDP", 0, current)
	if _, ok := r.Get("iphone"); ok {
		t.Fatal("current transport could not unregister")
	}
}

func TestPushWakeReinvitesRefreshedSameFlow(t *testing.T) {
	phone, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := NewServer("", NewRegistrar(), nil, nil, nil)
	s.conn = conn
	defer s.cancel()
	remote := phone.LocalAddr().(*net.UDPAddr)
	s.registrar.RegisterFrom("iphone", "<sip:iphone@198.18.0.1:5060>", "UDP", 300, remote)
	s.ringClients(modem.ModemEvent{Kind: "incoming", CallID: "wake", Peer: "fixture"})
	v, _ := s.sessions.Load("in-wake")
	defer v.(*SIPCallSession).EndedByModem()
	read := func() string {
		t.Helper()
		phone.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 8192)
		n, _, err := phone.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		return string(b[:n])
	}
	first := read()
	s.registrar.RegisterFrom("iphone", "<sip:iphone@198.18.0.1:5060>", "UDP", 300, remote)
	second := read()
	if first != second {
		t.Fatal("push wake changed the INVITE transaction")
	}
	if len(v.(*SIPCallSession).InviteAttempts()) != 1 {
		t.Fatal("duplicate teardown target")
	}
}
