package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	externalVoiceDefaultReconnectMin = time.Second
	externalVoiceDefaultReconnectMax = 30 * time.Second
)

// externalVoiceService is the narrow boundary consumed by the v2 HTTP API.
// Both the single-incarnation runtime used by tests and the reconnecting
// production supervisor implement it without exposing adapter credentials.
type externalVoiceService interface {
	Status(identity string) externalVoiceStatus
	Offer(context.Context, string, externalVoiceOfferRequest, [32]byte) (externalVoiceOfferResult, error)
	Answer(context.Context, string, string, externalVoiceCallRequest, [sha256.Size]byte) (sipgateway.MutationResult, error)
	End(context.Context, string, string, externalVoiceCallRequest, [sha256.Size]byte) (sipgateway.MutationResult, error)
	Reconcile(context.Context, string, externalVoiceCallRequest, bool, bool) (sipgateway.PublicCallSnapshot, error)
	Close() error
}

type externalVoiceDialService interface {
	externalVoiceService
	Dial(context.Context, string, string, externalVoiceDialRequest, [32]byte) (sipgateway.PublicCallSnapshot, error)
}

type externalVoiceControlService interface {
	externalVoiceService
	Reject(context.Context, string, string, externalVoiceRejectRequest, [sha256.Size]byte) (sipgateway.MutationResult, error)
	DTMF(context.Context, string, string, externalVoiceDTMFRequest, [sha256.Size]byte) (sipgateway.MutationResult, error)
}

type sipVoiceSupervisorConfig struct {
	Runtime                  sipVoiceRuntimeConfig
	AdapterFactory           func(context.Context) (sipgateway.Adapter, error)
	RecoveryInspectorFactory func(context.Context) (sipgateway.RecoveryInspector, error)
	RecoveryStore            *externalVoiceRecoveryStore
	RecoveryJournal          *externalVoiceMutationJournal
	ReconnectMin             time.Duration
	ReconnectMax             time.Duration
}

func (sipVoiceSupervisorConfig) String() string   { return "sipVoiceSupervisorConfig{redacted}" }
func (sipVoiceSupervisorConfig) GoString() string { return "sipVoiceSupervisorConfig{redacted}" }

