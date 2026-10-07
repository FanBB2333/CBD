package sip

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPushOnlyForUnreachableRingingCall(t *testing.T) {
	for _, tc := range []struct {
		name             string
		code             int
		ended, wrongPeer bool
		want             bool
	}{
		{name: "suspended", want: true},
		{name: "trying does not mean ringing", code: 100, want: true},
		{name: "already ringing", code: 180},
		{name: "early media", code: 183},
		{name: "call ended", ended: true},
		{name: "unrelated response", code: 180, wrongPeer: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := NewSIPCallSession("in-push", "fixture", "inbound", nil, nil, nil)
			defer sess.EndedByModem()
			peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}
			sess.RememberInvite(inviteAttempt{addr: peer, req: "INVITE sip:iphone@host SIP/2.0\r\nCSeq: 1 INVITE\r\n\r\n"})
			sent := false
			s := &Server{pushSender: func(ctx context.Context, uri, kind, body string) error { sent = true; return nil }}
			s.sessions.Store(sess.ID, sess)
			if tc.code != 0 {
				remote := peer
				if tc.wrongPeer {
					remote = &net.UDPAddr{IP: peer.IP, Port: 5001}
				}
				s.handleResponse(fmt.Sprintf("SIP/2.0 %d Test\r\nCall-ID: in-push\r\nCSeq: 1 INVITE\r\n\r\n", tc.code), remote)
			}
			if tc.ended {
				sess.cancel()
			}
			s.pushIfUnreachable(sess, "sip:fixture@host", 5*time.Millisecond)
			if sent != tc.want {
				t.Fatalf("push sent=%v want=%v", sent, tc.want)
			}
		})
	}
}
