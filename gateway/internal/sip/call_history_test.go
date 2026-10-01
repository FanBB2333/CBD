package sip

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/api"
	"github.com/cellbridge/cellbridge/gateway/internal/call"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type historyAudio struct{ modem.VoiceAudio }

func (*historyAudio) Start(context.Context, modem.CallID) error { return nil }
func (*historyAudio) ReadPCM([]int16) (int, error)              { return 0, io.EOF }
func (*historyAudio) WritePCM(p []int16) (int, error)           { return len(p), nil }
func (*historyAudio) Stop(context.Context) error                { return nil }

func TestSIPAnswerPersistsConnectedPhysicalCall(t *testing.T) {
	for _, direction := range []string{"inbound", "outbound"} {
		t.Run(direction, func(t *testing.T) {
			database, err := db.Open(filepath.Join(t.TempDir(), "calls.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			app := api.NewServer("test", "test", "test")
			app.Database, app.CallControl = database, call.NewController()
			ctl := &lifecycleControl{events: make(chan modem.ModemEvent), answered: true}
			adapter := modem.NewActiveCallAdapter(ctl)
			defer adapter.Close()
			if err := adapter.Dial(context.Background(), "fixture"); err != nil {
				t.Fatal(err)
			}
			media, err := NewMediaSession("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer("127.0.0.1:0", NewRegistrar(), nil, adapter, &historyAudio{})
			defer server.cancel()
			server.AttachCallEvents(app.HandleModemEvent)
			sess := server.newCallSession("sip-dialog-id", "13800138000", direction, media)
			sess.SetModemCallID("physical")
			defer sess.Hangup()
			// No HTTP answer/media action. The real SIP readiness path must
			// persist using the modem id, not create a second SIP-id record.
			if direction == "inbound" {
				err = sess.AnswerInbound(context.Background())
			} else {
				err = sess.AwaitBridge(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := database.ListCalls(context.Background(), 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows = %+v %v", rows, err)
			}
			row := rows[0]
			if row.ID != "physical" || row.State != "active" || row.ConnectedAt == nil || row.Direction != direction {
				t.Fatalf("connected row = %+v", row)
			}
			app.HandleModemEvent(modem.ModemEvent{Kind: "ended", CallID: "physical", Raw: "local_hangup"})
			row, err = database.GetCall(context.Background(), "physical")
			if err != nil || row.State != "ended" || row.EndedAt == nil || row.ConnectedAt == nil {
				t.Fatalf("ended row = %+v %v", row, err)
			}
		})
	}
}
