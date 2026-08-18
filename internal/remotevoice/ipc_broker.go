package remotevoice

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// IPCProtocolVersion is intentionally fixed for the local PCM bridge. There
	// is no compatibility negotiation or downgrade.
	IPCProtocolVersion  = 1
	IPCSessionTokenSize = 32
	IPCMaxPayloadBytes  = FrameBytes

	ipcHeaderBytes = 28
	ipcMagic       = "EVP1"
	// sockaddr_un.sun_path is 104 bytes on macOS, including the terminating
	// NUL. Reject long paths before bind so failure is explicit and portable
	// across Intel and Apple Silicon builds.
	ipcMaxSocketPathBytes = 103

	ipcMessageHello          = 1
	ipcMessageReady          = 2
	ipcMessageDeviceToRemote = 3
	ipcMessageRemoteToDevice = 4
)

var (
	ErrIPCProtocolViolation = errors.New("remotevoice: PCM IPC protocol violation")
	ErrIPCAuthentication    = errors.New("remotevoice: PCM IPC authentication failed")
)

// SyntheticIPCLease is an in-memory, one-use authorization for the local PCM
// bridge. It is deliberately not wired to the remote HTTP API or modem call
// control. SessionToken must never be logged, persisted, placed in argv, or
// reused for another generation.
type SyntheticIPCLease struct {
	// CallGeneration is the protocol field name retained for Swift wire
	// compatibility. Its value is a caller-owned media lease generation and is
	// not required to equal a raw modem/CLCC topology counter.
	CallGeneration uint64                    `json:"callGeneration"`
	SessionToken   [IPCSessionTokenSize]byte `json:"-"`
}

func (SyntheticIPCLease) String() string   { return "remotevoice.SyntheticIPCLease{redacted}" }
func (SyntheticIPCLease) GoString() string { return "remotevoice.SyntheticIPCLease{redacted}" }

// NewSyntheticIPCLease creates a one-use bearer token for synthetic bridge
// validation. Same-UID peer validation and the mode-0600 socket are mandatory
// additional checks; this token is not a challenge-response protocol.
func NewSyntheticIPCLease(leaseGeneration uint64) (SyntheticIPCLease, error) {
	if leaseGeneration == 0 {
		return SyntheticIPCLease{}, errors.New("remotevoice: media lease generation must be non-zero")
	}
	lease := SyntheticIPCLease{CallGeneration: leaseGeneration}
	if _, err := io.ReadFull(rand.Reader, lease.SessionToken[:]); err != nil {
		return SyntheticIPCLease{}, fmt.Errorf("remotevoice: create IPC session token: %w", err)
	}
	return lease, nil
}

// SyntheticIPCBrokerConfig configures a local-only, one-session PCM broker.
// SocketPath must live directly inside a dedicated same-UID mode-0700
// directory. StartSyntheticIPCBroker never removes a pre-existing path.
type SyntheticIPCBrokerConfig struct {
	SocketPath string `json:"-"`
	Lease      SyntheticIPCLease
	Port       *FramePort `json:"-"`
	IOTimeout  time.Duration
	// AuthenticatedIdleTimeout applies only after Ready and before the first
	// PCM frame. It lets a prepared local route wait briefly for explicit call
	// authorization without weakening the active per-frame stall timeout.
	AuthenticatedIdleTimeout time.Duration
	// ActivityNow is used only to timestamp authenticated, real PCM activity.
	// Deadlines intentionally continue to use the system clock. Tests and a
	// lease owner may inject a deterministic concurrency-safe clock here.
	ActivityNow     func() time.Time `json:"-"`
	beforeFrameRead func(uint64)     `json:"-"`
	writeMessage    func(io.Writer, ipcMessage) error
}

func (SyntheticIPCBrokerConfig) String() string {
	return "remotevoice.SyntheticIPCBrokerConfig{redacted}"
}
func (SyntheticIPCBrokerConfig) GoString() string {
	return "remotevoice.SyntheticIPCBrokerConfig{redacted}"
}

