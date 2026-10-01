package sip

import (
	"crypto/md5"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRegistrationContactExpiry(t *testing.T) {
	for _, tc := range []struct {
		contact, header string
		want            int
		invalid         bool
	}{
		{`<sip:iphone@100.64.0.20:60331>;expires=300`, "", 300, false},
		{`<sip:iphone@host>;expires=300`, "3600", 300, false},
		{`<sip:iphone@host>;EXPIRES=0`, "3600", 0, false},
		{`<sip:iphone@host>`, "120", 120, false},
		{`<sip:iphone@host>`, "", 3600, false},
		{`<sip:expires=0@host>`, "300", 300, false},
		{`<sip:iphone@host;expires=0>`, "300", 300, false},
		{`<sip:iphone@host>;expires=0120`, "", 120, false},
		{`<sip:iphone@host>;expires=-1`, "", 0, true},
		{`<sip:iphone@host>;expires=300junk`, "", 0, true},
		{`<sip:iphone@host>;expires=`, "", 0, true},
		{`<sip:iphone@host>;expires=0;expires=300`, "", 0, true},
	} {
		got, err := registrationExpiry(tc.contact, tc.header)
		if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
			t.Errorf("expiry(%q,%q) = %d, %v", tc.contact, tc.header, got, err)
		}
	}
}

func TestAuthenticatedBindingQueryPreservesPhone(t *testing.T) {
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
	a := NewAuth("cellbridge")
	a.AddUser("iphone", "test-password")
	s := &Server{conn: conn, auth: a, registrar: NewRegistrar()}
	phone := `<sip:iphone@100.64.0.20:60331>;expires=300`
	s.registrar.Register("iphone", phone, "UDP", 300)
	before, _ := s.registrar.Get("iphone")
	h := func(v string) string { return fmt.Sprintf("%x", md5.Sum([]byte(v))) }
	nonce := a.Nonce()
	response := h(h("iphone:cellbridge:test-password") + ":" + nonce + ":00000001:fixture:auth:" + h("REGISTER:sip:localhost"))
	msg := fmt.Sprintf("REGISTER sip:localhost SIP/2.0\r\nFrom: <sip:iphone@localhost>;tag=test\r\nTo: <sip:iphone@localhost>\r\nCall-ID: query\r\nCSeq: 1 REGISTER\r\nAuthorization: Digest username=\"iphone\", realm=\"cellbridge\", nonce=\"%s\", uri=\"sip:localhost\", qop=auth, nc=00000001, cnonce=\"fixture\", response=\"%s\"\r\nContent-Length: 0\r\n\r\n", nonce, response)
	s.handleRegister(msg, client.LocalAddr().(*net.UDPAddr))
	client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "SIP/2.0 200") || !strings.Contains(string(buf[:n]), "Contact: <sip:iphone@100.64.0.20:60331>;expires=") {
		t.Fatalf("query response = %s %v", buf[:n], err)
	}
	after, ok := s.registrar.Get("iphone")
	if !ok || after.Contact != phone || !after.Expires.Equal(before.Expires) {
		t.Fatal("diagnostic query changed phone registration")
	}
}
