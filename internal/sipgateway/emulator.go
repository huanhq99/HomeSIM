package sipgateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultEmulatorEventCapacity = 64
	defaultEmulatorMediaCapacity = 8
)

// CommandKind names the two mutations in the minimum incoming-call slice.
type CommandKind string

const (
	CommandAnswerIncoming CommandKind = "answer_incoming"
	CommandEndActive      CommandKind = "end_active"
)

// CommandFault configures exactly one subsequent emulator command. Unknown may
// optionally apply its state change while withholding the result, reproducing
// a lost response without asking Coordinator to guess.
type CommandFault struct {
	Outcome   CommandOutcome
	Apply     bool
	EmitEvent bool
}

// EmulatorConfig creates a socket-free, deterministic adapter.
type EmulatorConfig struct {
	GatewayID          string           `json:"-"`
	BootEpoch          string           `json:"-"`
	Capabilities       Capabilities     `json:"capabilities"`
	Now                func() time.Time `json:"-"`
	EventCapacity      int
	MediaQueueCapacity int
	MaxBootEpochs      int
}

func (EmulatorConfig) String() string   { return "sipgateway.EmulatorConfig{redacted}" }
func (EmulatorConfig) GoString() string { return "sipgateway.EmulatorConfig{redacted}" }

// Emulator is a pure in-memory Adapter intended for contract acceptance tests.
type Emulator struct {
	mu             sync.Mutex
	gatewayID      string
	bootEpoch      string
	seenBootEpochs map[string]struct{}
	maxBootEpochs  int
	capabilities   Capabilities
	now            func() time.Time
	mediaCapacity  int
	events         chan Event
	done           chan struct{}
	closed         bool
	closeOnce      sync.Once
	calls          map[string]CallSnapshot
	media          map[string]*emulatedMedia
	mediaCounter   uint64
	dialCounter    uint64
	faults         map[CommandKind]CommandFault
	commands       map[string]emulatorCommandRecord
	executions     map[CommandKind]uint64
}

type emulatorCommandFingerprint struct {
	kind         CommandKind
	call         CallRef
	mediaLeaseID string
}

type emulatorCommandRecord struct {
	fingerprint emulatorCommandFingerprint
	result      CommandResult
}

// NewEmulator validates all identity and capability inputs before publishing
// an adapter.
func NewEmulator(cfg EmulatorConfig) (*Emulator, error) {
	if !validToken(cfg.GatewayID, 1, 64) || !validOpaque(cfg.BootEpoch, 8, 128) {
		return nil, ErrInvalidIdentity
	}
	if err := cfg.Capabilities.Validate(); err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.EventCapacity == 0 {
		cfg.EventCapacity = defaultEmulatorEventCapacity
	}
	if cfg.EventCapacity < 1 || cfg.EventCapacity > 4096 {
		return nil, ErrObservationQueueFull
	}
	if cfg.MediaQueueCapacity == 0 {
		cfg.MediaQueueCapacity = defaultEmulatorMediaCapacity
	}
	if cfg.MediaQueueCapacity < 1 || cfg.MediaQueueCapacity > 4096 {
		return nil, ErrMediaQueueFull
	}
	if cfg.MaxBootEpochs == 0 {
		cfg.MaxBootEpochs = defaultMaxBootEpochs
	}
	if cfg.MaxBootEpochs < 1 || cfg.MaxBootEpochs > 65536 {
		return nil, ErrBootEpochStoreFull
	}
	return &Emulator{
		gatewayID: cfg.GatewayID, bootEpoch: cfg.BootEpoch,
		seenBootEpochs: map[string]struct{}{cfg.BootEpoch: {}},
		maxBootEpochs:  cfg.MaxBootEpochs,
		capabilities: Capabilities{
			Incoming: cfg.Capabilities.Incoming, Dial: cfg.Capabilities.Dial, PrepareMedia: cfg.Capabilities.PrepareMedia,
			AnswerIncoming: cfg.Capabilities.AnswerIncoming, EndActive: cfg.Capabilities.EndActive,
			Codecs: cloneCodecs(cfg.Capabilities.Codecs),
		},
		now: cfg.Now, mediaCapacity: cfg.MediaQueueCapacity,
		events: make(chan Event, cfg.EventCapacity), done: make(chan struct{}),
		calls: make(map[string]CallSnapshot), media: make(map[string]*emulatedMedia),
		faults: make(map[CommandKind]CommandFault), commands: make(map[string]emulatorCommandRecord),
		executions: make(map[CommandKind]uint64),
	}, nil
}