// IPCBrokerActivitySnapshot is a credential-free view of local PCM bridge
// activity. RemoteDownlinkFrames counts only frames actually dequeued from the
// network-to-device ring; synthesized silence is deliberately excluded.
type IPCBrokerActivitySnapshot struct {
	Consumed        bool
	AuthenticatedAt time.Time
	Ready           bool
	ReadyAt         time.Time
	// Live is current, not historical: it is true only after the complete Ready
	// response was written and becomes false before broker termination is visible.
	Live                    bool
	DeviceUplinkFrames      uint64
	LastDeviceUplinkAt      time.Time
	LastDeviceUplinkEpoch   uint64
	RemoteDownlinkFrames    uint64
	LastRemoteDownlinkAt    time.Time
	LastRemoteDownlinkEpoch uint64
}

// SyntheticIPCBroker is the legacy-named concrete LocalPCMBroker. It exposes a
// FramePort to one same-UID Swift client over a fixed binary Unix-socket
// protocol. Constructing it proves only local transport and never authorizes
// modem call control by itself.
type SyntheticIPCBroker struct {
	listener                 *net.UnixListener
	socketPath               string
	socketIdentity           os.FileInfo
	port                     *FramePort
	generation               uint64
	token                    [IPCSessionTokenSize]byte
	timeout                  time.Duration
	authenticatedIdleTimeout time.Duration
	peerEUID                 func(*net.UnixConn) (uint32, error)
	activityNow              func() time.Time
	exchangeEpoch            uint64
	beforeFrameRead          func(uint64)
	writeMessage             func(io.Writer, ipcMessage) error

	mu       sync.Mutex
	active   *net.UnixConn
	closing  bool
	activity IPCBrokerActivitySnapshot
	terminal error
	done     chan struct{}
	once     sync.Once
	// activationMu serializes completed exchanges against Activate while never
	// blocking activation on a client read that has not returned yet.
	activationMu sync.Mutex
}

func (*SyntheticIPCBroker) String() string   { return "remotevoice.SyntheticIPCBroker{redacted}" }
func (*SyntheticIPCBroker) GoString() string { return "remotevoice.SyntheticIPCBroker{redacted}" }

type ipcMessage struct {
	kind       byte
	generation uint64
	sequence   uint64
	payload    []byte
}

// StartSyntheticIPCBroker starts the legacy-named local bridge. The returned
// bearer/socket is never exposed to the remote HTTP API by this package.
func StartSyntheticIPCBroker(cfg SyntheticIPCBrokerConfig) (*SyntheticIPCBroker, error) {
	return startSyntheticIPCBroker(cfg, peerEffectiveUID)
}

