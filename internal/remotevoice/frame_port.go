package remotevoice

import (
	"errors"
	"sync"
)

var (
	ErrClosed          = errors.New("remotevoice: frame port closed")
	ErrInvalidFrame    = errors.New("remotevoice: PCM frame must contain exactly 160 samples")
	ErrStaleMediaEpoch = errors.New("remotevoice: stale media epoch")
)

type PortStats struct {
	UplinkOverflow   uint64
	UplinkUnderrun   uint64
	DownlinkOverflow uint64
	DownlinkUnderrun uint64
}

type pcmRing struct {
	frames   [][]int16
	head     int
	length   int
	overflow uint64
	underrun uint64
}

type FramePort struct {
	mu       sync.Mutex
	closed   bool
	epoch    uint64
	uplink   pcmRing
	downlink pcmRing
}

func NewFramePort(capacity int) (*FramePort, error) {
	if capacity <= 0 {
		return nil, errors.New("remotevoice: frame capacity must be positive")
	}
	return &FramePort{
		uplink:   pcmRing{frames: make([][]int16, capacity)},
		downlink: pcmRing{frames: make([][]int16, capacity)},
	}, nil
}

// PushUplink queues one modem/device-to-network PCM frame without blocking.
// When full, the oldest queued frame is discarded.
func (p *FramePort) PushUplink(frame []int16) error {
	return p.pushAtEpoch(&p.uplink, frame, 0)
}

// PushUplinkAtEpoch queues one device-to-network PCM frame only when epoch is
// the currently activated media epoch. It is intended for an in-process media
// bridge whose lifetime is owned by the same higher-level lease as Session.
// A stale producer cannot enqueue after an activation flush.
func (p *FramePort) PushUplinkAtEpoch(frame []int16, epoch uint64) error {
	return p.pushAtEpoch(&p.uplink, frame, epoch)
}

// TakeDownlink takes one network-to-modem/device PCM frame without blocking.
// ok is false on an empty or closed port; an empty read counts as an underrun.
func (p *FramePort) TakeDownlink() (frame []int16, ok bool) {
	return p.pop(&p.downlink)
}

// TakeDownlinkAtEpoch dequeues one network-to-device PCM frame only for the
// currently activated epoch. An empty queue returns ok=false and a nil error;
// a stale epoch returns ErrStaleMediaEpoch.
func (p *FramePort) TakeDownlinkAtEpoch(epoch uint64) (frame []int16, ok bool, err error) {
	return p.popAtEpoch(&p.downlink, epoch)
}

// pushDownlink and takeUplink are the zero-epoch network-side helpers used by
// Session before activation.
func (p *FramePort) pushDownlink(frame []int16) error { return p.pushAtEpoch(&p.downlink, frame, 0) }
func (p *FramePort) takeUplink() ([]int16, bool)      { return p.pop(&p.uplink) }

// PushDownlinkAtEpoch and TakeUplinkAtEpoch are the activated network-side
// seam for an injected MediaPeer implementation. Normal callers should use
// Session; higher-level in-process bridges use these only through the same
// generation owner and epoch barrier.
func (p *FramePort) PushDownlinkAtEpoch(frame []int16, epoch uint64) error {
	return p.pushAtEpoch(&p.downlink, frame, epoch)
}

func (p *FramePort) TakeUplinkAtEpoch(epoch uint64) (frame []int16, ok bool, err error) {
	return p.popAtEpoch(&p.uplink, epoch)
}

func (p *FramePort) pushUplinkAtEpoch(frame []int16, epoch uint64) error {
	return p.pushAtEpoch(&p.uplink, frame, epoch)
}

func (p *FramePort) pushDownlinkAtEpoch(frame []int16, epoch uint64) error {
	return p.pushAtEpoch(&p.downlink, frame, epoch)
}

func (p *FramePort) takeUplinkAtEpoch(epoch uint64) (frame []int16, ok bool, err error) {
	return p.popAtEpoch(&p.uplink, epoch)
}

func (p *FramePort) takeDownlinkAtEpoch(epoch uint64) (frame []int16, ok bool, err error) {
	return p.popAtEpoch(&p.downlink, epoch)
}

func (p *FramePort) pushAtEpoch(ring *pcmRing, frame []int16, epoch uint64) error {
	if len(frame) != FrameSamples {
		return ErrInvalidFrame
	}
	copyFrame := append([]int16(nil), frame...)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.epoch != epoch {
		return ErrStaleMediaEpoch
	}
	if ring.length == len(ring.frames) {
		ring.frames[ring.head] = nil
		ring.head = (ring.head + 1) % len(ring.frames)
		ring.length--
		ring.overflow++
	}
	index := (ring.head + ring.length) % len(ring.frames)
	ring.frames[index] = copyFrame
	ring.length++
	return nil
}

func (p *FramePort) popAtEpoch(ring *pcmRing, epoch uint64) (frame []int16, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, ErrClosed
	}
	if p.epoch != epoch {
		return nil, false, ErrStaleMediaEpoch
	}
	if ring.length == 0 {
		ring.underrun++
		return nil, false, nil
	}
	frame = ring.frames[ring.head]
	ring.frames[ring.head] = nil
	ring.head = (ring.head + 1) % len(ring.frames)
	ring.length--
	return frame, true, nil
}

func (p *FramePort) pop(ring *pcmRing) ([]int16, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || ring.length == 0 {
		ring.underrun++
		return nil, false
	}
	frame := ring.frames[ring.head]
	ring.frames[ring.head] = nil
	ring.head = (ring.head + 1) % len(ring.frames)
	ring.length--
	return frame, true
}

func (p *FramePort) Stats() PortStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PortStats{
		UplinkOverflow:   p.uplink.overflow,
		UplinkUnderrun:   p.uplink.underrun,
		DownlinkOverflow: p.downlink.overflow,
		DownlinkUnderrun: p.downlink.underrun,
	}
}

func (p *FramePort) Close() {
	p.mu.Lock()
	p.closed = true
	for i := range p.uplink.frames {
		p.uplink.frames[i] = nil
	}
	for i := range p.downlink.frames {
		p.downlink.frames[i] = nil
	}
	p.uplink.length = 0
	p.downlink.length = 0
	p.mu.Unlock()
}

// flush discards queued media at an activation boundary without closing the
// port or rewriting lifetime underrun/overflow statistics.
func (p *FramePort) flush() {
	p.mu.Lock()
	p.flushLocked()
	p.mu.Unlock()
}

// activate changes the port epoch and flushes both rings under the same lock.
// A producer from an earlier epoch therefore cannot enqueue after the flush:
// its epoch check and enqueue are one atomic port operation.
func (p *FramePort) activate(epoch uint64) error {
	if epoch == 0 {
		return errors.New("remotevoice: frame port activation epoch must be non-zero")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.epoch != 0 && p.epoch != epoch {
		return errors.New("remotevoice: frame port activation epoch cannot be reset")
	}
	if p.epoch == epoch {
		return nil
	}
	p.epoch = epoch
	p.flushLocked()
	return nil
}

// Activate advances the port to one non-zero media epoch and atomically drops
// all frames queued before that boundary. Repeating the same epoch is
// idempotent; changing a non-zero epoch is rejected to prevent ABA reuse.
func (p *FramePort) Activate(epoch uint64) error { return p.activate(epoch) }

func (p *FramePort) flushLocked() {
	for i := range p.uplink.frames {
		p.uplink.frames[i] = nil
	}
	for i := range p.downlink.frames {
		p.downlink.frames[i] = nil
	}
	p.uplink.head = 0
	p.uplink.length = 0
	p.downlink.head = 0
	p.downlink.length = 0
}
