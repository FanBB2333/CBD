package sip

import (
	"net"
	"testing"
)

func TestRTPUsesSIPListenerInterface(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := &Server{conn: conn}
	media, err := s.newMediaSession()
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	if actual := media.LocalAddr(); !actual.IP.Equal(conn.LocalAddr().(*net.UDPAddr).IP) || actual.Port == 0 {
		t.Fatalf("RTP must bind to SIP's interface with an allocated port: %v", actual)
	}
	if _, err := (&Server{}).newMediaSession(); err == nil {
		t.Fatal("RTP opened without a SIP listener")
	}
}
