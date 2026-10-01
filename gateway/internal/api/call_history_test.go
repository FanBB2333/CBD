package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/call"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestSIPCallHistoryLifecycle(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "calls.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewServer("test", "test", "test")
	s.Database, s.CallControl = database, call.NewController()
	for _, tc := range []struct {
		id, kind, reason string
		connected        bool
	}{
		{"in-answered", "incoming", "local_hangup", true},
		{"in-missed", "incoming", "NO CARRIER", false},
		{"out-answered", "outgoing", "NO CARRIER", true},
		{"out-failed", "outgoing", "dial_failed", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			id := modem.CallID(tc.id)
			s.HandleModemEvent(modem.ModemEvent{Kind: tc.kind, CallID: id})
			// RING may arrive before the caller number, and repeat afterward.
			s.HandleModemEvent(modem.ModemEvent{Kind: "caller_id", CallID: id, Peer: "+8613800138000"})
			s.HandleModemEvent(modem.ModemEvent{Kind: tc.kind, CallID: id, Peer: "unknown"})
			var connectedAt int64
			if tc.connected {
				s.HandleModemEvent(modem.ModemEvent{Kind: "connected", CallID: id})
				row, err := database.GetCall(context.Background(), tc.id)
				if err != nil || row.State != "active" || row.ConnectedAt == nil {
					t.Fatalf("connected row: %+v %v", row, err)
				}
				connectedAt = row.ConnectedAt.UnixMilli()
				s.HandleModemEvent(modem.ModemEvent{Kind: "connected", CallID: id})
			}
			stale := *s.CallControl.Snapshot()
			s.HandleModemEvent(modem.ModemEvent{Kind: "ended", CallID: id, Raw: tc.reason})
			if _, err := s.persistCall(context.Background(), stale, call.Event{Kind: "call.updated"}); err != nil {
				t.Fatal(err)
			}
			// Late URCs/media callbacks must not reopen or overwrite history.
			for _, kind := range []string{tc.kind, "connected", "caller_id", "ended"} {
				s.HandleModemEvent(modem.ModemEvent{Kind: kind, CallID: id, Peer: "wrong", Raw: "late"})
			}
			row, err := database.GetCall(context.Background(), tc.id)
			if err != nil || row.State != "ended" || row.EndedAt == nil || row.Peer != "+8613800138000" || row.EndReason != tc.reason {
				t.Fatalf("ended row: %+v %v", row, err)
			}
			if tc.connected && (row.ConnectedAt == nil || row.ConnectedAt.UnixMilli() != connectedAt || row.EndedAt.Before(*row.ConnectedAt)) {
				t.Fatalf("lost answer/duration: %+v", row)
			}
			if !tc.connected && row.ConnectedAt != nil {
				t.Fatal("unanswered call acquired answer time")
			}
			if s.CallControl.Snapshot() != nil || s.Line.ActiveCallID != nil {
				t.Fatal("completed call still owns line")
			}
		})
	}
	rows, err := database.ListCalls(context.Background(), 20)
	if err != nil || len(rows) != 4 {
		t.Fatalf("history = %d rows, %v", len(rows), err)
	}
}
