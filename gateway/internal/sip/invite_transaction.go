package sip

import (
	"crypto/sha256"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Retain final replies after session cleanup. A delayed copy of an already
// completed INVITE must never dial the SIM again. Exact bytes and source are
// required; this cache cannot authorize a modified request.
type inviteFinal struct {
	response string
	at       time.Time
}

func inviteRequestKey(req string, remote *net.UDPAddr) [32]byte {
	return sha256.Sum256([]byte(remote.String() + "\x00" + req))
}

func (s *Server) rememberInviteFinal(req string, remote *net.UDPAddr, response string) {
	s.inviteFinalMu.Lock()
	defer s.inviteFinalMu.Unlock()
	if s.inviteFinals == nil {
		s.inviteFinals = make(map[[32]byte]inviteFinal)
	}
	key := inviteRequestKey(req, remote)
	if _, exists := s.inviteFinals[key]; exists {
		return // Retransmissions do not extend the lifetime or change the final.
	}
	var oldest [32]byte
	var oldestAt time.Time
	for k, final := range s.inviteFinals {
		if time.Since(final.at) >= 32*time.Second {
			delete(s.inviteFinals, k)
			continue
		}
		if oldestAt.IsZero() || final.at.Before(oldestAt) {
			oldest, oldestAt = k, final.at
		}
	}
	if len(s.inviteFinals) >= 256 {
		delete(s.inviteFinals, oldest)
	}
	s.inviteFinals[key] = inviteFinal{response: response, at: time.Now()}
}

func (s *Server) resendInviteFinal(req string, remote *net.UDPAddr) bool {
	key := inviteRequestKey(req, remote)
	s.inviteFinalMu.Lock()
	final, exists := s.inviteFinals[key]
	if exists && time.Since(final.at) >= 32*time.Second {
		delete(s.inviteFinals, key)
		exists = false
	}
	s.inviteFinalMu.Unlock()
	if exists {
		_, _ = s.conn.WriteToUDP([]byte(final.response), remote)
	}
	return exists
}

// RFC 3261 §13.3.1.4: retransmit the same 2xx from T1 up to T2 until ACK;
// after 64*T1, terminate an unconfirmed call rather than orphaning the SIM.
// t1 is passed explicitly to allow fast deterministic lifecycle tests.
func (s *Server) answerOutbound(sess *SIPCallSession, remote *net.UDPAddr, req, headers, body string, t1 time.Duration) {
	sess.mu.Lock()
	if sess.ctx.Err() != nil || sess.answerSent {
		sess.mu.Unlock()
		return
	}
	sess.answerSent = true
	sess.mu.Unlock()
	response := buildResponse(req, 200, "OK", headers, body, sess.ToTag())
	s.rememberInviteFinal(req, remote, response)
	_, _ = s.conn.WriteToUDP([]byte(response), remote)
	deadline := time.NewTimer(64 * t1)
	defer deadline.Stop()
	delay := t1
	retry := time.NewTimer(delay)
	defer retry.Stop()
	for {
		select {
		case <-sess.ctx.Done():
			return
		case <-sess.answerACK:
			return
		case <-retry.C:
			if sess.ctx.Err() != nil {
				return
			}
			_, _ = s.conn.WriteToUDP([]byte(response), remote)
			delay = min(delay*2, 8*t1)
			retry.Reset(delay)
		case <-deadline.C:
			// ACK and timeout contend under the same lock: a timely ACK
			// cannot be overtaken by a ready timer in the select above.
			sess.mu.Lock()
			if sess.answerAcked || sess.ctx.Err() != nil {
				sess.mu.Unlock()
				return
			}
			sess.cancel()
			sess.mu.Unlock()
			slog.Warn("sip answer ACK timeout; releasing cellular call", "call", sess.ID)
			s.sendDialogTeardown(sess, "answer ACK timeout")
			if err := sess.Hangup(); err == nil {
				s.sessions.CompareAndDelete(sess.ID, sess)
			}
			return
		}
	}
}

func sipHeaderTag(header string) string {
	// Ignore URI parameters inside <...>.
	if end := strings.LastIndex(header, ">"); end >= 0 {
		header = header[end+1:]
	}
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(name, "tag") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *Server) handleInviteACK(msg string, remote *net.UDPAddr) {
	v, exists := s.sessions.Load(parseHeader(msg, "Call-ID"))
	if !exists {
		return
	}
	sess := v.(*SIPCallSession)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	plan := sess.bye
	cseq := strings.Fields(parseHeader(msg, "CSeq"))
	if sess.Direction != "outbound" || !sess.answerSent || sess.answerAcked || sess.ctx.Err() != nil ||
		plan.remote == nil || remote == nil || !plan.remote.IP.Equal(remote.IP) ||
		len(cseq) != 2 || cseq[1] != "ACK" || cseqNumber(cseq[0]) != plan.inviteCSeq ||
		sess.localTag == "" || sipHeaderTag(parseHeader(msg, "To")) != sess.localTag ||
		sipHeaderTag(parseHeader(msg, "From")) != sipHeaderTag(plan.to) {
		return
	}
	sess.answerAcked = true
	close(sess.answerACK)
	slog.Info("sip answer acknowledged", "call", sess.ID)
}
