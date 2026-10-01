package qdc507

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	if root := os.Getenv("HIL_ALSA_PROC"); root != "" {
		testPacedUSBPlayback(t, ctx, a, root)
	}
}

// This uses locally generated PCM, never a SIP INVITE or cellular dial. It
// exercises the production bridge's priming handoff and bounded playout on
// real USB hardware, while reading the host's ALSA parameters and delay.
func testPacedUSBPlayback(t *testing.T, ctx context.Context, a *Audio, procRoot string) {
	t.Helper()
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	media := &hardwarePCMTransport{}
	bridge := voice.NewBridge(a, media)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer bridge.Stop(context.Background())
	if err := bridge.Start(ctx, "cbd-hil-paced"); err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, voice.FrameSamples)
	for i := range pcm {
		pcm[i] = int16((i%16 - 8) * 100)
	}
	frame, err := voice.EncodePCMU(pcm)
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	maxDelay := 0
	minDelay := 1 << 30
	for i := 0; i < 500; i++ {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
		media.receive(frame)
		if i == 50 {
			for _, stream := range []string{"pcm0c", "pcm0p"} {
				params, err := os.ReadFile(filepath.Join(procRoot, stream, "sub0/hw_params"))
				if err != nil {
					t.Fatal(err)
				}
				p := string(params)
				t.Logf("%s hardware: %s", stream, strings.ReplaceAll(strings.TrimSpace(p), "\n", "; "))
				if !strings.Contains(p, "rate: 8000") || !strings.Contains(p, "format: S16_LE") ||
					alsaField(p, "period_size") > 320 || alsaField(p, "buffer_size") > 1280 || alsaField(p, "buffer_size") < 1 {
					t.Fatalf("unexpected ALSA hardware parameters: %s", p)
				}
			}
		}
		if i >= 50 && i%5 == 0 {
			for _, stream := range []string{"pcm0c", "pcm0p"} {
				status, err := os.ReadFile(filepath.Join(procRoot, stream, "sub0/status"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(status), "state: RUNNING") {
					t.Fatalf("%s: %s", stream, status)
				}
				if stream == "pcm0p" {
					delay := alsaField(string(status), "delay")
					if delay < 0 || delay > 1600 {
						t.Fatalf("USB playback delay exceeds 200ms: %s", status)
					}
					if delay > maxDelay {
						maxDelay = delay
					}
					if delay < minDelay {
						minDelay = delay
					}
				}
			}
		}
	}
	media.mu.Lock()
	frames := media.sent
	media.mu.Unlock()
	if frames < 450 || frames > 550 {
		t.Fatalf("10s capture pacing drift: got %d frames", frames)
	}
	t.Logf("10s paced USB bridge: %d capture frames, playback kernel delay %.1f..%.1fms", frames, float64(minDelay)/8, float64(maxDelay)/8)
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func alsaField(text, field string) int {
	for _, line := range strings.Split(text, "\n") {
		name, value, ok := strings.Cut(line, ":")
		parts := strings.Fields(value)
		if ok && strings.TrimSpace(name) == field && len(parts) > 0 {
			if v, err := strconv.Atoi(parts[0]); err == nil {
				return v
			}
		}
	}
	return -1
}

func TestALSAStatusFieldSpacing(t *testing.T) {
	for _, text := range []string{"delay       : 360\n", "delay: 360\n"} {
		if got := alsaField(text, "delay"); got != 360 {
			t.Fatalf("%q parsed as %d", text, got)
		}
	}
}

type hardwarePCMTransport struct {
	mu       sync.Mutex
	callback func([]byte)
	sent     int
}

func (m *hardwarePCMTransport) OnPCMUFrame(fn func([]byte)) {
	m.mu.Lock()
	m.callback = fn
	m.mu.Unlock()
}
func (m *hardwarePCMTransport) WritePCMU([]byte) error {
	m.mu.Lock()
	m.sent++
	m.mu.Unlock()
	return nil
}
func (m *hardwarePCMTransport) receive(frame []byte) {
	m.mu.Lock()
	fn := m.callback
	m.mu.Unlock()
	if fn != nil {
		fn(frame)
	}
}
