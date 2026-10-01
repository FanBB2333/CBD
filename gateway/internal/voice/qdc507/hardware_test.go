package qdc507

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/voice"
)

// Explicit opt-in: installs the hash-pinned runtime into module RAM, rotates
// its audio route and tests idle USB PCM twice. It never dials or reads SMS.
// This does NOT certify that a real cellular call carries intelligible audio.
func TestHardwareQDC507USBPCM(t *testing.T) {
	if os.Getenv("HIL_QDC507") != "1" {
		t.Skip("requires an explicitly authorized QDC507 hardware run")
	}
	port, runtimeDir, alsa := os.Getenv("HIL_USB_PATH"), os.Getenv("HIL_RUNTIME_DIR"), os.Getenv("HIL_ALSA_DEVICE")
	if port == "" || runtimeDir == "" || alsa == "" {
		t.Fatal("HIL_USB_PATH, HIL_RUNTIME_DIR and HIL_ALSA_DEVICE are required")
	}
	host, err := voice.OpenALSA(alsa, alsa)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(host, Config{USBPath: port, RuntimeDir: runtimeDir, Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := a.stopRemoteRoute(ctx); err != nil {
			t.Errorf("stop hardware test route: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := a.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	// Reproduce a cancelled dial: prepare the module route, then release it
	// before host capture ever starts. The next call must rebuild it.
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	transport, err := a.transportID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.adb(ctx, transport, "shell", "test ! -s "+remoteRoutePIDFile); err != nil {
		t.Fatal("cancelled preparation left its route PID file")
	}
	t.Log("prepared-only route was released before opening host PCM")
	for pass := 1; pass <= 2; pass++ {
		if err := a.PrepareRoute(ctx); err != nil {
			t.Fatalf("pass %d prepare route: %v", pass, err)
		}
		if err := a.Start(ctx, modem.CallID(fmt.Sprintf("cbd-hil-%d", pass))); err != nil {
			t.Fatal(err)
		}
		type captureResult struct {
			samples int
			err     error
		}
		result := make(chan captureResult, 1)
		go func() {
			buffer := make([]int16, 160)
			total := 0
			for total < 8000 {
				n, err := a.ReadPCM(buffer)
				total += n
				if err != nil {
					result <- captureResult{total, err}
					return
				}
			}
			result <- captureResult{samples: total}
		}()
		select {
		case got := <-result:
			if got.err != nil || got.samples < 8000 {
				t.Fatalf("pass %d capture: samples=%d err=%v", pass, got.samples, got.err)
			}
			t.Logf("pass %d: %d idle PCM samples received; playback primed with silence", pass, got.samples)
		case <-time.After(12 * time.Second):
			_ = a.Stop(context.Background())
			t.Fatalf("pass %d: capture did not produce one second of PCM", pass)
		}
		if err := a.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