// Dial creates one deterministic outbound ringing dialog for contract tests.
// It deliberately models provider admission plus the normal observation event;
// production adapters remain responsible for their own trunk/dialplan checks.
func (e *Emulator) Dial(ctx context.Context, number string) (CallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return CallSnapshot{}, err
	}
	if !validDialNumber(number) {
		return CallSnapshot{}, ErrInvalidCommand
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return CallSnapshot{}, ErrClosed
	}
	if !e.capabilities.Dial {
		return CallSnapshot{}, ErrCapabilityUnavailable
	}
	for _, current := range e.calls {
		if current.State != ProviderCallEnded {
			return CallSnapshot{}, ErrConcurrentCall
		}
	}
	e.dialCounter++
	handle := "outgoing-" + strconv.FormatUint(e.dialCounter, 36) + "-" + base64.RawURLEncoding.EncodeToString([]byte(number))
	ref := CallRef{GatewayID: e.gatewayID, BootEpoch: e.bootEpoch, ProviderHandle: handle, Revision: 1}
	snapshot := CallSnapshot{Ref: ref, State: ProviderCallIncoming, Direction: CallDirectionOutgoing}
	e.calls[handle] = snapshot
	if err := e.enqueueCallLocked(snapshot); err != nil {
		delete(e.calls, handle)
		return CallSnapshot{}, err
	}
	return snapshot, nil
}

// Capabilities returns an isolated copy of the emulator's contract.
func (e *Emulator) Capabilities() Capabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	copy := e.capabilities
	copy.Codecs = cloneCodecs(copy.Codecs)
	return copy
}

// Observe returns the next scripted observation without opening a socket.
func (e *Emulator) Observe(ctx context.Context) (Event, error) {
	if err := contextError(ctx); err != nil {
		return Event{}, err
	}
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return Event{}, ErrClosed
	}
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-e.done:
		return Event{}, ErrClosed
	case event := <-e.events:
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return Event{}, ErrClosed
		}
		return cloneEvent(event), nil
	}
}

// Inspect returns current authoritative state for a dialog. The supplied
// revision may be old because this method is the only recovery path after an
// ambiguous command.
func (e *Emulator) Inspect(ctx context.Context, ref CallRef) (CallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return CallSnapshot{}, err
	}
	if err := ref.Validate(); err != nil {
		return CallSnapshot{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return CallSnapshot{}, ErrClosed
	}
	if ref.GatewayID != e.gatewayID || ref.BootEpoch != e.bootEpoch {
		return CallSnapshot{}, ErrBootEpochMismatch
	}
	current, ok := e.calls[ref.ProviderHandle]
	if !ok {
		return CallSnapshot{}, ErrCallNotFound
	}
	return current, nil
}

