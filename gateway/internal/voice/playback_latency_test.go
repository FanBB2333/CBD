package voice

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"
)

type pcmSink struct{ writes chan []byte }

func (w *pcmSink) Write(p []byte) (int, error) {
	w.writes <- append([]byte(nil), p...)
	return len(p), nil
}
func (*pcmSink) Close() error { return nil }

// Previously primeLoop checked a 15ms last-write threshold on its own 20ms
// clock. It could keep inserting zeros between perfectly paced speech frames.
func TestALSAPrimingHandsOffWithoutInsertingSilence(t *testing.T) {
	sink := &pcmSink{writes: make(chan []byte, 100)}
	stop := make(chan struct{})
	a := &ALSAAudio{call: "test", tx: sink, primeStop: stop}
	done := make(chan struct{})
	go func() { a.primeLoop("test", stop); close(done) }()
	t.Cleanup(func() { close(stop); <-done })
	select {
	case p := <-sink.writes:
		if len(p) != FrameSamples*2 || pcmBytePeak(p) != 0 {
			t.Fatal("idle priming did not supply one silence frame")
		}
	case <-time.After(time.Second):
		t.Fatal("idle capture was not primed")
	}
	for i := 1; i <= 8; i++ {
		frame := make([]int16, FrameSamples)
		for j := range frame {
			frame[j] = int16(i)
		}
		if n, err := a.WritePCM(frame); err != nil || n != FrameSamples {
			t.Fatalf("write %d: n=%d err=%v", i, n, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Even when the bridge pauses, ALSA must not resume a competing clock.
	time.Sleep(60 * time.Millisecond)
	next := 1
	for len(sink.writes) > 0 {
		p := <-sink.writes
		value := pcmBytePeak(p)
		if next == 1 && value == 0 {
			continue
		} // priming before ownership
		if value != next {
			t.Fatalf("inserted/reordered audio: got %d want %d", value, next)
		}
		next++
	}
	if next != 9 {
		t.Fatalf("speech frames lost: next=%d", next)
	}
}

func pcmBytePeak(p []byte) int {
	peak := 0
	for i := 0; i+1 < len(p); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(p[i:])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

func TestUpstreamBurstRecoversWithoutOneSecondBacklog(t *testing.T) {
	b := &Bridge{started: true, clientBuffering: true}
	for i := 0; i < 50; i++ {
		frame := make([]int16, FrameSamples)
		frame[0] = int16(i)
		b.enqueueClient(frame)
		frame[0] = -1 // enqueuing must copy the caller's storage
	}
	if len(b.clientQueue) > 6 || b.clientDropped == 0 {
		t.Fatalf("burst retained %d frames, dropped %d", len(b.clientQueue), b.clientDropped)
	}
	frame := b.nextClientFrame(time.Now())
	if frame == nil || frame[0] < 44 {
		t.Fatalf("replaying stale speech after burst: %v", frame)
	}
}

func TestUpstreamDropsSpeechThatAgedDuringDeviceStall(t *testing.T) {
	now := time.Now()
	b := &Bridge{started: true}
	b.clientQueue = []clientPCMFrame{
		{samples: []int16{1}, arrived: now.Add(-time.Second)},
		{samples: []int16{2}, arrived: now.Add(-200 * time.Millisecond)},
		{samples: []int16{3}, arrived: now.Add(-20 * time.Millisecond)},
	}
	if got := b.nextClientFrame(now); len(got) != 1 || got[0] != 3 || b.clientDropped != 2 {
		t.Fatalf("stale playback: got=%v dropped=%d", got, b.clientDropped)
	}
}

func TestUpstreamCushionAndUnderflow(t *testing.T) {
	now := time.Now()
	b := &Bridge{started: true, clientBuffering: true}
	b.clientQueue = []clientPCMFrame{{samples: []int16{1}, arrived: now}}
	if got := b.nextClientFrame(now.Add(20 * time.Millisecond)); got != nil {
		t.Fatal("did not allow a short jitter cushion")
	}
	if got := b.nextClientFrame(now.Add(40 * time.Millisecond)); len(got) != 1 || got[0] != 1 {
		t.Fatal("short packet burst waited indefinitely")
	}
	if got := b.nextClientFrame(now.Add(60 * time.Millisecond)); got != nil || b.clientUnderruns != 1 {
		t.Fatal("underflow should request silence and rebuild the cushion")
	}
}

type blockedPlaybackAudio struct {
	fakeAudio
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (a *blockedPlaybackAudio) WritePCM([]int16) (int, error) {
	close(a.entered)
	<-a.closed
	return 0, io.ErrClosedPipe
}
func (a *blockedPlaybackAudio) Stop(context.Context) error {
	a.once.Do(func() { close(a.closed) })
	return nil
}

func TestBlockedDeviceDoesNotBlockRTPOrHangup(t *testing.T) {
	a := &blockedPlaybackAudio{entered: make(chan struct{}), closed: make(chan struct{})}
	b := &Bridge{audio: a, started: true}
	defer a.Stop(context.Background())
	written := make(chan struct{})
	go func() { _ = b.writePCM(make([]int16, FrameSamples)); close(written) }()
	<-a.entered
	queued := make(chan struct{})
	go func() { b.enqueueClient(make([]int16, FrameSamples)); close(queued) }()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("blocked sound card blocked RTP reception")
	}
	stopped := make(chan struct{})
	go func() { _ = b.Stop(context.Background()); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("blocked sound card prevented hangup")
	}
	<-written
}
