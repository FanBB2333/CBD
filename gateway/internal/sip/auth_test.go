package sip

import (
	"crypto/md5"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Independent RFC 2617 section 3.5 calculation, with SIP's request framing.
const digestVector = `Digest username="Mufasa", realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", uri="/dir/index.html", qop=auth, nc=00000001, cnonce="0a4f113b", response="6629fae49393a05397450978507c4ef1", algorithm=MD5`

func vectorAuth() *Auth {
	a := NewAuth("testrealm@host.com")
	a.nonces["dcd98b7102dd2f0e8b11d0f600bfb0c093"] = &digestNonce{created: time.Now(), clients: make(map[string]*digestCounter)}
	a.AddUser("Mufasa", "Circle Of Life")
	return a
}

func TestDigestVerifiesIndependentVectorAndRejectsChangedFields(t *testing.T) {
	msg := "GET /dir/index.html SIP/2.0\r\nAuthorization: " + digestVector + "\r\n\r\n"
	if !vectorAuth().Verify(msg, "Mufasa") {
		t.Fatal("RFC Digest vector was rejected")
	}
	for name, invalid := range map[string]string{
		"method":            strings.Replace(msg, "GET ", "REGISTER ", 1),
		"request URI":       strings.Replace(msg, "GET /dir/index.html", "GET /different", 1),
		"digest URI":        strings.Replace(msg, `uri="/dir/index.html"`, `uri="/different"`, 1),
		"password response": strings.Replace(msg, "6629fae49393a05397450978507c4ef1", strings.Repeat("0", 32), 1),
		"nonce":             strings.Replace(msg, `nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"`, `nonce="old"`, 1),
		"qop":               strings.Replace(msg, "qop=auth", "qop=auth-int", 1),
		"duplicate field":   strings.Replace(msg, "algorithm=MD5", `algorithm=MD5, username="Mufasa"`, 1),
		"duplicate header":  strings.Replace(msg, "\r\n\r\n", "\r\nAuthorization: "+digestVector+"\r\n\r\n", 1),
		"fake header":       "REGISTER sip:test SIP/2.0\r\nAuthorization: anything\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if vectorAuth().Verify(invalid, "Mufasa") {
				t.Fatal("invalid Digest accepted")
			}
		})
	}
	a := vectorAuth()
	a.AddUser("Mufasa", "wrong password")
	if a.Verify(msg, "Mufasa") || vectorAuth().Verify(msg, "other-user") {
		t.Fatal("wrong account accepted")
	}
}

func TestDigestRetransmissionReplayAndExpiry(t *testing.T) {
	a := vectorAuth()
	msg := "GET /dir/index.html SIP/2.0\r\nCall-ID: original\r\nAuthorization: " + digestVector + "\r\n\r\n"
	if !a.Verify(msg, "Mufasa") || !a.Verify(msg, "Mufasa") {
		t.Fatal("valid retransmission rejected")
	}
	if a.Verify(strings.Replace(msg, "original", "new-call", 1), "Mufasa") {
		t.Fatal("replayed Digest accepted for different transaction")
	}
	a.nonces["dcd98b7102dd2f0e8b11d0f600bfb0c093"].created = time.Now().Add(-6 * time.Minute)
	if a.Verify(msg, "Mufasa") {
		t.Fatal("expired nonce accepted")
	}
	if a.Nonce() == "dcd98b7102dd2f0e8b11d0f600bfb0c093" {
		t.Fatal("expired nonce did not rotate")
	}
}

func TestUnauthorizedRequestsNeverReachModemOrSMS(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := &Server{conn: conn, auth: NewAuth("cellbridge"), registrar: NewRegistrar()}
	s.auth.AddUser("iphone", "not-empty")
	for _, method := range []string{"REGISTER", "INVITE", "MESSAGE"} {
		msg := fmt.Sprintf("%s sip:iphone@localhost SIP/2.0\r\nFrom: <sip:iphone@localhost>;tag=test\r\nTo: <sip:iphone@localhost>\r\nCall-ID: test-%s\r\nCSeq: 1 %s\r\nAuthorization: garbage\r\nContent-Length: 0\r\n\r\n", method, method, method)
		switch method {
		case "REGISTER":
			s.handleRegister(msg, peer.LocalAddr().(*net.UDPAddr))
		case "INVITE":
			s.handleInvite(msg, peer.LocalAddr().(*net.UDPAddr))
		case "MESSAGE":
			s.handleMessageRequest(msg, peer.LocalAddr().(*net.UDPAddr))
		}
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 4096)
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil || !strings.HasPrefix(string(buf[:n]), "SIP/2.0 401 ") {
			t.Fatalf("%s response: %s %v", method, buf[:n], err)
		}
	}
	if len(s.registrar.All()) != 0 {
		t.Fatal("unauthenticated registration stored")
	}
}