// RecoveryToken seals one exact, locally known synthetic dialog identity into
// a bounded token that another emulator with the same configured provider
// incarnation can parse. The token remains private through RecoveryToken's
// redacted formatting and binary-only persistence boundary.
func (e *Emulator) RecoveryToken(ref CallRef) (RecoveryToken, error) {
	if err := ref.Validate(); err != nil {
		return RecoveryToken{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return RecoveryToken{}, ErrClosed
	}
	if ref.GatewayID != e.gatewayID || ref.BootEpoch != e.bootEpoch {
		return RecoveryToken{}, ErrBootEpochMismatch
	}
	current, ok := e.calls[ref.ProviderHandle]
	if !ok || !current.Ref.SameDialog(ref) {
		return RecoveryToken{}, ErrCallNotFound
	}
	if current.Ref != ref {
		return RecoveryToken{}, ErrStaleRevision
	}
	encoded := strings.Join([]string{
		"emu1",
		base64.RawURLEncoding.EncodeToString([]byte(ref.GatewayID)),
		base64.RawURLEncoding.EncodeToString([]byte(ref.BootEpoch)),
		base64.RawURLEncoding.EncodeToString([]byte(ref.ProviderHandle)),
	}, ".")
	var token RecoveryToken
	if err := token.UnmarshalBinary([]byte(encoded)); err != nil {
		return RecoveryToken{}, err
	}
	return token, nil
}

// InspectRecovery performs a read-only lookup through a previously issued
// synthetic recovery token. It never adopts a call, changes its revision, or
// emits an observation.
func (e *Emulator) InspectRecovery(ctx context.Context, token RecoveryToken) (RecoverySnapshot, error) {
	if err := contextError(ctx); err != nil {
		return RecoverySnapshot{}, err
	}
	encoded, err := token.MarshalBinary()
	if err != nil {
		return RecoverySnapshot{}, err
	}
	parts := strings.Split(string(encoded), ".")
	if len(parts) != 4 || parts[0] != "emu1" {
		return RecoverySnapshot{}, ErrInvalidIdentity
	}
	decoded := make([]string, 3)
	for index := range decoded {
		value, decodeErr := base64.RawURLEncoding.DecodeString(parts[index+1])
		if decodeErr != nil {
			return RecoverySnapshot{}, ErrInvalidIdentity
		}
		decoded[index] = string(value)
	}
	if !validToken(decoded[0], 1, 64) || !validOpaque(decoded[1], 8, 128) ||
		!validOpaque(decoded[2], 1, MaxProviderHandleBytes) {
		return RecoverySnapshot{}, ErrInvalidIdentity
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return RecoverySnapshot{}, ErrClosed
	}
	if decoded[0] != e.gatewayID || decoded[1] != e.bootEpoch {
		return RecoverySnapshot{}, ErrBootEpochMismatch
	}
	current, ok := e.calls[decoded[2]]
	if !ok || current.Ref.GatewayID != decoded[0] || current.Ref.BootEpoch != decoded[1] {
		return RecoverySnapshot{}, ErrCallNotFound
	}
	result := RecoverySnapshot{State: current.State}
	if err := result.Validate(); err != nil {
		return RecoverySnapshot{}, err
	}
	return result, nil
}

// PrepareMedia negotiates one configured codec for an exact incoming revision.
func (e *Emulator) PrepareMedia(ctx context.Context, request PrepareMediaRequest) (MediaSession, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := request.Call.Validate(); err != nil {
		return nil, err
	}
	if err := validateRequestedCodecs(request.Codecs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if !e.capabilities.PrepareMedia {
		return nil, ErrCapabilityUnavailable
	}
	current, ok := e.calls[request.Call.ProviderHandle]
	if !ok || !current.Ref.SameDialog(request.Call) {
		return nil, ErrCallNotFound
	}
	if current.Ref != request.Call {
		return nil, ErrStaleRevision
	}
	if current.State != ProviderCallIncoming {
		return nil, ErrWrongCallPhase
	}
	codec, ok := negotiateCodec(request.Codecs, e.capabilities.Codecs)
	if !ok {
		return nil, ErrCodecMismatch
	}
	e.mediaCounter++
	leaseID := fmt.Sprintf("synthetic-media-%d", e.mediaCounter)
	media := newEmulatedMedia(leaseID, request.Call, codec, e.mediaCapacity, e.now)
	e.media[leaseID] = media
	return media, nil
}

// AnswerIncoming applies, rejects, or obscures one exact answer command.
func (e *Emulator) AnswerIncoming(ctx context.Context, request AnswerIncomingRequest) (CommandResult, error) {
	fingerprint := emulatorCommandFingerprint{
		kind: CommandAnswerIncoming, call: request.Call, mediaLeaseID: request.MediaLeaseID,
	}
	return e.executeCommand(ctx, request.CommandID, fingerprint)
}

// EndActive applies, rejects, or obscures one exact active-dialog end command.
func (e *Emulator) EndActive(ctx context.Context, request EndActiveRequest) (CommandResult, error) {
	fingerprint := emulatorCommandFingerprint{kind: CommandEndActive, call: request.Call}
	return e.executeCommand(ctx, request.CommandID, fingerprint)
}

func (e *Emulator) executeCommand(
	ctx context.Context,
	commandID string,
	fingerprint emulatorCommandFingerprint,
) (CommandResult, error) {
	if err := contextError(ctx); err != nil {
		return CommandResult{}, err
	}
	if !validCommandID(commandID) || fingerprint.call.Validate() != nil {
		return CommandResult{}, ErrInvalidCommand
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return CommandResult{}, ErrClosed
	}
	if existing, ok := e.commands[commandID]; ok {
		if existing.fingerprint != fingerprint {
			return CommandResult{}, ErrCommandConflict
		}
		return cloneCommandResult(existing.result), nil
	}
	current, ok := e.calls[fingerprint.call.ProviderHandle]
	if !ok || !current.Ref.SameDialog(fingerprint.call) {
		return CommandResult{}, ErrCallNotFound
	}
	if current.Ref != fingerprint.call {
		return CommandResult{}, ErrStaleRevision
	}
	switch fingerprint.kind {
	case CommandAnswerIncoming:
		if !e.capabilities.AnswerIncoming {
			return CommandResult{}, ErrCapabilityUnavailable
		}
		if current.State != ProviderCallIncoming {
			return CommandResult{}, ErrWrongCallPhase
		}
		media := e.media[fingerprint.mediaLeaseID]
		if media == nil || media.CallRef() != fingerprint.call || !media.Snapshot().Prepared || media.Snapshot().Closed {
			return CommandResult{}, ErrMediaLeaseMismatch
		}
	case CommandEndActive:
		if !e.capabilities.EndActive {
			return CommandResult{}, ErrCapabilityUnavailable
		}
		if current.State != ProviderCallActive {
			return CommandResult{}, ErrWrongCallPhase
		}
	default:
		return CommandResult{}, ErrInvalidCommand
	}

	fault, hasFault := e.faults[fingerprint.kind]
	if hasFault {
		delete(e.faults, fingerprint.kind)
	} else {
		fault = CommandFault{Outcome: CommandApplied, Apply: true, EmitEvent: true}
	}
	e.executions[fingerprint.kind]++
	result := CommandResult{CommandID: commandID, Outcome: fault.Outcome}
	switch fault.Outcome {
	case CommandApplied:
		current = e.applyCommandLocked(fingerprint.kind, current)
		result.Current = cloneCallSnapshot(current)
		if fault.EmitEvent {
			// The direct result remains authoritative if the observation queue is
			// deliberately saturated; Inspect provides later reconciliation.
			_ = e.enqueueCallLocked(current)
		}
	case CommandRejected:
		result.Current = cloneCallSnapshot(current)
	case CommandUnknown:
		if fault.Apply {
			current = e.applyCommandLocked(fingerprint.kind, current)
			if fault.EmitEvent {
				_ = e.enqueueCallLocked(current)
			}
		}
	default:
		return CommandResult{}, ErrInvalidCommand
	}
	e.commands[commandID] = emulatorCommandRecord{fingerprint: fingerprint, result: cloneCommandResult(result)}
	return cloneCommandResult(result), nil
}

func (e *Emulator) applyCommandLocked(kind CommandKind, current CallSnapshot) CallSnapshot {
	current.Ref.Revision++
	if kind == CommandAnswerIncoming {
		current.State = ProviderCallActive
	} else {
		current.State = ProviderCallEnded
		for _, media := range e.media {
			if media.CallRef().SameDialog(current.Ref) {
				_ = media.Close()
			}
		}
	}
	e.calls[current.Ref.ProviderHandle] = current
	return current
}

// Incoming scripts a new authoritative incoming dialog and queues its event.
func (e *Emulator) Incoming(providerHandle string) (CallRef, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return CallRef{}, ErrClosed
	}
	ref := CallRef{
		GatewayID: e.gatewayID, BootEpoch: e.bootEpoch,
		ProviderHandle: providerHandle, Revision: 1,
	}
	if err := ref.Validate(); err != nil {
		return CallRef{}, err
	}
	// A provider handle is unique for the complete boot epoch. Allowing reuse
	// after Ended would manufacture a same-epoch dialog ABA.
	if _, exists := e.calls[providerHandle]; exists {
		return CallRef{}, ErrConcurrentCall
	}
	snapshot := CallSnapshot{Ref: ref, State: ProviderCallIncoming}
	e.calls[providerHandle] = snapshot
	if err := e.enqueueCallLocked(snapshot); err != nil {
		delete(e.calls, providerHandle)
		return CallRef{}, err
	}
	return ref, nil
}

// Cancel scripts a remote cancellation of an exact incoming revision.
func (e *Emulator) Cancel(ref CallRef) (CallRef, error) {
	return e.transition(ref, ProviderCallIncoming, ProviderCallEnded)
}

// ActivateExternal scripts an answer outside Coordinator.
func (e *Emulator) ActivateExternal(ref CallRef) (CallRef, error) {
	return e.transition(ref, ProviderCallIncoming, ProviderCallActive)
}

// EndExternal scripts a remote end of an exact active revision.
func (e *Emulator) EndExternal(ref CallRef) (CallRef, error) {
	return e.transition(ref, ProviderCallActive, ProviderCallEnded)
}

func (e *Emulator) transition(ref CallRef, from, to ProviderCallState) (CallRef, error) {
	if err := ref.Validate(); err != nil {
		return CallRef{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return CallRef{}, ErrClosed
	}
	current, ok := e.calls[ref.ProviderHandle]
	if !ok || current.Ref != ref {
		return CallRef{}, ErrStaleRevision
	}
	if current.State != from {
		return CallRef{}, ErrWrongCallPhase
	}
	if len(e.events) == cap(e.events) {
		return CallRef{}, ErrObservationQueueFull
	}
	current.Ref.Revision++
	current.State = to
	e.calls[ref.ProviderHandle] = current
	if to == ProviderCallEnded {
		for _, media := range e.media {
			if media.CallRef().SameDialog(current.Ref) {
				_ = media.Close()
			}
		}
	}
	if err := e.enqueueCallLocked(current); err != nil {
		return CallRef{}, err
	}
	return current.Ref, nil
}

// Restart changes the boot epoch, invalidates every dialog/media session, and
// queues a restart event before any subsequent call event.
func (e *Emulator) Restart(bootEpoch string) error {
	if !validOpaque(bootEpoch, 8, 128) {
		return ErrInvalidIdentity
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if _, seen := e.seenBootEpochs[bootEpoch]; seen {
		e.mu.Unlock()
		return ErrBootEpochMismatch
	}
	if len(e.seenBootEpochs) >= e.maxBootEpochs {
		e.mu.Unlock()
		return ErrBootEpochStoreFull
	}
	// Check capacity before changing authoritative epoch/call state. Observe may
	// concurrently drain the channel, which can only make this conservative.
	if len(e.events) == cap(e.events) {
		e.mu.Unlock()
		return ErrObservationQueueFull
	}
	media := make([]*emulatedMedia, 0, len(e.media))
	for _, session := range e.media {
		media = append(media, session)
	}
	e.bootEpoch = bootEpoch
	e.seenBootEpochs[bootEpoch] = struct{}{}
	e.calls = make(map[string]CallSnapshot)
	e.media = make(map[string]*emulatedMedia)
	event := Event{
		Kind: EventGatewayRestart, GatewayID: e.gatewayID,
		BootEpoch: bootEpoch, ObservedAt: e.now(),
	}
	err := e.enqueueEventLocked(event)
	e.mu.Unlock()
	for _, session := range media {
		_ = session.Close()
	}
	return err
}

// InjectEvent queues a duplicate or reordered observation without changing
// authoritative state returned by Inspect.
func (e *Emulator) InjectEvent(event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return e.enqueueEventLocked(event)
}

// SetNextCommandFault installs one fault for the next command of kind.
func (e *Emulator) SetNextCommandFault(kind CommandKind, fault CommandFault) error {
	if kind != CommandAnswerIncoming && kind != CommandEndActive {
		return ErrInvalidCommand
	}
	if fault.Outcome != CommandApplied && fault.Outcome != CommandRejected && fault.Outcome != CommandUnknown {
		return ErrInvalidCommand
	}
	if fault.Outcome == CommandRejected && fault.Apply {
		return ErrInvalidCommand
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	e.faults[kind] = fault
	return nil
}

// CommandExecutions counts first executions; exact idempotent replays do not
// advance it.
func (e *Emulator) CommandExecutions(kind CommandKind) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executions[kind]
}

// MediaSession returns a synthetic media handle for local acceptance tests.
func (e *Emulator) MediaSession(leaseID string) (MediaSession, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	media, ok := e.media[leaseID]
	return media, ok
}

// InjectGatewayFrame queues one synthetic gateway-to-client frame.
func (e *Emulator) InjectGatewayFrame(leaseID string, frame MediaFrame) error {
	e.mu.Lock()
	media := e.media[leaseID]
	e.mu.Unlock()
	if media == nil {
		return ErrMediaLeaseMismatch
	}
	return media.injectGatewayFrame(frame)
}

func (e *Emulator) enqueueCallLocked(snapshot CallSnapshot) error {
	event := Event{Kind: EventCallChanged, Call: cloneCallSnapshot(snapshot), ObservedAt: e.now()}
	return e.enqueueEventLocked(event)
}

func (e *Emulator) enqueueEventLocked(event Event) error {
	select {
	case e.events <- cloneEvent(event):
		return nil
	default:
		return ErrObservationQueueFull
	}
}

// Close terminates observations and every synthetic media session.
func (e *Emulator) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		media := make([]*emulatedMedia, 0, len(e.media))
		for _, session := range e.media {
			media = append(media, session)
		}
		e.mu.Unlock()
		close(e.done)
		for _, session := range media {
			_ = session.Close()
		}
	})
	return nil
}