func startSyntheticIPCBroker(
	cfg SyntheticIPCBrokerConfig,
	peerLookup func(*net.UnixConn) (uint32, error),
) (*SyntheticIPCBroker, error) {
	if err := ipcPlatformSupported(); err != nil {
		return nil, err
	}
	if cfg.Port == nil {
		return nil, errors.New("remotevoice: PCM IPC frame port is required")
	}
	if cfg.Lease.CallGeneration == 0 {
		return nil, errors.New("remotevoice: PCM IPC call generation must be non-zero")
	}
	if allZero(cfg.Lease.SessionToken[:]) {
		return nil, errors.New("remotevoice: PCM IPC session token must be random and non-zero")
	}
	if peerLookup == nil {
		return nil, errors.New("remotevoice: PCM IPC peer credential lookup is required")
	}
	timeout := cfg.IOTimeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	if timeout < 100*time.Millisecond || timeout > 10*time.Second {
		return nil, errors.New("remotevoice: PCM IPC timeout must be between 100 ms and 10 s")
	}
	authenticatedIdleTimeout := cfg.AuthenticatedIdleTimeout
	if authenticatedIdleTimeout == 0 {
		authenticatedIdleTimeout = timeout
	}
	if authenticatedIdleTimeout < timeout || authenticatedIdleTimeout > 2*time.Minute {
		return nil, errors.New("remotevoice: authenticated PCM IPC idle timeout must be between the frame timeout and 2 minutes")
	}
	activityNow := cfg.ActivityNow
	if activityNow == nil {
		activityNow = time.Now
	}
	writeMessage := cfg.writeMessage
	if writeMessage == nil {
		writeMessage = writeIPCMessage
	}

	parentIdentity, err := validateIPCPathBeforeListen(cfg.SocketPath)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("remotevoice: listen on PCM IPC socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	createdIdentity, err := os.Lstat(cfg.SocketPath)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("remotevoice: inspect newly bound PCM IPC socket: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = listener.Close()
			removeOwnedSocket(cfg.SocketPath, createdIdentity)
		}
	}()

	if err := secureIPCSocketMode(cfg.SocketPath); err != nil {
		return nil, fmt.Errorf("remotevoice: secure PCM IPC socket: %w", err)
	}
	socketIdentity, err := validateIPCPathAfterListen(cfg.SocketPath, parentIdentity)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(createdIdentity, socketIdentity) {
		return nil, errors.New("remotevoice: PCM IPC socket changed while securing it")
	}

	b := &SyntheticIPCBroker{
		listener:                 listener,
		socketPath:               cfg.SocketPath,
		socketIdentity:           socketIdentity,
		port:                     cfg.Port,
		generation:               cfg.Lease.CallGeneration,
		token:                    cfg.Lease.SessionToken,
		timeout:                  timeout,
		authenticatedIdleTimeout: authenticatedIdleTimeout,
		peerEUID:                 peerLookup,
		activityNow:              activityNow,
		beforeFrameRead:          cfg.beforeFrameRead,
		writeMessage:             writeMessage,
		done:                     make(chan struct{}),
	}
	cleanup = false
	go b.serve()
	return b, nil
}

// SocketPath returns the broker's local socket path. It is not a
// remote endpoint and possession of the path does not replace the lease token.
func (b *SyntheticIPCBroker) SocketPath() string { return b.socketPath }

// Wait waits for the one authenticated session to finish or for Close.
func (b *SyntheticIPCBroker) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.terminal
	}
}

// Close stops the listener and any current local client. It never closes
// the caller-owned FramePort.
func (b *SyntheticIPCBroker) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.activity.Live = false
		for index := range b.token {
			b.token[index] = 0
		}
		listener := b.listener
		active := b.active
		b.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
		if active != nil {
			_ = active.Close()
		}
	})
	<-b.done
	return nil
}

// Consumed reports whether the one-use token authenticated a local client.
// It exposes no token or audio data and is useful for fail-closed lifecycle
// bookkeeping by a future media lease owner.
func (b *SyntheticIPCBroker) Consumed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activity.Consumed
}

// ActivitySnapshot returns timestamps and counters without exposing the
// socket path, call token, or one-use bearer token.
func (b *SyntheticIPCBroker) ActivitySnapshot() IPCBrokerActivitySnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activity
}

// Activate establishes an epoch barrier for subsequent request/response
// exchanges and flushes queued pre-activation PCM. A read already waiting at
// the boundary retains the previous epoch and cannot satisfy post-activation
// freshness.
func (b *SyntheticIPCBroker) Activate(epoch uint64) (IPCBrokerActivitySnapshot, error) {
	if epoch == 0 {
		return IPCBrokerActivitySnapshot{}, errors.New("remotevoice: IPC activation epoch must be non-zero")
	}
	b.activationMu.Lock()
	defer b.activationMu.Unlock()
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return IPCBrokerActivitySnapshot{}, ErrClosed
	}
	if b.exchangeEpoch != 0 && b.exchangeEpoch != epoch {
		b.mu.Unlock()
		return IPCBrokerActivitySnapshot{}, errors.New("remotevoice: IPC activation epoch cannot be reset")
	}
	if b.exchangeEpoch == 0 {
		if err := b.port.activate(epoch); err != nil {
			b.mu.Unlock()
			return IPCBrokerActivitySnapshot{}, err
		}
		b.exchangeEpoch = epoch
	}
	activity := b.activity
	b.mu.Unlock()
	return activity, nil
}

