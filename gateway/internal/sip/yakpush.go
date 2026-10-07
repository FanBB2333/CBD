package sip

import (
	"context"
	"log/slog"
	"time"
)

// A push while YakPhone already has a live ringing SIP call can create a
// second system-call UI. Answering it rejects the original INVITE with 486.
// Give the existing SIP flow a short chance to ring before waking the app.
func (s *Server) pushIfUnreachable(sess *SIPCallSession, callerURI string, grace time.Duration) {
	if s.pushSender == nil {
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-sess.ctx.Done():
		return
	case <-sess.ringing:
		slog.Info("yakpush unnecessary", "call", sess.ID, "reason", "SIP client already ringing")
		return
	case <-timer.C:
	}
	sess.mu.Lock()
	needed := !sess.ringingSeen && sess.state == "init" && sess.ctx.Err() == nil
	sess.mu.Unlock()
	if needed {
		s.sendYakPush(sess.ctx, callerURI, "voip", "")
	}
}

func (s *Server) sendYakPush(ctx context.Context, callerURI, pushType, messageBody string) {
	if s.pushSender == nil {
		return
	}
	if err := s.pushSender(ctx, callerURI, pushType, messageBody); err != nil && ctx.Err() == nil {
		slog.Warn("yakpush failed", "type", pushType, "error", err)
	}
}
