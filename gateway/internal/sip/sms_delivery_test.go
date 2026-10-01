package sip

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestInboundSMSTransactionRetransmitsThenAcceptsClientResponse(t *testing.T) {
	gw, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	phone, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	s := NewServer("", NewRegistrar(), nil, nil, nil)
	s.conn = gw
	defer s.cancel()
	reg := &Registration{Username: "iphone", Contact: fmt.Sprintf("<sip:iphone@%s>;expires=300", phone.LocalAddr())}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { done <- s.deliverSMS(ctx, reg, "msg-fixture", "+8613800000000", "中文 hello") }()
	var first string
	for i := 0; i < 2; i++ {
		phone.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 4096)
		n, _, err := phone.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		msg := string(b[:n])
		if i == 0 {
			first = msg
		} else if msg != first {
			t.Fatal("retry changed transaction")
		}
	}
	if !strings.Contains(first, "Content-Length: 12\r\n") || !strings.HasSuffix(first, "中文 hello") || !strings.Contains(first, "sip:+8613800000000@") {
		t.Fatalf("bad MESSAGE: %q", first)
	}
	response := fmt.Sprintf("SIP/2.0 200 OK\r\nCall-ID: %s\r\nCSeq: 1 MESSAGE\r\n\r\n", parseHeader(first, "Call-ID"))
	s.handleResponse(response, phone.LocalAddr().(*net.UDPAddr))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