func signedFixture(method, callID, nonce, password string, count int) string {
	h := func(v string) string { return fmt.Sprintf("%x", md5.Sum([]byte(v))) }
	nc := fmt.Sprintf("%08x", count)
	response := h(h("iphone:cellbridge:"+password) + ":" + nonce + ":" + nc + ":fixture:auth:" + h(method+":sip:localhost"))
	return fmt.Sprintf("%s sip:localhost SIP/2.0\r\nFrom: <sip:iphone@localhost>;tag=client\r\nTo: <sip:fixture@localhost>\r\nVia: SIP/2.0/UDP localhost;branch=z9hG4bK%s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nAuthorization: Digest username=\"iphone\", realm=\"cellbridge\", nonce=\"%s\", uri=\"sip:localhost\", qop=auth, nc=%s, cnonce=\"fixture\", response=\"%s\"\r\nContent-Length: 0\r\n\r\n", method, callID, callID, count, method, nonce, nc, response)
}

func TestDigestInterleavedRegisterAndInviteRetransmissions(t *testing.T) {
	a := NewAuth("cellbridge")
	a.AddUser("iphone", "test-password")
	nonce := a.Nonce()
	invite := signedFixture("INVITE", "call", nonce, "test-password", 1)
	register := signedFixture("REGISTER", "reg", nonce, "test-password", 3)
	outOfOrder := signedFixture("MESSAGE", "msg", nonce, "test-password", 2)
	for _, msg := range []string{invite, register, invite, outOfOrder, register, outOfOrder, invite} {
		if !a.Verify(msg, "iphone") {
			t.Fatal("concurrent request or UDP retransmission rejected")
		}
	}
	if a.Verify(strings.Replace(invite, "Call-ID: call", "Call-ID: replay", 1), "iphone") {
		t.Fatal("changed transaction reused captured authorization")
	}
	for n := 4; n < 100; n++ {
		if !a.Verify(signedFixture("REGISTER", "reg", nonce, "test-password", n), "iphone") {
			t.Fatal("new nonce count rejected")
		}
	}
	if a.Verify(invite, "iphone") || len(a.nonces[nonce].clients["iphone\x00fixture"].uses) > digestReplayWindow {
		t.Fatal("replay window is not bounded")
	}
}

func TestDigestFreshChallengesAndStaleRecovery(t *testing.T) {
	a := NewAuth("cellbridge")
	a.AddUser("iphone", "test-password")
	first, second := a.Nonce(), a.Nonce()
	if first == second {
		t.Fatal("new call challenge reused old nonce")
	}
	for _, nonce := range []string{first, second} {
		if !a.Verify(signedFixture("INVITE", nonce, nonce, "test-password", 1), "iphone") {
			t.Fatal("parallel challenge invalidated earlier challenge")
		}
	}
	a.nonces[first].created = time.Now().Add(-6 * time.Minute)
	for _, nonce := range []string{first, "before-server-restart"} {
		if ok, stale, _ := a.verify(signedFixture("INVITE", "old", nonce, "test-password", 1), "iphone"); ok || !stale {
			t.Fatal("correct credentials with stale nonce must receive refresh")
		}
		if ok, stale, _ := a.verify(signedFixture("INVITE", "bad", nonce, "wrong-password", 1), "iphone"); ok || stale {
			t.Fatal("wrong password must not be classified as nonce expiry")
		}
	}
	for i := 0; i < 2*maxDigestNonces; i++ {
		a.Nonce()
	}
	if len(a.nonces) > maxDigestNonces {
		t.Fatal("challenge state grew without bound")
	}
}
