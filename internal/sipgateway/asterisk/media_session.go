package asterisk

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
	"golang.org/x/net/websocket"
)

type mediaSessionConfig struct {
	id           string
	call         sipgateway.CallRef
	channelID    string
	connectionID string
	ws           *websocket.Conn
	now          func() time.Time
	capacity     int
	ioTimeout    time.Duration
	cleanup      func()
}

type queuedFrame struct {
	frame sipgateway.MediaFrame
	epoch uint64
	at    time.Time
}

type mediaControl struct {
	Event            string `json:"event"`
	ConnectionID     string `json:"connection_id"`
	ChannelID        string `json:"channel_id"`
	Format           string `json:"format"`
	OptimalFrameSize int    `json:"optimal_frame_size"`
	Ptime            int    `json:"ptime"`
}

type mediaCommand struct {
	Command string `json:"command"`
}

type mediaSession struct {
	id           string
	call         sipgateway.CallRef
	channelID    string
	connectionID string
	ws           *websocket.Conn
	now          func() time.Time
	ioTimeout    time.Duration
	cleanup      func()

	mu               sync.Mutex
	activateMu       sync.Mutex
	snapshot         sipgateway.MediaSnapshot
	sequence         uint64
	flowPaused       bool
	flowSignal       chan struct{}
	statusSignal     chan struct{}
	statusGeneration uint64
	frames           chan queuedFrame
	ready            chan struct{}
	readyOnce        sync.Once
	done             chan struct{}
	closeOnce        sync.Once
	cleanupOnce      sync.Once
	writeGate        chan struct{}
}

func newMediaSession(cfg mediaSessionConfig) *mediaSession {
	session := &mediaSession{
		id: cfg.id, call: cfg.call, channelID: cfg.channelID, connectionID: cfg.connectionID,
		ws: cfg.ws, now: cfg.now, ioTimeout: cfg.ioTimeout, cleanup: cfg.cleanup,
		frames: make(chan queuedFrame, cfg.capacity), ready: make(chan struct{}),
		done: make(chan struct{}), flowSignal: make(chan struct{}), statusSignal: make(chan struct{}),
		writeGate: make(chan struct{}, 1),
		snapshot: sipgateway.MediaSnapshot{
			LeaseID: cfg.id, Call: cfg.call, Codec: sipgateway.CodecPCMU,
		},
	}
	session.writeGate <- struct{}{}
	go session.readLoop()
	return session
}

func (m *mediaSession) String() string   { return "asterisk.MediaSession{redacted}" }
func (m *mediaSession) GoString() string { return "asterisk.MediaSession{redacted}" }

func (m *mediaSession) ID() string                  { return m.id }
func (m *mediaSession) CallRef() sipgateway.CallRef { return m.call }
func (m *mediaSession) Codec() sipgateway.Codec     { return sipgateway.CodecPCMU }

func (m *mediaSession) waitPrepared(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ready:
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.snapshot.Prepared && !m.snapshot.Closed {
			return nil
		}
		return sipgateway.ErrMediaNotPrepared
	case <-m.done:
		return sipgateway.ErrMediaClosed
	}
}

func (m *mediaSession) readLoop() {
	for {
		var message framedMessage
		if err := receiveFrame.Receive(m.ws, &message); err != nil {
			m.failLocal()
			return
		}
		switch message.payloadType {
		case websocket.TextFrame:
			if err := m.handleControl(message.data); err != nil {
				m.failLocal()
				return
			}
		case websocket.BinaryFrame:
			if err := m.handleBinary(message.data); err != nil {
				m.failLocal()
				return
			}
		default:
			m.failLocal()
			return
		}
	}
}

