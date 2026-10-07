package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/google/uuid"
)

type smsReceipt struct {
	remote *net.UDPAddr
	result chan int
}

func (s *Server) handleSMSResponse(callID string, code int, remote *net.UDPAddr) {
	if code < 200 {
		return
	}
	if v, ok := s.smsReceipts.Load(callID); ok {
		receipt := v.(*smsReceipt)
		if receipt.remote.String() != remote.String() {
			return
		}
		select {
		case receipt.result <- code:
		default:
		}
	}
}

// deliverSMS sends RFC 3428 MESSAGE to the registered client. Retries reuse
// the same transaction; a successful response means client acceptance, not
// that a person has read it. Offline messages remain in the NAS database.
func (s *Server) deliverSMS(ctx context.Context, reg *Registration, id, sender, body string) error {
	remote := reg.RemoteAddr()
	if remote == nil {
		return fmt.Errorf("invalid registered contact")
	}
	uri := reg.Contact
	start := strings.Index(uri, "sip:")
	if start < 0 {
		return fmt.Errorf("invalid registered URI")
	}
	uri = uri[start:]
	if end := strings.IndexAny(uri, ">;"); end >= 0 {
		uri = uri[:end]
	}
	local := net.JoinHostPort(s.localIPFor(remote), fmt.Sprint(s.conn.LocalAddr().(*net.UDPAddr).Port))
	callID := "sms-" + id + "-" + url.QueryEscape(reg.Username)
	// A fresh branch on a later retry permits an expired client transaction
	// to accept the message after the phone reconnects.
	branch := "z9hG4bK" + uuid.NewString()
	request := fmt.Sprintf("MESSAGE %s SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=%s;rport\r\nMax-Forwards: 70\r\nFrom: <sip:%s@%s>;tag=%s\r\nTo: <%s>\r\nCall-ID: %s\r\nCSeq: 1 MESSAGE\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Length: %d\r\n\r\n%s", uri, local, branch, url.PathEscape(sender), local, id, uri, callID, len(body), body)
	receipt := &smsReceipt{remote: remote, result: make(chan int, 1)}
	s.smsReceipts.Store(callID, receipt)
	defer s.smsReceipts.Delete(callID)
	for _, delay := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second} {
		if _, err := s.conn.WriteToUDP([]byte(request), remote); err != nil {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case code := <-receipt.result:
			timer.Stop()
			if code >= 200 && code < 300 {
				return nil
			}
			return fmt.Errorf("client returned SIP %d", code)
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("client did not acknowledge SMS")
}

// RunSMSDelivery also picks up messages received while the phone was offline.
// Receipt records are independent per authenticated registered SIP account.
func (s *Server) RunSMSDelivery(ctx context.Context, database *db.DB) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		for _, reg := range s.registrar.All() {
			device := "sip:" + reg.Username
			rows, err := database.QueryContext(ctx, `SELECT id,peer,body FROM messages m WHERE direction='inbound'
				AND NOT EXISTS (SELECT 1 FROM idempotency_records r WHERE r.device_id=? AND r.operation='sms.deliver' AND r.request_key=m.id AND r.status_code=200)
				ORDER BY created_at LIMIT 20`, device)
			if err != nil {
				slog.Warn("SIP SMS inbox lookup failed", "error", err)
				continue
			}
			type entry struct{ id, sender, body string }
			var entries []entry
			for rows.Next() {
				var item entry
				if err = rows.Scan(&item.id, &item.sender, &item.body); err != nil {
					break
				}
				entries = append(entries, item)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				slog.Warn("SIP SMS inbox read failed", "error", err)
				continue
			}
			for _, item := range entries {
				if err = s.deliverSMS(ctx, reg, item.id, item.sender, item.body); err != nil {
					slog.Warn("SIP SMS delivery deferred", "message_id", item.id, "error", err)
					break
				}
				_, _, err = database.ClaimIdempotency(ctx, device, "sms.deliver", item.id)
				if err == nil {
					err = database.CompleteIdempotency(ctx, device, "sms.deliver", item.id, 200, nil)
				}
				if err != nil {
					slog.Warn("SIP SMS receipt persistence failed", "message_id", item.id, "error", err)
					break
				}
				slog.Info("SIP SMS accepted by client", "message_id", item.id, "user", reg.Username)
			}
		}
	}
}
