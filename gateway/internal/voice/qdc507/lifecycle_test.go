package qdc507

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type fixtureAudio struct {
	modem.VoiceAudio
	startErr      error
	starts, stops int
}

func (h *fixtureAudio) Start(context.Context, modem.CallID) error { h.starts++; return h.startErr }
func (h *fixtureAudio) Stop(context.Context) error                { h.stops++; return nil }

type fixtureADB struct {
	launches int
	alive    bool
}

func (r *fixtureADB) Run(_ context.Context, args ...string) (string, error) {
	if len(args) == 2 && args[0] == "devices" {
		return "fixture device usb:2-1 transport_id:7", nil
	}
	if len(args) < 4 || args[0] != "-t" || args[1] != "7" {
		return "", fmt.Errorf("unbound ADB command: %v", args)
	}
	command := args[len(args)-1]
	switch command {
	case "id -u":
		return "0", nil
	case "uname -r":
		return expectedKernel, nil
	case "cat /proc/asound/cards":
		return expectedCardName, nil
	case "sh /data/celldock-route-launch.sh":
		r.launches++
		r.alive = true
	case routeAliveScript():
		if r.alive {
			return "alive", nil
		}
		return "", errors.New("module rebooted")
	case routeReadyScript():
		return "ready", nil
	case "pgrep -f [m]avo-pcm-bridge.armv7 | wc -l":
		return "0", nil
	}
	if strings.HasSuffix(command, "&& echo ready") {
		return "ready", nil
	}
	return "", nil
}

func TestQDC507RouteLifecycle(t *testing.T) {
	// Exercise the complete bootstrap protocol with fake artifacts and ADB,
	// including the same-call liveness check and immediate consecutive calls.
	original := trustedRuntimeArtifacts
	trustedRuntimeArtifacts = append([]runtimeArtifact(nil), original...)
	t.Cleanup(func() { trustedRuntimeArtifacts = original })
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o750)
	for i := range trustedRuntimeArtifacts {
		artifact := &trustedRuntimeArtifacts[i]
		data := []byte("fixture " + artifact.name)
		artifact.hash = fmt.Sprintf("%x", sha256.Sum256(data))
		if err := os.WriteFile(filepath.Join(dir, artifact.name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := &fixtureAudio{}
	runner := &fixtureADB{}
	a, err := OpenWithRunner(host, Config{TTY: "/dev/ttyUSB2", RuntimeDir: dir, Bootstrap: true}, runner)
	if err != nil {
		t.Fatal(err)
	}
	a.usbPath = func(string) (string, error) { return "2-1", nil }
	ctx := context.Background()
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if runner.launches != 1 {
		t.Fatalf("same call rotated %d times", runner.launches)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if runner.launches != 2 {
		t.Fatal("immediate second call reused stale UAC route")
	}
	_ = a.Stop(ctx)
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	runner.alive = false // module restarts between route preparation and capture
	if err := a.Start(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if runner.launches != 4 {
		t.Fatal("module restart did not rebuild route")
	}
	_ = a.Stop(ctx)
	host.startErr = errors.New("ALSA unavailable")
	if err := a.Start(ctx, "failed"); err == nil {
		t.Fatal("capture failure ignored")
	}
	host.startErr = nil
	if err := a.Start(ctx, "retry"); err != nil {
		t.Fatal(err)
	}
	if runner.launches != 6 {
		t.Fatal("failed capture left route reusable")
	}
	_ = a.Stop(ctx)
	// Cancellation before Start must not let the next preparation reuse an
	// old live mavo process. Also guard a new preparation if cleanup failed.
	before := runner.launches
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.PrepareRoute(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.launches != before+3 {
		t.Fatal("prepared-only call left a reusable route")
	}
	_ = a.Stop(ctx)
}

func TestADBRunnerKeepsHomeAndOverridesSharedSocket(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "adb")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$HOME\" \"$ADB_SERVER_SOCKET\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	homeBefore := os.Getenv("HOME")
	t.Setenv("ADB_SERVER_SOCKET", "tcp:localhost:5037")
	output, err := (commandRunner{path: stub}).Run(context.Background(), "devices", "-l")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{homeBefore, adbServerSocket, "-L", adbServerSocket, "devices", "-l"}, "\n")
	if output != want {
		t.Fatalf("ADB invocation = %q, want %q", output, want)
	}
}

func TestPinnedPTYUSBIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "1-2")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []struct {
		vid, pid string
		accepted bool
	}{
		{"2ca3", "4006", true}, {"2c7c", "0125", true}, {"18d1", "4ee7", false},
	} {
		if err := os.WriteFile(filepath.Join(dir, "idVendor"), []byte(identity.vid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "idProduct"), []byte(identity.pid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := verifyUSBIdentity(root, "1-2")
		if (err == nil) != identity.accepted {
			t.Fatalf("identity %+v: %v", identity, err)
		}
	}
	if err := verifyUSBIdentity(root, "2-1"); err == nil {
		t.Fatal("absent physical port accepted")
	}
}
