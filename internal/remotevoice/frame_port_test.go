package remotevoice

import (
	"errors"
	"sync"
	"testing"
)

func filledFrame(value int16) []int16 {
	frame := make([]int16, FrameSamples)
	for i := range frame {
		frame[i] = value
	}
	return frame
}

func TestFramePortDropsOldestAndCountsStats(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := port.PushUplink(filledFrame(1)); err != nil {
		t.Fatal(err)
	}
	if err := port.PushUplink(filledFrame(2)); err != nil {
		t.Fatal(err)
	}
	if err := port.PushUplink(filledFrame(3)); err != nil {
		t.Fatal(err)
	}
	first, ok := port.takeUplink()
	if !ok || first[0] != 2 {
		t.Fatalf("oldest retained frame = %v, want value 2", first)
	}
	second, ok := port.takeUplink()
	if !ok || second[0] != 3 {
		t.Fatalf("newest frame = %v, want value 3", second)
	}
	if _, ok := port.takeUplink(); ok {
		t.Fatal("empty ring unexpectedly returned a frame")
	}
	stats := port.Stats()
	if stats.UplinkOverflow != 1 || stats.UplinkUnderrun != 1 {
		t.Fatalf("uplink stats = %+v, want overflow=1 underrun=1", stats)
	}

	if err := port.pushDownlink(filledFrame(4)); err != nil {
		t.Fatal(err)
	}
	downlink, ok := port.TakeDownlink()
	if !ok || downlink[0] != 4 {
		t.Fatalf("downlink = %v, want value 4", downlink)
	}
	if _, ok := port.TakeDownlink(); ok {
		t.Fatal("empty downlink unexpectedly returned a frame")
	}
	stats = port.Stats()
	if stats.DownlinkUnderrun != 1 {
		t.Fatalf("downlink stats = %+v, want underrun=1", stats)
	}
}

func TestFramePortConcurrentUseAndClose(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(8)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(value int16) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_ = port.PushUplink(filledFrame(value))
				_, _ = port.takeUplink()
				_ = port.pushDownlink(filledFrame(value))
				_, _ = port.TakeDownlink()
				_ = port.Stats()
			}
		}(int16(worker))
	}
	wg.Wait()
	port.Close()
	if err := port.PushUplink(filledFrame(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("PushUplink after close = %v, want ErrClosed", err)
	}
}

func TestFramePortRejectsInvalidFrames(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := port.PushUplink(make([]int16, FrameSamples-1)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("short frame error = %v, want ErrInvalidFrame", err)
	}
}

func TestFramePortFlushDropsQueuedFramesWithoutClosing(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := port.PushUplink(filledFrame(1)); err != nil {
		t.Fatal(err)
	}
	if err := port.pushDownlink(filledFrame(2)); err != nil {
		t.Fatal(err)
	}
	port.flush()
	if _, ok := port.takeUplink(); ok {
		t.Fatal("flushed uplink frame remained queued")
	}
	if _, ok := port.TakeDownlink(); ok {
		t.Fatal("flushed downlink frame remained queued")
	}
	if err := port.PushUplink(filledFrame(3)); err != nil {
		t.Fatalf("flush closed the port: %v", err)
	}
}

func TestFramePortActivationAtomicallyRejectsStaleProducersAndConsumers(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(4)
	if err != nil {
		t.Fatal(err)
	}
	if err := port.pushUplinkAtEpoch(filledFrame(1), 0); err != nil {
		t.Fatal(err)
	}
	if err := port.pushDownlinkAtEpoch(filledFrame(2), 0); err != nil {
		t.Fatal(err)
	}
	if err := port.activate(1); err != nil {
		t.Fatal(err)
	}
	if err := port.pushUplinkAtEpoch(filledFrame(3), 0); !errors.Is(err, ErrStaleMediaEpoch) {
		t.Fatalf("stale uplink producer error = %v", err)
	}
	if err := port.pushDownlinkAtEpoch(filledFrame(4), 0); !errors.Is(err, ErrStaleMediaEpoch) {
		t.Fatalf("stale downlink producer error = %v", err)
	}
	if _, _, err := port.takeUplinkAtEpoch(0); !errors.Is(err, ErrStaleMediaEpoch) {
		t.Fatalf("stale uplink consumer error = %v", err)
	}
	if _, _, err := port.takeDownlinkAtEpoch(0); !errors.Is(err, ErrStaleMediaEpoch) {
		t.Fatalf("stale downlink consumer error = %v", err)
	}
	if _, ok, err := port.takeUplinkAtEpoch(1); err != nil || ok {
		t.Fatalf("activation failed to flush uplink: ok=%v err=%v", ok, err)
	}
	if _, ok, err := port.takeDownlinkAtEpoch(1); err != nil || ok {
		t.Fatalf("activation failed to flush downlink: ok=%v err=%v", ok, err)
	}
	if err := port.pushUplinkAtEpoch(filledFrame(5), 1); err != nil {
		t.Fatalf("current uplink producer rejected: %v", err)
	}
	if frame, ok, err := port.takeUplinkAtEpoch(1); err != nil || !ok || frame[0] != 5 {
		t.Fatalf("current epoch frame = %v/%v/%v", frame, ok, err)
	}
	port.Close()
	if _, _, err := port.takeUplinkAtEpoch(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed epoch read error = %v", err)
	}
}