type sipVoiceSupervisor struct {
	mu sync.Mutex

	cfg      sipVoiceSupervisorConfig
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	current  *sipVoiceRuntime
	health   string
	recovery bool
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

func (*sipVoiceSupervisor) String() string   { return "sipVoiceSupervisor{redacted}" }
func (*sipVoiceSupervisor) GoString() string { return "sipVoiceSupervisor{redacted}" }

func newSIPVoiceSupervisor(parent context.Context, cfg sipVoiceSupervisorConfig) (*sipVoiceSupervisor, error) {
	if parent == nil || cfg.AdapterFactory == nil || cfg.Runtime.Adapter != nil ||
		cfg.Runtime.GatewayID == "" || cfg.Runtime.AllowEnd && !cfg.Runtime.AllowAnswer ||
		!validExternalVoiceNetworkBase(cfg.Runtime.Network) {
		return nil, errExternalVoiceInvalid
	}
	hasRecoveryComponent := cfg.RecoveryInspectorFactory != nil || cfg.RecoveryStore != nil || cfg.RecoveryJournal != nil
	mutationsEnabled := cfg.Runtime.AllowAnswer || cfg.Runtime.AllowEnd
	if mutationsEnabled && !hasRecoveryComponent {
		return nil, errExternalVoiceInvalid
	}
	if hasRecoveryComponent && (cfg.RecoveryInspectorFactory == nil || cfg.RecoveryStore == nil ||
		cfg.RecoveryJournal == nil || cfg.Runtime.Journal != cfg.RecoveryJournal || cfg.Runtime.Evidence == nil) {
		return nil, errExternalVoiceInvalid
	}
	if mutationsEnabled && (cfg.Runtime.Journal == nil || cfg.Runtime.Evidence == nil) {
		return nil, errExternalVoiceInvalid
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = externalVoiceDefaultReconnectMin
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = externalVoiceDefaultReconnectMax
	}
	if cfg.ReconnectMin < 10*time.Millisecond || cfg.ReconnectMax < cfg.ReconnectMin ||
		cfg.ReconnectMax > 5*time.Minute {
		return nil, errExternalVoiceInvalid
	}
	cfg.Runtime.Network = cloneExternalVoiceNetwork(cfg.Runtime.Network)
	ctx, cancel := context.WithCancel(parent)
	supervisor := &sipVoiceSupervisor{
		cfg: cfg, ctx: ctx, cancel: cancel, done: make(chan struct{}), health: "connecting",
	}
	go supervisor.run()
	return supervisor, nil
}

func (s *sipVoiceSupervisor) run() {
	defer close(s.done)
	backoff := s.cfg.ReconnectMin
	for {
		if s.ctx.Err() != nil {
			return
		}
		if s.cfg.RecoveryStore != nil {
			snapshot, err := s.cfg.RecoveryStore.Snapshot()
			if err != nil {
				s.setRecoveryHealth("manual_recovery_required")
				return
			}
			if snapshot.State == externalVoiceRecoveryArmed {
				resolved, retry := s.inspectArmedRecovery(snapshot)
				zeroExternalVoiceRecoverySnapshot(&snapshot)
				if resolved {
					backoff = s.cfg.ReconnectMin
					continue
				}
				if retry && waitExternalVoiceBackoff(s.ctx, backoff) {
					backoff = min(2*backoff, s.cfg.ReconnectMax)
					continue
				}
				return
			}
			zeroExternalVoiceRecoverySnapshot(&snapshot)
		}
		adapter, err := s.cfg.AdapterFactory(s.ctx)
		if err != nil {
			if errors.Is(err, sipgateway.ErrRecoveryRequired) || errors.Is(err, sipgateway.ErrReconcileRequired) {
				s.setRecoveryHealth("manual_recovery_required")
				return
			}
			s.setDisconnected("reconnecting", false)
			if !waitExternalVoiceBackoff(s.ctx, backoff) {
				return
			}
			backoff = min(2*backoff, s.cfg.ReconnectMax)
			continue
		}
		runtimeConfig := s.cfg.Runtime
		runtimeConfig.Adapter = adapter
		runtime, err := newExternalVoiceRuntime(s.ctx, runtimeConfig)
		if err != nil {
			_ = adapter.Close()
			s.setDisconnected("reconnecting", false)
			if !waitExternalVoiceBackoff(s.ctx, backoff) {
				return
			}
			backoff = min(2*backoff, s.cfg.ReconnectMax)
			continue
		}
		if !s.install(runtime) {
			_ = runtime.Close()
			return
		}
		backoff = s.cfg.ReconnectMin
		select {
		case <-runtime.done:
		case <-s.ctx.Done():
		}
		recovery := runtime.requiresManualRecoveryAfterDisconnect()
		s.detach(runtime, recovery)
		_ = runtime.Close()
		if s.ctx.Err() != nil {
			return
		}
		if recovery {
			return
		}
		if !waitExternalVoiceBackoff(s.ctx, backoff) {
			return
		}
		backoff = min(2*backoff, s.cfg.ReconnectMax)
	}
}

// inspectArmedRecovery is strictly GET-only. It may retire an armed record only
// after the exact provider token proves the dialog ended. Incoming, active,
// identity mismatch, or malformed state remains locked for manual PBX review.
func (s *sipVoiceSupervisor) inspectArmedRecovery(
	snapshot externalVoiceRecoverySnapshot,
) (resolved bool, retry bool) {
	s.setRecoveryHealth("recovery_inspecting")
	if snapshot.State != externalVoiceRecoveryArmed || len(snapshot.ProviderToken) == 0 ||
		s.cfg.RecoveryInspectorFactory == nil || s.cfg.RecoveryStore == nil {
		s.setRecoveryHealth("manual_recovery_required")
		return false, false
	}
	inspector, err := s.cfg.RecoveryInspectorFactory(s.ctx)
	if err != nil {
		if s.ctx.Err() != nil {
			return false, false
		}
		if errors.Is(err, sipgateway.ErrReconcileRequired) || errors.Is(err, sipgateway.ErrRecoveryRequired) {
			s.setRecoveryHealth("manual_recovery_required")
			return false, false
		}
		return false, true
	}
	defer inspector.Close()
	var token sipgateway.RecoveryToken
	if err := token.UnmarshalBinary(snapshot.ProviderToken); err != nil {
		s.setRecoveryHealth("manual_recovery_required")
		return false, false
	}
	result, err := inspector.InspectRecovery(s.ctx, token)
	if err != nil {
		if s.ctx.Err() != nil {
			return false, false
		}
		if errors.Is(err, sipgateway.ErrReconcileRequired) || errors.Is(err, sipgateway.ErrInvalidIdentity) ||
			errors.Is(err, sipgateway.ErrInvalidEvent) {
			s.setRecoveryHealth("manual_recovery_required")
			return false, false
		}
		return false, true
	}
	if err := result.Validate(); err != nil || result.State != sipgateway.ProviderCallEnded {
		s.setRecoveryHealth("manual_recovery_required")
		return false, false
	}
	if _, err := s.cfg.RecoveryStore.Resolve(snapshot.RecoveryID, externalVoiceRecoveryProviderEnded); err != nil {
		s.setRecoveryHealth("manual_recovery_required")
		return false, false
	}
	s.mu.Lock()
	if !s.closed {
		s.recovery = false
		s.health = "connecting"
	}
	s.mu.Unlock()
	return true, false
}

func (s *sipVoiceSupervisor) install(runtime *sipVoiceRuntime) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.recovery {
		return false
	}
	s.current = runtime
	s.health = "connected"
	return true
}

func (s *sipVoiceSupervisor) detach(runtime *sipVoiceRuntime, recovery bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == runtime {
		s.current = nil
	}
	if recovery {
		s.recovery = true
		s.health = "manual_recovery_required"
	} else if !s.closed {
		s.health = "reconnecting"
	}
}

