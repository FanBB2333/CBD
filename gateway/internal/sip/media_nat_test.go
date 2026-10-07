package sip

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestMediaReturnsAudioToObservedNATPort(t *testing.T) {
	media, err := NewMediaSession("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	advertised, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer advertised.Close()
	actual, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer actual.Close()
	if err := media.SetRemote(advertised.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 8)
	media.OnPCMUFrame(func([]byte) { received <- struct{}{} })
	for i := 0; i < 3; i++ {
		packet := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(i), Timestamp: uint32(i * 160), SSRC: 42}, Payload: bytes.Repeat([]byte{0xff}, 160)}
		raw, _ := packet.Marshal()
		if _, err := actual.WriteToUDP(raw, media.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("valid RTP not received")
		}
	}
	if media.RemoteAddr().String() != actual.LocalAddr().String() {
		t.Fatal("audio still addressed to SDP port instead of observed NAT port")
	}
	if err := media.WritePCMU(bytes.Repeat([]byte{0xab}, 160)); err != nil {
		t.Fatal(err)
	}
	actual.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := actual.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal("return audio did not reach NAT mapping:", err)
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(buffer[:n]); err != nil || !bytes.Equal(packet.Payload, bytes.Repeat([]byte{0xab}, 160)) {
		t.Fatal("return audio changed")
	}
}

func TestMediaLearningRejectsUnrelatedPacketsAndRequiresSequence(t *testing.T) {
	m, err := NewMediaSession("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	original := &net.UDPAddr{IP: net.ParseIP("100.64.0.2"), Port: 20000}
	mapped := &net.UDPAddr{IP: original.IP, Port: 40000}
	m.SetRemote(original.String())
	packet := func(seq uint16) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, SSRC: 42}, Payload: make([]byte, 160)}
	}
	for _, tc := range []struct {
		name string
		p    *rtp.Packet
		addr *net.UDPAddr
	}{
		{"other peer", packet(1), &net.UDPAddr{IP: net.ParseIP("100.64.0.3"), Port: 40000}},
		{"other codec", &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 8}, Payload: make([]byte, 160)}, mapped},
		{"invalid version", &rtp.Packet{Header: rtp.Header{Version: 1, PayloadType: 0}, Payload: make([]byte, 160)}, mapped},
		{"wrong frame size", &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0}, Payload: make([]byte, 4)}, mapped},
	} {
		if m.acceptPCMU(tc.p, tc.addr) {
			t.Fatal("accepted", tc.name)
		}
	}
	if rx, _ := m.Stats(); rx != 0 {
		t.Fatal("invalid packet counted as audio")
	}
	for _, seq := range []uint16{1, 1, 8, 9} {
		m.acceptPCMU(packet(seq), mapped)
	}
	if m.RemoteAddr().Port != original.Port {
		t.Fatal("stray or duplicate packets moved target")
	}
	m.acceptPCMU(packet(10), mapped)
	if m.RemoteAddr().Port != mapped.Port {
		t.Fatal("confirmed NAT mapping not learned")
	}
	// The phone can change its translated port again within the same call.
	rebound := &net.UDPAddr{IP: original.IP, Port: 41000}
	for _, seq := range []uint16{65535, 0, 1} {
		m.acceptPCMU(packet(seq), rebound)
	}
	if m.RemoteAddr().Port != rebound.Port {
		t.Fatal("rebinding/sequence wrap failed")
	}
	m.SetRemote(original.String())
	if m.RemoteAddr().Port != original.Port {
		t.Fatal("explicit new SDP did not reset learned mapping")
	}
}