type queuedMediaFrame struct {
	frame MediaFrame
	epoch uint64
	at    time.Time
}

type emulatedMedia struct {
	id        string
	call      CallRef
	codec     Codec
	now       func() time.Time
	frames    chan queuedMediaFrame
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	snapshot MediaSnapshot
}

func newEmulatedMedia(
	id string,
	call CallRef,
	codec Codec,
	capacity int,
	now func() time.Time,
) *emulatedMedia {
	return &emulatedMedia{
		id: id, call: call, codec: codec, now: now,
		frames: make(chan queuedMediaFrame, capacity), done: make(chan struct{}),
		snapshot: MediaSnapshot{LeaseID: id, Call: call, Codec: codec, Prepared: true},
	}
}

func (m *emulatedMedia) ID() string       { return m.id }
func (m *emulatedMedia) CallRef() CallRef { return m.call }
func (m *emulatedMedia) Codec() Codec     { return m.codec }

func (m *emulatedMedia) ReadGatewayFrame(ctx context.Context) (MediaFrame, error) {
	if err := contextError(ctx); err != nil {
		return MediaFrame{}, err
	}
	select {
	case <-ctx.Done():
		return MediaFrame{}, ctx.Err()
	case <-m.done:
		return MediaFrame{}, ErrMediaClosed
	case queued := <-m.frames:
		m.mu.Lock()
		if m.snapshot.Closed {
			m.mu.Unlock()
			return MediaFrame{}, ErrMediaClosed
		}
		m.snapshot.GatewayToClientFrames++
		m.snapshot.LastGatewayToClientAt = queued.at
		m.snapshot.LastGatewayToClientEpoch = queued.epoch
		m.mu.Unlock()
		return cloneMediaFrame(queued.frame), nil
	}
}

