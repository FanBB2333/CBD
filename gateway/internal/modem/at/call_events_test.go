package at

import (
	"context"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestCallerIDEnrichesSameCallAndDoesNotLeakToNext(t *testing.T) {
	a := NewAdapter(nil)
	defer a.Close()
	a.HandleURC("RING")
	ring := <-a.Events()
	a.HandleURC(`+CLIP: "8613800138000",145,,,,0`)
	clip := <-a.Events()
	if clip.CallID != ring.CallID || clip.Peer != "+8613800138000" {
		t.Fatalf("CLIP = %+v", clip)
	}
	a.HandleURC("RING")
	if got := <-a.Events(); got.CallID != ring.CallID || got.Peer != clip.Peer {
		t.Fatalf("repeat = %+v", got)
	}
	a.HandleURC("NO CARRIER")
	<-a.Events()
	a.HandleURC("RING")
	if got := <-a.Events(); got.CallID == ring.CallID || got.Peer != "" {
		t.Fatalf("new call = %+v", got)
	}
	if !isUnsolicited(`+CLIP: "13800138000",129`) {
		t.Fatal("CLIP lost inside command exchange")
	}
}

func TestCLCCIgnoresQDC507DataLegs(t *testing.T) {
	lines := []string{`+CLCC: 1,1,0,1,0,"",128`, `+CLCC: 2,1,0,1,0,"",128`, `+CLCC: 4,1,4,0,0,"13800138000",128`}
	if clccActive(lines, modem.CLCCDirIncoming) {
		t.Fatal("data legs were mistaken for an answered voice call")
	}
	if peer, ok := clccVoice(lines, modem.CLCCDirIncoming, 4); !ok || peer != "13800138000" {
		t.Fatalf("ringing voice = %q, %v", peer, ok)
	}
	lines[2] = `+CLCC: 4,1,0,0,0,"8613800138000",145`
	if peer, ok := clccVoice(lines, modem.CLCCDirIncoming, 0); !ok || peer != "+8613800138000" {
		t.Fatalf("active voice = %q, %v", peer, ok)
	}
}

func TestDialEmitsHistoryBeforeTerminalURC(t *testing.T) {
	a := NewAdapter(NewClient(&scriptedPort{}))
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id, err := a.Dial(ctx, "13800138000")
	if err != nil {
		t.Fatal(err)
	}
	event := <-a.Events()
	if event.Kind != "outgoing" || event.CallID != id || event.Peer != "13800138000" {
		t.Fatalf("dial event = %+v", event)
	}
	if err := a.Hangup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if event := <-a.Events(); event.Kind != "ended" || event.CallID != id {
		t.Fatalf("end event = %+v", event)
	}
}
