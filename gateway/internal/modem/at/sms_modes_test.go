package at

import (
	"context"
	"strings"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/sms"
)

func TestShortCodeChineseUsesPDU(t *testing.T) {
	p := &scriptedPort{}
	a := NewAdapter(NewClient(p))
	parts, _ := sms.EncodeSubmitSegments("10010", "中文短信", 1)
	if _, err := a.SendSMS(context.Background(), "10010", modem.SMSPayload{Body: parts[0].Body, PDU: parts[0].PDU}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.writes, "")
	if !strings.Contains(got, "AT+CMGF=0\rAT+CMGS=") || !strings.Contains(got, parts[0].PDU+"\x1a") || strings.Contains(got, "AT+CMGF=1") {
		t.Fatalf("wrong SMS commands: %q", got)
	}
}

type inboxFixturePort struct{ scriptedPort }

func (p *inboxFixturePort) Write(b []byte) (int, error) {
	if strings.HasPrefix(string(b), "AT+CMGL=") {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.writes = append(p.writes, string(b))
		p.read.WriteString("+CMGL: 1,2,,3\r\n000101\r\n+CMGL: 2,0,,3\r\n000102\r\n+CMGL: 3,3,,3\r\n000103\r\n+CMGL: 4,1,,3\r\n000104\r\nOK\r\n")
		return len(b), nil
	}
	return p.scriptedPort.Write(b)
}
func TestInboxSkipsStoredDraftsAndSentSMS(t *testing.T) {
	p := &inboxFixturePort{}
	got, _, err := NewAdapter(NewClient(p)).ListSMS(context.Background(), "")
	if err != nil || len(got) != 2 || got[0].ModemIndex != 2 || got[1].ModemIndex != 4 || got[0].ModemStorage != "MT" {
		t.Fatalf("inbox: %#v %v", got, err)
	}
}