func (s *sipVoiceSupervisor) setDisconnected(health string, recovery bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if recovery {
		s.recovery = true
	}
	if s.recovery {
		s.health = "manual_recovery_required"
	} else {
		s.health = health
	}
}

func (s *sipVoiceSupervisor) setRecoveryHealth(health string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.recovery = true
	s.health = health
}

func (s *sipVoiceSupervisor) Status(identity string) externalVoiceStatus {
	runtime, health, recovery, closed := s.snapshot()
	if runtime == nil {
		if closed {
			health = "closed"
		}
		return externalVoiceStatus{
			Enabled: true, Health: health, RecoveryRequired: recovery,
			AnswerEnabled: s.cfg.Runtime.AllowAnswer, RejectEnabled: s.cfg.Runtime.AllowReject,
			DTMFEnabled: s.cfg.Runtime.AllowDTMF, EndEnabled: s.cfg.Runtime.AllowEnd,
			DialEnabled: s.cfg.Runtime.AllowDial,
		}
	}
	status := runtime.Status(identity)
	status.RecoveryRequired = recovery
	return status
}

func (s *sipVoiceSupervisor) Dial(
	ctx context.Context, identity, commandID string, request externalVoiceDialRequest,
	bodyHash [32]byte,
) (sipgateway.PublicCallSnapshot, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.PublicCallSnapshot{}, err
	}
	return runtime.Dial(ctx, identity, commandID, request, bodyHash)
}

func (s *sipVoiceSupervisor) Offer(
	ctx context.Context,
	identity string,
	request externalVoiceOfferRequest,
	bodyHash [32]byte,
) (externalVoiceOfferResult, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return externalVoiceOfferResult{}, err
	}
	return runtime.Offer(ctx, identity, request, bodyHash)
}

func (s *sipVoiceSupervisor) Answer(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceCallRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.MutationResult{}, err
	}
	return runtime.Answer(ctx, identity, commandID, request, bodyHash)
}

func (s *sipVoiceSupervisor) Reject(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceRejectRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.MutationResult{}, err
	}
	return runtime.Reject(ctx, identity, commandID, request, bodyHash)
}

func (s *sipVoiceSupervisor) DTMF(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceDTMFRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.MutationResult{}, err
	}
	return runtime.DTMF(ctx, identity, commandID, request, bodyHash)
}

func (s *sipVoiceSupervisor) End(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceCallRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.MutationResult{}, err
	}
	return runtime.End(ctx, identity, commandID, request, bodyHash)
}

func (s *sipVoiceSupervisor) Reconcile(
	ctx context.Context,
	identity string,
	request externalVoiceCallRequest,
	allowControl, allowHangup bool,
) (sipgateway.PublicCallSnapshot, error) {
	runtime, err := s.requireRuntime()
	if err != nil {
		return sipgateway.PublicCallSnapshot{}, err
	}
	return runtime.Reconcile(ctx, identity, request, allowControl, allowHangup)
}

func (s *sipVoiceSupervisor) requireRuntime() (*sipVoiceRuntime, error) {
	runtime, _, recovery, closed := s.snapshot()
	if recovery {
		return nil, errExternalVoiceConflict
	}
	if closed {
		return nil, errExternalVoiceClosed
	}
	if runtime == nil {
		return nil, errExternalVoiceUnavailable
	}
	return runtime, nil
}

func (s *sipVoiceSupervisor) snapshot() (*sipVoiceRuntime, string, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.health, s.recovery, s.closed
}

func (s *sipVoiceSupervisor) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.health = "closed"
		runtime := s.current
		s.mu.Unlock()
		s.cancel()
		if runtime != nil {
			s.closeErr = runtime.Close()
		}
		<-s.done
		if s.cfg.RecoveryJournal != nil {
			_ = s.cfg.RecoveryJournal.Close()
		}
		if s.cfg.RecoveryStore != nil {
			if err := s.cfg.RecoveryStore.Close(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

func waitExternalVoiceBackoff(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *sipVoiceRuntime) hasUncertainOutcome() bool {
	// The observer can terminate at the same instant that a provider mutation
	// returns an ambiguous transport result. Drain the serialized operation
	// before deciding whether a new adapter incarnation is safe; otherwise a
	// reconnect could discard the just-published reconciliation authority.
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.uncertainKind != ""
}

func (r *sipVoiceRuntime) requiresManualRecoveryAfterDisconnect() bool {
	// Serialize behind any provider mutation before classifying the failed
	// adapter incarnation. Reconnecting while a ringing or active dialog may
	// still exist would create a second controller without provider continuity.
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.uncertainKind != "" || r.recoveryRequired {
		return !r.closed && (r.uncertainKind != "" || r.recoveryRequired)
	}
	return r.current != nil && r.current.Phase != sipgateway.PhaseEnded
}

var _ externalVoiceService = (*sipVoiceRuntime)(nil)
var _ externalVoiceDialService = (*sipVoiceRuntime)(nil)
var _ externalVoiceService = (*sipVoiceSupervisor)(nil)
var _ externalVoiceDialService = (*sipVoiceSupervisor)(nil)
