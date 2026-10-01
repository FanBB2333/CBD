package sms

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestSIPRetransmissionsAndRestartSendOnlyOnce(t *testing.T) {
	database := openTestDB(t)
	ctl := &fakeSMSModem{}
	e := NewEngine(database, ctl)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.SendOnce(context.Background(), "transaction", "10010", "中文测试"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e = NewEngine(database, ctl)
	if pending, err := e.SendOnce(context.Background(), "transaction", "10010", "中文测试"); err != nil || pending {
		t.Fatalf("replay: %v %v", pending, err)
	}
	if len(ctl.payloads) != 1 {
		t.Fatalf("sent %d copies", len(ctl.payloads))
	}
}

func TestIngestRecoversPersistedSegmentAfterMessageInsertFailure(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	if _, err := database.Exec(`CREATE TRIGGER fail_inbound BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	parts, _ := EncodeSubmitSegments("10010", "recover this message", 1)
	pdu, _ := hex.DecodeString(parts[0].PDU)
	raw := modem.RawSMS{RawPDU: pdu, ModemIndex: 2, ModemStorage: "MT"}
	if _, err := NewEngine(database, nil).Ingest(ctx, raw); err == nil {
		t.Fatal("expected interrupted insert")
	}
	if _, err := database.Exec("DROP TRIGGER fail_inbound"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(database, nil)
	got, err := e.Ingest(ctx, raw)
	if err != nil || got == nil || got.Body != "recover this message" {
		t.Fatalf("recovery: %#v %v", got, err)
	}
	if duplicate, err := e.Ingest(ctx, raw); err != nil || duplicate != nil {
		t.Fatalf("duplicate: %#v %v", duplicate, err)
	}
}