func (m *mediaSession) handleControl(data []byte) error {
	var control mediaControl
	if len(data) == 0 || json.Unmarshal(data, &control) != nil || control.Event == "" {
		return ErrProtocol
	}
	switch control.Event {
	case "MEDIA_START":
		if control.ConnectionID != m.connectionID || control.ChannelID != m.channelID ||
			control.Format != "ulaw" || control.OptimalFrameSize != pcmuFrameBytes || control.Ptime != 20 {
			return ErrProtocol
		}
		m.mu.Lock()
		if m.snapshot.Closed || m.snapshot.Prepared {
			m.mu.Unlock()
			return ErrProtocol
		}
		m.snapshot.Prepared = true
		m.mu.Unlock()
		m.readyOnce.Do(func() { close(m.ready) })
	case "MEDIA_XOFF", "MEDIA_XON":
		if control.ChannelID != m.channelID {
			return ErrProtocol
		}
		m.mu.Lock()
		if !m.snapshot.Prepared || m.snapshot.Closed {
			m.mu.Unlock()
			return ErrProtocol
		}
		paused := control.Event == "MEDIA_XOFF"
		if m.flowPaused != paused {
			m.flowPaused = paused
			close(m.flowSignal)
			m.flowSignal = make(chan struct{})
		}
		m.mu.Unlock()
	case "STATUS":
		if control.ChannelID != m.channelID {
			return ErrProtocol
		}
		m.mu.Lock()
		if !m.snapshot.Prepared || m.snapshot.Closed {
			m.mu.Unlock()
			return ErrProtocol
		}
		m.statusGeneration++
		close(m.statusSignal)
		m.statusSignal = make(chan struct{})
		m.mu.Unlock()
	default:
		// DTMF and queue-status events are deliberately outside this adapter's
		// authority. A valid JSON control object can be ignored safely.
	}
	return nil
}

