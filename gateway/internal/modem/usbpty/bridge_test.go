package usbpty

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"testing"
	"time"
)

func TestUSBPTYHelperProcess(t *testing.T) {
	mode := os.Getenv("CBD_USBPTY_TEST_HELPER")
	if mode == "" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	switch mode {
	case "ready":
		fmt.Println(`{"tty":"/dev/null","usb_path":"1-2"}`)
	case "wrong-port":
		fmt.Println(`{"tty":"/dev/null","usb_path":"2-1"}`)
	case "failure":
		os.Exit(17)
	case "timeout": // deliberately never reports readiness
	}
	<-interrupt
	os.Exit(0)
}

func TestBridgeSupervision(t *testing.T) {
	for _, mode := range []string{"ready", "wrong-port", "failure", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CBD_USBPTY_TEST_HELPER", mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b, err := start(ctx, "1-2", time.Second, os.Args[0], "-test.run=^TestUSBPTYHelperProcess$")
			if mode != "ready" {
				if err == nil {
					_ = b.Close()
					t.Fatal("invalid bridge startup accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.TTY != "/dev/null" {
				t.Fatalf("unexpected TTY %s", b.TTY)
			}
			cancel()
			select {
			case <-b.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("bridge outlived gateway context")
			}
			_ = b.Close() // idempotent, including after parent cancellation
		})
	}
}