func (b *SyntheticIPCBroker) serve() {
	defer close(b.done)
	defer removeOwnedSocket(b.socketPath, b.socketIdentity)
	defer b.setLive(false)
	for {
		conn, err := b.listener.AcceptUnix()
		if err != nil {
			if !b.isClosing() {
				b.setTerminal(fmt.Errorf("remotevoice: accept PCM IPC client: %w", err))
			}
			return
		}
		b.setActive(conn)
		authenticated, sessionErr := b.handleCandidate(conn)
		_ = conn.Close()
		b.clearActive(conn)
		if authenticated {
			if sessionErr != nil && !errors.Is(sessionErr, io.EOF) && !b.isClosing() {
				b.setTerminal(sessionErr)
			}
			return
		}
		if b.isClosing() {
			return
		}
	}
}

func (b *SyntheticIPCBroker) handleCandidate(conn *net.UnixConn) (bool, error) {
	livePublished := false
	defer func() {
		if livePublished {
			b.setLive(false)
		}
	}()
	peerUID, err := b.peerEUID(conn)
	if err != nil || peerUID != uint32(os.Geteuid()) {
		return false, ErrIPCAuthentication
	}
	if err := conn.SetDeadline(time.Now().Add(b.timeout)); err != nil {
		return false, ErrIPCAuthentication
	}
	hello, err := readIPCMessage(conn)
	if err != nil {
		return false, ErrIPCAuthentication
	}
	if hello.kind != ipcMessageHello || hello.generation != b.generation || hello.sequence != 0 ||
		len(hello.payload) != IPCSessionTokenSize {
		return false, ErrIPCAuthentication
	}
	b.mu.Lock()
	authenticated := !b.closing && subtle.ConstantTimeCompare(hello.payload, b.token[:]) == 1
	if authenticated {
		for i := range b.token {
			b.token[i] = 0
		}
	}
	if !authenticated {
		b.mu.Unlock()
		return false, ErrIPCAuthentication
	}
	b.activity.Consumed = true
	b.activity.AuthenticatedAt = b.activityNow()
	b.mu.Unlock()
	// A successful lease is single-use. Stop accepting before acknowledging it.
	_ = b.listener.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(b.timeout)); err != nil {
		return true, fmt.Errorf("%w: ready deadline", ErrIPCProtocolViolation)
	}
	if err := b.writeMessage(conn, ipcMessage{
		kind:       ipcMessageReady,
		generation: b.generation,
		sequence:   0,
	}); err != nil {
		return true, fmt.Errorf("%w: ready response", ErrIPCProtocolViolation)
	}
	// Ready and Live are published only after the complete response write.
	// A client read may briefly precede this store (a safe false-negative), but
	// a blocked/failed write can never make TransportPrepared true.
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return true, ErrClosed
	}
	b.activity.Ready = true
	b.activity.ReadyAt = b.activityNow()
	b.activity.Live = true
	b.mu.Unlock()
	livePublished = true

	expectedSequence := uint64(1)
	for {
		exchangeEpoch := b.currentExchangeEpoch()
		if b.beforeFrameRead != nil {
			b.beforeFrameRead(exchangeEpoch)
		}
		readTimeout := b.timeout
		if expectedSequence == 1 {
			readTimeout = b.authenticatedIdleTimeout
		}
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return true, fmt.Errorf("%w: deadline", ErrIPCProtocolViolation)
		}
		message, err := readIPCMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, io.EOF
			}
			var networkErr net.Error
			if errors.As(err, &networkErr) && networkErr.Timeout() {
				return true, fmt.Errorf("%w: authenticated PCM IPC timed out", ErrIPCProtocolViolation)
			}
			return true, err
		}
		if message.kind != ipcMessageDeviceToRemote || message.generation != b.generation ||
			message.sequence != expectedSequence || len(message.payload) != FrameBytes {
			return true, ErrIPCProtocolViolation
		}
		// The longer pre-call idle allowance ends as soon as the first real
		// frame arrives. Every response write and all later frame reads retain
		// the short active stall timeout.
		if err := conn.SetWriteDeadline(time.Now().Add(b.timeout)); err != nil {
			return true, fmt.Errorf("%w: deadline", ErrIPCProtocolViolation)
		}
		deviceFrame := decodePCM16LE(message.payload)

		b.activationMu.Lock()
		currentExchange := b.currentExchangeEpoch() == exchangeEpoch
		var remoteFrame []int16
		remoteFramePresent := false
		if currentExchange {
			var portErr error
			remoteFrame, remoteFramePresent, portErr = b.port.takeDownlinkAtEpoch(exchangeEpoch)
			if errors.Is(portErr, ErrStaleMediaEpoch) {
				currentExchange = false
			} else if portErr != nil {
				b.activationMu.Unlock()
				return true, fmt.Errorf("%w: frame port unavailable", ErrIPCProtocolViolation)
			}
		}
		if !currentExchange || !remoteFramePresent {
			remoteFrame = make([]int16, FrameSamples)
		}
		if err := b.writeMessage(conn, ipcMessage{
			kind:       ipcMessageRemoteToDevice,
			generation: b.generation,
			sequence:   expectedSequence,
			payload:    encodePCM16LE(remoteFrame),
		}); err != nil {
			b.activationMu.Unlock()
			return true, fmt.Errorf("%w: frame response", ErrIPCProtocolViolation)
		}
		if currentExchange {
			if err := b.port.pushUplinkAtEpoch(deviceFrame, exchangeEpoch); errors.Is(err, ErrStaleMediaEpoch) {
				currentExchange = false
			} else if err != nil {
				b.activationMu.Unlock()
				return true, fmt.Errorf("%w: frame port unavailable", ErrIPCProtocolViolation)
			}
		}
		// One request/response exchange is the unit of authenticated local media
		// activity. Never publish either direction before the complete response
		// write succeeds.
		if currentExchange {
			b.recordExchange(remoteFramePresent, exchangeEpoch)
		}
		b.activationMu.Unlock()
		if expectedSequence == ^uint64(0) {
			return true, ErrIPCProtocolViolation
		}
		expectedSequence++
	}
}