func (m *mediaSession) handleBinary(data []byte) error {
	if len(data) == 0 || len(data)%pcmuFrameBytes != 0 || len(data) > maxMediaMessageBytes {
		return ErrProtocol
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.snapshot.Prepared || m.snapshot.Closed {
		return sipgateway.ErrMediaNotPrepared
	}
	// Ringing early media is intentionally consumed and discarded. The
	// browser bridge does not read gateway frames until the exact call has
	// been answered and an activation epoch exists; retaining 20ms frames here
	// would otherwise fill the bounded queue in well under one second and tear
	// down a perfectly valid ringing lease.
	if !m.snapshot.Activated {
		return nil
	}
	for offset := 0; offset < len(data); offset += pcmuFrameBytes {
		m.sequence++
		frame := sipgateway.MediaFrame{
			Sequence: m.sequence,
			Payload:  append([]byte(nil), data[offset:offset+pcmuFrameBytes]...),
		}
		queued := queuedFrame{
			frame: frame, epoch: m.snapshot.ActivationEpoch, at: m.now(),
		}
		select {
		case m.frames <- queued:
		default:
			return sipgateway.ErrMediaQueueFull
		}
	}
	return nil
}

func (m *mediaSession) ReadGatewayFrame(ctx context.Context) (sipgateway.MediaFrame, error) {
	if err := contextErr(ctx); err != nil {
		return sipgateway.MediaFrame{}, err
	}
	select {
	case <-ctx.Done():
		return sipgateway.MediaFrame{}, ctx.Err()
	case <-m.done:
		return sipgateway.MediaFrame{}, sipgateway.ErrMediaClosed
	case queued := <-m.frames:
		m.mu.Lock()
		if m.snapshot.Closed {
			m.mu.Unlock()
			return sipgateway.MediaFrame{}, sipgateway.ErrMediaClosed
		}
		m.snapshot.GatewayToClientFrames++
		m.snapshot.LastGatewayToClientAt = queued.at
		m.snapshot.LastGatewayToClientEpoch = queued.epoch
		m.mu.Unlock()
		return cloneMediaFrame(queued.frame), nil
	}
}

func (m *mediaSession) WriteGatewayFrame(ctx context.Context, frame sipgateway.MediaFrame) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := frame.Validate(); err != nil || len(frame.Payload) != pcmuFrameBytes {
		return sipgateway.ErrMediaNotPrepared
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return sipgateway.ErrMediaClosed
	case <-m.writeGate:
	}
	defer func() { m.writeGate <- struct{}{} }()
	for {
		m.mu.Lock()
		closed := m.snapshot.Closed
		prepared := m.snapshot.Prepared
		paused := m.flowPaused
		flowSignal := m.flowSignal
		m.mu.Unlock()
		if closed {
			return sipgateway.ErrMediaClosed
		}
		if !prepared {
			return sipgateway.ErrMediaNotPrepared
		}
		if !paused {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.done:
			return sipgateway.ErrMediaClosed
		case <-flowSignal:
		}
	}
	deadline := m.now().Add(m.ioTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = m.ws.SetWriteDeadline(deadline)
	if err := websocket.Message.Send(m.ws, append([]byte(nil), frame.Payload...)); err != nil {
		m.failLocal()
		return stableWebsocketError(ctx, err)
	}
	m.mu.Lock()
	if m.snapshot.Closed {
		m.mu.Unlock()
		return sipgateway.ErrMediaClosed
	}
	m.snapshot.ClientToGatewayFrames++
	m.snapshot.LastClientToGatewayAt = m.now()
	m.snapshot.LastClientToGatewayEpoch = m.snapshot.ActivationEpoch
	m.mu.Unlock()
	return nil
}

func (m *mediaSession) Activate(epoch uint64) (sipgateway.MediaSnapshot, error) {
	m.activateMu.Lock()
	defer m.activateMu.Unlock()
	if epoch == 0 {
		return sipgateway.MediaSnapshot{}, sipgateway.ErrInvalidIdentity
	}
	m.mu.Lock()
	if m.snapshot.Closed {
		m.mu.Unlock()
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
	}
	if !m.snapshot.Prepared {
		m.mu.Unlock()
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaNotPrepared
	}
	if m.snapshot.Activated {
		if m.snapshot.ActivationEpoch != epoch {
			m.mu.Unlock()
			return sipgateway.MediaSnapshot{}, sipgateway.ErrStaleRevision
		}
		snapshot := m.snapshot
		m.mu.Unlock()
		return snapshot, nil
	}
	statusGeneration := m.statusGeneration
	statusSignal := m.statusSignal
	m.mu.Unlock()

	// GET_STATUS is an ordered JSON control round-trip on the same media
	// WebSocket. Asterisk emits STATUS after all earlier server-to-client frames
	// already written on that stream, so the read loop has consumed and dropped
	// every ringing frame before this activation boundary is committed.
	timer := time.NewTimer(m.ioTimeout)
	defer timer.Stop()
	select {
	case <-m.done:
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
	case <-timer.C:
		return sipgateway.MediaSnapshot{}, ErrTransport
	case <-m.writeGate:
	}
	defer func() { m.writeGate <- struct{}{} }()
	command, err := json.Marshal(mediaCommand{Command: "GET_STATUS"})
	if err != nil {
		return sipgateway.MediaSnapshot{}, ErrProtocol
	}
	_ = m.ws.SetWriteDeadline(time.Now().Add(m.ioTimeout))
	if err := websocket.Message.Send(m.ws, string(command)); err != nil {
		m.failLocal()
		return sipgateway.MediaSnapshot{}, ErrTransport
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(m.ioTimeout)
	for {
		select {
		case <-m.done:
			return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
		case <-timer.C:
			return sipgateway.MediaSnapshot{}, ErrTransport
		case <-statusSignal:
		}
		m.mu.Lock()
		if m.snapshot.Closed {
			m.mu.Unlock()
			return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
		}
		if m.statusGeneration > statusGeneration {
			m.mu.Unlock()
			break
		}
		statusSignal = m.statusSignal
		m.mu.Unlock()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Closed {
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
	}
	if m.snapshot.Activated {
		if m.snapshot.ActivationEpoch != epoch {
			return sipgateway.MediaSnapshot{}, sipgateway.ErrStaleRevision
		}
		return m.snapshot, nil
	}
	// No frame received while the call was merely ringing can prove live call
	// audio. Holding mu serializes this drain with handleBinary's enqueue.
	for {
		select {
		case <-m.frames:
		default:
			m.snapshot.Activated = true
			m.snapshot.ActivationEpoch = epoch
			m.snapshot.ActivatedAt = m.now()
			m.snapshot.GatewayToClientBaseline = m.snapshot.GatewayToClientFrames
			m.snapshot.ClientToGatewayBaseline = m.snapshot.ClientToGatewayFrames
			return m.snapshot, nil
		}
	}
}

func (m *mediaSession) Snapshot() sipgateway.MediaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

func (m *mediaSession) Close() error {
	m.closeLocal()
	m.cleanupOnce.Do(func() {
		if m.cleanup != nil {
			m.cleanup()
		}
	})
	return nil
}

func (m *mediaSession) closeLocal() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.snapshot.Closed = true
		close(m.flowSignal)
		close(m.statusSignal)
		m.mu.Unlock()
		close(m.done)
		_ = m.ws.Close()
	})
}

func (m *mediaSession) failLocal() {
	m.closeLocal()
	go func() { _ = m.Close() }()
}

func cloneMediaFrame(frame sipgateway.MediaFrame) sipgateway.MediaFrame {
	return sipgateway.MediaFrame{
		Sequence: frame.Sequence, Payload: append([]byte(nil), frame.Payload...),
	}
}
