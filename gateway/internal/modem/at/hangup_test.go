package at

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type hangupPort struct {
	scriptedPort
	voiceRemains bool
	releaseOnATH bool
	polls        int
}

func (p *hangupPort) Write(data []byte) (int, error) {
	if strings.TrimSpace(string(data)) == "ATH" && p.releaseOnATH {
		p.mu.Lock()
		p.voiceRemains = false
		p.mu.Unlock()
	}
	if strings.TrimSpace(string(data)) != "AT+CLCC" {
		return p.scriptedPort.Write(data)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polls++
	p.writes = append(p.writes, string(data))
	if p.voiceRemains {
		p.read.WriteString("\r\n+CLCC: 1,0,3,0,0,\"13800138000\",129\r\n")
	}
	p.read.WriteString("\r\nOK\r\n")
	return len(data), nil
}

func TestHangupRequiresVoiceLegToDisappear(t *testing.T) {
	for _, remains := range []bool{false, true} {
		p := &hangupPort{voiceRemains: remains}
		a := NewAdapter(NewClient(p))
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		id, err := a.Dial(ctx, "13800138000")
		if err != nil {
			t.Fatal(err)
		}
		<-a.Events()
		err = a.Hangup(ctx, id)
		if remains {
			if err == nil || a.validateActive(id) != nil {
				t.Fatalf("false successful hangup: %v", err)
			}
			select {
			case event := <-a.Events():
				t.Fatalf("false ended event: %+v", event)
			default:
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if event := <-a.Events(); event.Kind != "ended" {
				t.Fatalf("missing completion: %+v", event)
			}
		}
		if p.polls == 0 || !strings.Contains(strings.Join(p.writes, ""), "AT+CHUP") {
			t.Fatal("hangup was not sent and verified")
		}
		cancel()
		a.Close()
	}
}

func TestUncertainDialKeepsCallIDForCleanup(t *testing.T) {
	p := newBlockingPort()
	a := NewAdapter(NewClient(p))
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	id, err := a.Dial(ctx, "13800138000")
	if id == "" || !errors.Is(err, context.DeadlineExceeded) || a.validateActive(id) != nil {
		t.Fatalf("uncertain dial lost ownership: %q %v", id, err)
	}
}

func TestHangupRetriesAcknowledgedButIgnoredCHUP(t *testing.T) {
	p := &hangupPort{voiceRemains: true, releaseOnATH: true}
	a := NewAdapter(NewClient(p))
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id, err := a.Dial(ctx, "13800138000")
	if err != nil {
		t.Fatal(err)
	}
	<-a.Events()
	if err := a.Hangup(ctx, id); err != nil {
		t.Fatal(err)
	}
	writes := strings.Join(p.writes, "")
	if !strings.Contains(writes, "AT+CHUP") || !strings.Contains(writes, "ATH") || p.polls < 2 {
		t.Fatalf("no verified fallback: %q", writes)
	}
	if event := <-a.Events(); event.Kind != "ended" {
		t.Fatal("missing end")
	}
}
