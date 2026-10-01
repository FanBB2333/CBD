package qdc507

import (
	"context"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type fakeExchanger struct {
	responses map[string][]string
}

func (f fakeExchanger) Exchange(_ context.Context, command string) ([]string, error) {
	return f.responses[command], nil
}

func TestProbeCapabilitiesRecognizesQDC507VoiceGates(t *testing.T) {
	value, err := ProbeCapabilities(context.Background(), fakeExchanger{responses: map[string][]string{
		`AT+QCFG="ims"`:           {`+QCFG: "ims",1,1`, "OK"},
		`AT+QCFG="volte_disable"`: {`+QCFG: "volte/disable",0`, "OK"},
		"AT+VTS=?":                {"+VTS: (0-9,A-D,*,#),(0-255)", "OK"},
		"AT+QPCMV=?":              {"+QPCMV: (0,1),(0-2)", "OK"},
	}}, "Baiwang", "QDC507")
	if err != nil {
		t.Fatal(err)
	}
	if !value.Voice || !value.DTMF || !value.RequiresBootstrap || value.Tier != modem.CapabilityVoiceControlOnly {
		t.Fatalf("capabilities = %#v", value)
	}
	if value.Audio == nil || value.Audio.Backend != "uac-qdc507" {
		t.Fatalf("audio = %#v", value.Audio)
	}
}

func TestProbeCapabilitiesDoesNotPromoteGenericEC25(t *testing.T) {
	value, err := ProbeCapabilities(context.Background(), fakeExchanger{}, "Quectel", "EC25")
	if err != nil {
		t.Fatal(err)
	}
	if value.Voice || value.DTMF || value.Tier != modem.CapabilitySMSOnly {
		t.Fatalf("capabilities = %#v", value)
	}
}

func TestIMSConfigurationIsNotRegistration(t *testing.T) {
	for _, tc := range []struct {
		response string
		enabled  bool
		invalid  bool
	}{
		{`+QCFG: "ims",0,1`, true, false},
		{`+QCFG: "ims",1,1`, true, false},
		{`+QCFG: "ims",2,1`, false, false},
		{`+QCFG: "ims",0,0`, false, false},
		{`+QCFG: "ims",1,0`, false, false},
		{`+QCFG: "ims",3,1`, false, true},
		{`+QCFG: "ims",0,2`, false, true},
		{`+QCFG: "ims",x,1`, false, true},
		{`+QCFG: "ims",1`, false, true},
		{`+QCFG: "ims",0,1,1`, false, true},
	} {
		t.Run(tc.response, func(t *testing.T) {
			enabled, valid, err := queryIMS(context.Background(), fakeExchanger{responses: map[string][]string{
				`AT+QCFG="ims"`: {tc.response, "OK"},
			}})
			if enabled != tc.enabled || valid == tc.invalid || (err != nil) != tc.invalid {
				t.Fatalf("enabled=%v valid=%v err=%v", enabled, valid, err)
			}
		})
	}
}
