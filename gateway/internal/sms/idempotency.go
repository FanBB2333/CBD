package sms

import (
	"context"
	"fmt"
	"time"
)

// SendOnce reserves the authenticated SIP transaction before touching the
// modem. Retransmissions and process restarts cannot submit it twice.
// pending means accepted but not yet known to have completed; never resend it.
func (e *Engine) SendOnce(ctx context.Context, key, to, body string) (pending bool, err error) {
	claimed, record, err := e.Database.ClaimIdempotency(ctx, "sip", "sms.send", key)
	if err != nil {
		return false, err
	}
	if !claimed {
		if record.Pending {
			return true, nil
		}
		if record.StatusCode >= 400 {
			return false, fmt.Errorf("previous SMS submission failed")
		}
		return false, nil
	}
	_, sendErr := e.Send(ctx, to, body)
	code := 200
	if sendErr != nil {
		code = 500
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Database.CompleteIdempotency(finishCtx, "sip", "sms.send", key, code, nil); err != nil {
		return true, fmt.Errorf("record SIP SMS submission: %w", err)
	}
	return false, sendErr
}