func (b *SyntheticIPCBroker) setLive(live bool) {
	b.mu.Lock()
	b.activity.Live = live
	b.mu.Unlock()
}

func (b *SyntheticIPCBroker) currentExchangeEpoch() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exchangeEpoch
}

func (b *SyntheticIPCBroker) recordExchange(remoteDownlink bool, epoch uint64) {
	now := b.activityNow()
	b.mu.Lock()
	b.activity.DeviceUplinkFrames++
	b.activity.LastDeviceUplinkAt = now
	b.activity.LastDeviceUplinkEpoch = epoch
	if remoteDownlink {
		b.activity.RemoteDownlinkFrames++
		b.activity.LastRemoteDownlinkAt = now
		b.activity.LastRemoteDownlinkEpoch = epoch
	}
	b.mu.Unlock()
}

func readIPCMessage(r io.Reader) (ipcMessage, error) {
	header := make([]byte, ipcHeaderBytes)
	if _, err := io.ReadFull(r, header); err != nil {
		return ipcMessage{}, err
	}
	if string(header[:4]) != ipcMagic || header[4] != IPCProtocolVersion ||
		header[6] != 0 || header[7] != 0 {
		return ipcMessage{}, ErrIPCProtocolViolation
	}
	payloadBytes := binary.BigEndian.Uint32(header[24:28])
	if payloadBytes > IPCMaxPayloadBytes {
		return ipcMessage{}, ErrIPCProtocolViolation
	}
	message := ipcMessage{
		kind:       header[5],
		generation: binary.BigEndian.Uint64(header[8:16]),
		sequence:   binary.BigEndian.Uint64(header[16:24]),
		payload:    make([]byte, int(payloadBytes)),
	}
	if _, err := io.ReadFull(r, message.payload); err != nil {
		return ipcMessage{}, err
	}
	return message, nil
}

