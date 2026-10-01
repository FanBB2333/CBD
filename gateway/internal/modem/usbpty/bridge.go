// Package usbpty owns the userspace USB AT bridge for hosts without a serial driver.
package usbpty

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

const ScriptPath = "/usr/local/lib/cellbridge/usb_at.py"

type Bridge struct {
	TTY    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error // written before done closes
}

func Start(ctx context.Context, usbPath string) (*Bridge, error) {
	return start(ctx, usbPath, 15*time.Second, "python3", ScriptPath, "--mode", "bridge", "--usb-path", usbPath)
}

func start(ctx context.Context, usbPath string, timeout time.Duration, command string, args ...string) (*Bridge, error) {
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, command, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start USB AT bridge: %w", err)
	}
	b := &Bridge{cancel: cancel, done: make(chan struct{})}
	type result struct {
		line []byte
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024), 4096)
		if scanner.Scan() {
			ready <- result{line: append([]byte(nil), scanner.Bytes()...)}
		} else {
			err := scanner.Err()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			ready <- result{err: err}
		}
		_, _ = io.Copy(io.Discard, stdout)
		b.err = cmd.Wait()
		close(b.done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var received result
	select {
	case received = <-ready:
	case <-timer.C:
		_ = b.Close()
		return nil, fmt.Errorf("USB AT bridge readiness timed out")
	case <-ctx.Done():
		_ = b.Close()
		return nil, ctx.Err()
	}
	var info struct {
		TTY     string `json:"tty"`
		USBPath string `json:"usb_path"`
	}
	if received.err != nil {
		err = received.err
	} else if err = json.Unmarshal(received.line, &info); err == nil {
		if info.USBPath != usbPath {
			err = fmt.Errorf("USB bridge reported an unexpected physical port")
		} else if stat, statErr := os.Stat(info.TTY); statErr != nil {
			err = statErr
		} else if stat.Mode()&os.ModeCharDevice == 0 {
			err = fmt.Errorf("USB bridge did not provide a character device")
		}
	}
	if err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("USB AT bridge readiness: %w", err)
	}
	b.TTY = info.TTY
	return b, nil
}

func (b *Bridge) Done() <-chan struct{} { return b.done }
func (b *Bridge) Err() error            { <-b.done; return b.err }
func (b *Bridge) Close() error          { b.cancel(); <-b.done; return b.err }