func (m *emulatedMedia) WriteGatewayFrame(ctx context.Context, frame MediaFrame) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return ErrMediaClosed
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Closed {
		return ErrMediaClosed
	}
	m.snapshot.ClientToGatewayFrames++
	m.snapshot.LastClientToGatewayAt = m.now()
	m.snapshot.LastClientToGatewayEpoch = m.snapshot.ActivationEpoch
	return nil
}

func (m *emulatedMedia) injectGatewayFrame(frame MediaFrame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.snapshot.Closed {
		m.mu.Unlock()
		return ErrMediaClosed
	}
	epoch := m.snapshot.ActivationEpoch
	m.mu.Unlock()
	queued := queuedMediaFrame{frame: cloneMediaFrame(frame), epoch: epoch, at: m.now()}
	select {
	case <-m.done:
		return ErrMediaClosed
	case m.frames <- queued:
		m.mu.Lock()
		closed := m.snapshot.Closed
		m.mu.Unlock()
		if closed {
			return ErrMediaClosed
		}
		return nil
	default:
		return ErrMediaQueueFull
	}
}

func (m *emulatedMedia) Activate(epoch uint64) (MediaSnapshot, error) {
	if epoch == 0 {
		return MediaSnapshot{}, ErrInvalidIdentity
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Closed {
		return MediaSnapshot{}, ErrMediaClosed
	}
	if m.snapshot.Activated {
		if m.snapshot.ActivationEpoch != epoch {
			return MediaSnapshot{}, ErrStaleRevision
		}
		return m.snapshot, nil
	}
	m.snapshot.Activated = true
	m.snapshot.ActivationEpoch = epoch
	m.snapshot.ActivatedAt = m.now()
	m.snapshot.GatewayToClientBaseline = m.snapshot.GatewayToClientFrames
	m.snapshot.ClientToGatewayBaseline = m.snapshot.ClientToGatewayFrames
	return m.snapshot, nil
}

func (m *emulatedMedia) Snapshot() MediaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

func (m *emulatedMedia) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.snapshot.Closed = true
		m.mu.Unlock()
		close(m.done)
	})
	return nil
}

func negotiateCodec(requested, supported []Codec) (Codec, bool) {
	available := make(map[Codec]struct{}, len(supported))
	for _, codec := range supported {
		available[codec] = struct{}{}
	}
	for _, codec := range requested {
		if _, ok := available[codec]; ok {
			return codec, true
		}
	}
	return Codec{}, false
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sipgateway: context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func cloneEvent(event Event) Event {
	copy := event
	if event.Call != nil {
		copy.Call = cloneCallSnapshot(*event.Call)
	}
	return copy
}

func cloneCommandResult(result CommandResult) CommandResult {
	copy := result
	if result.Current != nil {
		copy.Current = cloneCallSnapshot(*result.Current)
	}
	return copy
}

func cloneMediaFrame(frame MediaFrame) MediaFrame {
	return MediaFrame{Sequence: frame.Sequence, Payload: append([]byte(nil), frame.Payload...)}
}