func writeIPCMessage(w io.Writer, message ipcMessage) error {
	if len(message.payload) > IPCMaxPayloadBytes {
		return ErrIPCProtocolViolation
	}
	header := make([]byte, ipcHeaderBytes)
	copy(header[:4], ipcMagic)
	header[4] = IPCProtocolVersion
	header[5] = message.kind
	binary.BigEndian.PutUint64(header[8:16], message.generation)
	binary.BigEndian.PutUint64(header[16:24], message.sequence)
	binary.BigEndian.PutUint32(header[24:28], uint32(len(message.payload)))
	if err := writeFull(w, header); err != nil {
		return err
	}
	return writeFull(w, message.payload)
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := w.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func encodePCM16LE(samples []int16) []byte {
	payload := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(payload[index*2:index*2+2], uint16(sample))
	}
	return payload
}

func decodePCM16LE(payload []byte) []int16 {
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2 : index*2+2]))
	}
	return samples
}

func allZero(value []byte) bool {
	combined := byte(0)
	for _, item := range value {
		combined |= item
	}
	return combined == 0
}

func (b *SyntheticIPCBroker) isClosing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closing
}

func (b *SyntheticIPCBroker) setActive(conn *net.UnixConn) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		_ = conn.Close()
		return
	}
	b.active = conn
	b.mu.Unlock()
}

func (b *SyntheticIPCBroker) clearActive(conn *net.UnixConn) {
	b.mu.Lock()
	if b.active == conn {
		b.active = nil
	}
	b.mu.Unlock()
}

func (b *SyntheticIPCBroker) setTerminal(err error) {
	b.mu.Lock()
	if b.terminal == nil {
		b.terminal = err
	}
	b.mu.Unlock()
}

func validateIPCPathBeforeListen(socketPath string) (os.FileInfo, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return nil, errors.New("remotevoice: PCM IPC socket path must be clean and absolute")
	}
	if len([]byte(socketPath)) > ipcMaxSocketPathBytes {
		return nil, errors.New("remotevoice: PCM IPC socket path exceeds the macOS limit")
	}
	parent := filepath.Dir(socketPath)
	if parent == filepath.Dir(parent) {
		return nil, errors.New("remotevoice: PCM IPC socket needs a dedicated parent directory")
	}
	parentInfo, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(parent, 0o700); err != nil {
			return nil, fmt.Errorf("remotevoice: create PCM IPC directory: %w", err)
		}
		parentInfo, err = os.Lstat(parent)
	}
	if err != nil {
		return nil, fmt.Errorf("remotevoice: inspect PCM IPC directory: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, errors.New("remotevoice: PCM IPC parent must be a real directory")
	}
	if parentInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("remotevoice: PCM IPC parent permissions must be 0700")
	}
	if err := requireCurrentUserOwner(parentInfo); err != nil {
		return nil, fmt.Errorf("remotevoice: PCM IPC parent ownership: %w", err)
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return nil, errors.New("remotevoice: refusing pre-existing PCM IPC socket path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remotevoice: inspect PCM IPC socket path: %w", err)
	}
	return parentInfo, nil
}

func validateIPCPathAfterListen(socketPath string, originalParent os.FileInfo) (os.FileInfo, error) {
	parentInfo, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil || !os.SameFile(originalParent, parentInfo) || parentInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("remotevoice: PCM IPC parent changed while binding")
	}
	if err := requireCurrentUserOwner(parentInfo); err != nil {
		return nil, fmt.Errorf("remotevoice: PCM IPC parent ownership changed: %w", err)
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("remotevoice: inspect bound PCM IPC socket: %w", err)
	}
	if socketInfo.Mode()&os.ModeSymlink != 0 || socketInfo.Mode()&os.ModeSocket == 0 ||
		socketInfo.Mode().Perm() != 0o600 {
		return nil, errors.New("remotevoice: bound PCM IPC path is not a mode-0600 socket")
	}
	if err := requireCurrentUserOwner(socketInfo); err != nil {
		return nil, fmt.Errorf("remotevoice: PCM IPC socket ownership: %w", err)
	}
	return socketInfo, nil
}

func removeOwnedSocket(path string, expected os.FileInfo) {
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSocket == 0 {
		return
	}
	if expected != nil && !os.SameFile(expected, current) {
		return
	}
	_ = os.Remove(path)
}
