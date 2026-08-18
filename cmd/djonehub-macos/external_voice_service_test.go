package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

type syntheticExternalVoiceRecoveryInspector struct {
	state sipgateway.ProviderCallState
	err   error
	calls atomic.Int32
}

type recoveryRequiredSIPAdapter struct{ *sipgateway.Emulator }

func (*recoveryRequiredSIPAdapter) Observe(context.Context) (sipgateway.Event, error) {
	return sipgateway.Event{}, sipgateway.ErrRecoveryRequired
}

func (i *syntheticExternalVoiceRecoveryInspector) InspectRecovery(
	ctx context.Context,
	_ sipgateway.RecoveryToken,
) (sipgateway.RecoverySnapshot, error) {
	i.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return sipgateway.RecoverySnapshot{}, err
	}
	return sipgateway.RecoverySnapshot{State: i.state}, i.err
}

func (*syntheticExternalVoiceRecoveryInspector) Close() error { return nil }

func TestSIPVoiceSupervisorReconnectsAfterProviderFailure(t *testing.T) {
	var attempts atomic.Int32
	adapters := make(chan *sipgateway.Emulator, 4)
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(context.Context) (sipgateway.Adapter, error) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			return nil, errors.New("synthetic provider unavailable")
		}
		emulator := newSyntheticSIPVoiceEmulator(t, fmt.Sprintf("synthetic-boot-epoch-%d", attempt))
		adapters <- emulator
		return emulator, nil
	})
	first := waitSyntheticSupervisorAdapter(t, adapters)
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return (status.Health == "starting" || status.Health == "connected") && status.Call == nil
	})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := waitSyntheticSupervisorAdapter(t, adapters)
	if second == first {
		t.Fatal("supervisor reused a closed adapter incarnation")
	}
	if _, err := second.Incoming("synthetic-provider-dialog-2"); err != nil {
		t.Fatal(err)
	}
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.Health == "connected" && status.Call != nil &&
			status.Call.Phase == sipgateway.PhaseIncomingRinging
	})
	if got := attempts.Load(); got < 3 {
		t.Fatalf("adapter factory attempts=%d", got)
	}
}

func TestSIPVoiceSupervisorQuarantinesRingingCallAfterProviderDisconnect(t *testing.T) {
	var attempts atomic.Int32
	adapters := make(chan *sipgateway.Emulator, 2)
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(context.Context) (sipgateway.Adapter, error) {
		attempt := attempts.Add(1)
		emulator := newSyntheticSIPVoiceEmulator(t, fmt.Sprintf("synthetic-boot-live-%d", attempt))
		adapters <- emulator
		return emulator, nil
	})
	adapter := waitSyntheticSupervisorAdapter(t, adapters)
	if _, err := adapter.Incoming("synthetic-provider-dialog-live"); err != nil {
		t.Fatal(err)
	}
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.Call != nil && status.Call.Phase == sipgateway.PhaseIncomingRinging
	})
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	status := waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.RecoveryRequired && status.Health == "manual_recovery_required"
	})
	if status.Call != nil {
		t.Fatalf("quarantine exposed stale call: %+v", status.Call)
	}
	time.Sleep(4 * externalVoiceTestReconnectDelay)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("supervisor reconnected across a possibly live call: attempts=%d", got)
	}
}

func TestSIPVoiceSupervisorQuarantinesApplicationOwnershipLossWithoutKnownCall(t *testing.T) {
	var attempts atomic.Int32
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(context.Context) (sipgateway.Adapter, error) {
		attempts.Add(1)
		return &recoveryRequiredSIPAdapter{
			Emulator: newSyntheticSIPVoiceEmulator(t, "synthetic-ownership-loss-boot"),
		}, nil
	})
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.RecoveryRequired && status.Health == "manual_recovery_required"
	})
	time.Sleep(4 * externalVoiceTestReconnectDelay)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("supervisor reconnected after ARI application ownership loss: %d", got)
	}
}

func TestSIPVoiceSupervisorRestartRecoveryIsReadOnlyAndRequiresEnded(t *testing.T) {
	for _, test := range []struct {
		name           string
		state          sipgateway.ProviderCallState
		inspectErr     error
		wantAdapter    bool
		wantResolution externalVoiceRecoveryResolution
		wantHealth     string
	}{
		{
			name:  "exact ended retires recovery before normal startup",
			state: sipgateway.ProviderCallEnded, wantAdapter: true,
			wantResolution: externalVoiceRecoveryProviderEnded,
		},
		{
			name:  "active remains manually locked",
			state: sipgateway.ProviderCallActive, wantHealth: "manual_recovery_required",
		},
		{
			name:  "incoming remains manually locked",
			state: sipgateway.ProviderCallIncoming, wantHealth: "manual_recovery_required",
		},
		{
			name:       "identity mismatch remains manually locked",
			inspectErr: sipgateway.ErrReconcileRequired, wantHealth: "manual_recovery_required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "external-voice", "recovery.json")
			store, err := openExternalVoiceRecoveryStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := newExternalVoiceMutationJournal(
				store,
				"synthetic-home-gateway",
				bytes.NewReader(bytes.Repeat([]byte{0x51}, externalVoiceRecoveryIDBytes)),
			)
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			evidence, err := journal.Evidence(
				sipgateway.CommandAnswerIncoming,
				"synthetic-owner@example.invalid",
				sha256.Sum256([]byte("synthetic durable answer body")),
			)
			if err != nil {
				t.Fatal(err)
			}
			var token sipgateway.RecoveryToken
			if err := token.UnmarshalBinary([]byte("synthetic-provider-recovery-token")); err != nil {
				t.Fatal(err)
			}
			recoveryID, err := journal.Arm(sipgateway.PendingMutation{
				Kind:               sipgateway.CommandAnswerIncoming,
				CommandID:          "synthetic-restart-command",
				Call:               sipgateway.PublicCallRef{PublicCallID: strings.Repeat("r", 43), Generation: 9},
				ExpectedRevision:   17,
				PublicMediaLeaseID: "pml_synthetic_restart",
				ProviderToken:      token,
				Evidence:           evidence,
			})
			if err != nil || recoveryID == "" {
				t.Fatalf("arm recoveryID=%q err=%v", recoveryID, err)
			}
			inspector := &syntheticExternalVoiceRecoveryInspector{state: test.state, err: test.inspectErr}
			var adapterAttempts atomic.Int32
			adapters := make(chan *sipgateway.Emulator, 1)
			supervisor, err := newSIPVoiceSupervisor(context.Background(), sipVoiceSupervisorConfig{
				Runtime: syntheticSIPVoiceRuntimeConfig(journal),
				AdapterFactory: func(context.Context) (sipgateway.Adapter, error) {
					adapterAttempts.Add(1)
					emulator := newSyntheticSIPVoiceEmulator(t, "synthetic-recovered-boot")
					adapters <- emulator
					return emulator, nil
				},
				RecoveryInspectorFactory: func(context.Context) (sipgateway.RecoveryInspector, error) {
					return inspector, nil
				},
				RecoveryStore: store, RecoveryJournal: journal,
				ReconnectMin: externalVoiceTestReconnectDelay,
				ReconnectMax: 2 * externalVoiceTestReconnectDelay,
			})
			if err != nil {
				_ = journal.Close()
				_ = store.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = supervisor.Close() })
			if test.wantAdapter {
				_ = waitSyntheticSupervisorAdapter(t, adapters)
				snapshot, err := store.Snapshot()
				if err != nil || snapshot.State != externalVoiceRecoveryResolved ||
					snapshot.Resolution != test.wantResolution {
					t.Fatalf("recovery snapshot=%+v err=%v", snapshot, err)
				}
			} else {
				waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
					return status.RecoveryRequired && status.Health == test.wantHealth
				})
				time.Sleep(4 * externalVoiceTestReconnectDelay)
				if got := adapterAttempts.Load(); got != 0 {
					t.Fatalf("normal adapter started while recovery remained unresolved: %d", got)
				}
				snapshot, err := store.Snapshot()
				if err != nil || snapshot.State != externalVoiceRecoveryArmed || snapshot.RecoveryID != recoveryID {
					t.Fatalf("armed recovery was changed: %+v err=%v", snapshot, err)
				}
			}
			if got := inspector.calls.Load(); got != 1 {
				t.Fatalf("recovery inspections=%d", got)
			}
		})
	}
}

func TestSIPVoiceSupervisorRetriesTransientRecoveryInspectionReadOnly(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "external-voice", "recovery.json")
	store, err := openExternalVoiceRecoveryStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newExternalVoiceMutationJournal(
		store, "synthetic-home-gateway",
		bytes.NewReader(bytes.Repeat([]byte{0x61}, externalVoiceRecoveryIDBytes)),
	)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	evidence, err := journal.Evidence(
		sipgateway.CommandAnswerIncoming, "synthetic-owner@example.invalid",
		sha256.Sum256([]byte("synthetic transient recovery body")),
	)
	if err != nil {
		t.Fatal(err)
	}
	var token sipgateway.RecoveryToken
	if err := token.UnmarshalBinary([]byte("synthetic-transient-recovery-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Arm(sipgateway.PendingMutation{
		Kind: sipgateway.CommandAnswerIncoming, CommandID: "synthetic-transient-command",
		Call:             sipgateway.PublicCallRef{PublicCallID: strings.Repeat("t", 43), Generation: 7},
		ExpectedRevision: 11, PublicMediaLeaseID: "pml_synthetic_transient",
		ProviderToken: token, Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	var inspections atomic.Int32
	adapters := make(chan *sipgateway.Emulator, 1)
	supervisor, err := newSIPVoiceSupervisor(context.Background(), sipVoiceSupervisorConfig{
		Runtime: syntheticSIPVoiceRuntimeConfig(journal),
		AdapterFactory: func(context.Context) (sipgateway.Adapter, error) {
			emulator := newSyntheticSIPVoiceEmulator(t, "synthetic-transient-recovered-boot")
			adapters <- emulator
			return emulator, nil
		},
		RecoveryInspectorFactory: func(context.Context) (sipgateway.RecoveryInspector, error) {
			if inspections.Add(1) == 1 {
				return nil, errors.New("synthetic transient transport failure")
			}
			return &syntheticExternalVoiceRecoveryInspector{state: sipgateway.ProviderCallEnded}, nil
		},
		RecoveryStore: store, RecoveryJournal: journal,
		ReconnectMin: externalVoiceTestReconnectDelay,
		ReconnectMax: 2 * externalVoiceTestReconnectDelay,
	})
	if err != nil {
		_ = journal.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	_ = waitSyntheticSupervisorAdapter(t, adapters)
	if inspections.Load() != 2 {
		t.Fatalf("recovery inspections=%d, want 2", inspections.Load())
	}
}

func TestSIPVoiceSupervisorResolveFailureStaysManuallyLocked(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "external-voice", "recovery.json")
	store, err := openExternalVoiceRecoveryStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newExternalVoiceMutationJournal(
		store, "synthetic-home-gateway",
		bytes.NewReader(bytes.Repeat([]byte{0x71}, externalVoiceRecoveryIDBytes)),
	)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	evidence, err := journal.Evidence(
		sipgateway.CommandEndActive, "synthetic-owner@example.invalid",
		sha256.Sum256([]byte("synthetic resolve failure body")),
	)
	if err != nil {
		t.Fatal(err)
	}
	var token sipgateway.RecoveryToken
	if err := token.UnmarshalBinary([]byte("synthetic-resolve-failure-token")); err != nil {
		t.Fatal(err)
	}
	recoveryID, err := journal.Arm(sipgateway.PendingMutation{
		Kind: sipgateway.CommandEndActive, CommandID: "synthetic-resolve-failure-command",
		Call:             sipgateway.PublicCallRef{PublicCallID: strings.Repeat("u", 43), Generation: 8},
		ExpectedRevision: 12, PublicMediaLeaseID: "pml_synthetic_resolve_failure",
		ProviderToken: token, Evidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.io.syncFile = func(*os.File) error { return errors.New("synthetic resolve sync failure") }
	var adapterAttempts atomic.Int32
	supervisor, err := newSIPVoiceSupervisor(context.Background(), sipVoiceSupervisorConfig{
		Runtime: syntheticSIPVoiceRuntimeConfig(journal),
		AdapterFactory: func(context.Context) (sipgateway.Adapter, error) {
			adapterAttempts.Add(1)
			return newSyntheticSIPVoiceEmulator(t, "must-not-start"), nil
		},
		RecoveryInspectorFactory: func(context.Context) (sipgateway.RecoveryInspector, error) {
			return &syntheticExternalVoiceRecoveryInspector{state: sipgateway.ProviderCallEnded}, nil
		},
		RecoveryStore: store, RecoveryJournal: journal,
		ReconnectMin: externalVoiceTestReconnectDelay,
		ReconnectMax: 2 * externalVoiceTestReconnectDelay,
	})
	if err != nil {
		_ = journal.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.RecoveryRequired && status.Health == "manual_recovery_required"
	})
	if adapterAttempts.Load() != 0 {
		t.Fatalf("normal adapter starts=%d", adapterAttempts.Load())
	}
	if _, err := store.Snapshot(); !errors.Is(err, errExternalVoiceRecoveryStoreFailed) {
		t.Fatalf("failed store Snapshot error=%v", err)
	}
	_ = supervisor.Close()
	reopened, err := openExternalVoiceRecoveryStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	if err != nil || snapshot.State != externalVoiceRecoveryArmed || snapshot.RecoveryID != recoveryID {
		t.Fatalf("reopened snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSIPVoiceSupervisorCloseCancelsBlockedFactory(t *testing.T) {
	entered := make(chan struct{})
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(ctx context.Context) (sipgateway.Adapter, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("adapter factory did not start")
	}
	done := make(chan error, 1)
	go func() { done <- supervisor.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor Close did not cancel adapter construction")
	}
}

func TestSIPVoiceSupervisorLatchesUnknownOutcomeAcrossDisconnect(t *testing.T) {
	var attempts atomic.Int32
	adapters := make(chan *sipgateway.Emulator, 2)
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(context.Context) (sipgateway.Adapter, error) {
		attempts.Add(1)
		emulator := newSyntheticSIPVoiceEmulator(t, "synthetic-boot-epoch-recovery")
		adapters <- emulator
		return emulator, nil
	})
	emulator := waitSyntheticSupervisorAdapter(t, adapters)
	if _, err := emulator.Incoming("synthetic-provider-dialog-recovery"); err != nil {
		t.Fatal(err)
	}
	incoming := waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.Call != nil && status.Call.Phase == sipgateway.PhaseIncomingRinging
	})
	offer, err := supervisor.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(*incoming.Call, "synthetic_nonce_supervisor_1"),
		sha256.Sum256([]byte("synthetic-supervisor-offer-body")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	if err := emulator.SetNextCommandFault(sipgateway.CommandAnswerIncoming, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: false,
	}); err != nil {
		t.Fatal(err)
	}
	request := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	if _, err := supervisor.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-supervisor-answer-command", request,
		sha256.Sum256([]byte("synthetic-supervisor-answer-body"))); !errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		t.Fatalf("answer error=%v", err)
	}
	if err := emulator.Close(); err != nil {
		t.Fatal(err)
	}
	recovery := waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.RecoveryRequired && status.Health == "manual_recovery_required"
	})
	if recovery.Call != nil {
		t.Fatalf("manual recovery exposed a stale current call: %+v", recovery.Call)
	}
	if _, err := supervisor.Reconcile(context.Background(), "synthetic-owner@example.invalid",
		request, true, false); !errors.Is(err, errExternalVoiceConflict) {
		t.Fatalf("reconcile after lost process-local correlation error=%v", err)
	}
	time.Sleep(4 * externalVoiceTestReconnectDelay)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("supervisor reconnected across unresolved outcome: attempts=%d", got)
	}
}

func TestSIPVoiceSupervisorDrainsConcurrentUnknownBeforeReconnect(t *testing.T) {
	var attempts atomic.Int32
	adapters := make(chan *blockingUnknownSIPAdapter, 2)
	supervisor := newSyntheticSIPVoiceSupervisor(t, func(context.Context) (sipgateway.Adapter, error) {
		attempts.Add(1)
		adapter := &blockingUnknownSIPAdapter{
			Emulator: newSyntheticSIPVoiceEmulator(t, "synthetic-boot-epoch-concurrent"),
			entered:  make(chan struct{}), release: make(chan struct{}),
		}
		adapters <- adapter
		return adapter, nil
	})
	adapter := <-adapters
	if _, err := adapter.Incoming("synthetic-provider-dialog-concurrent"); err != nil {
		t.Fatal(err)
	}
	incoming := waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.Call != nil && status.Call.Phase == sipgateway.PhaseIncomingRinging
	})
	offer, err := supervisor.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(*incoming.Call, "synthetic_nonce_supervisor_2"),
		sha256.Sum256([]byte("synthetic-supervisor-concurrent-offer")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	request := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	answerDone := make(chan error, 1)
	go func() {
		_, answerErr := supervisor.Answer(context.Background(), "synthetic-owner@example.invalid",
			"synthetic-supervisor-concurrent-command", request,
			sha256.Sum256([]byte("synthetic-supervisor-concurrent-answer-body")))
		answerDone <- answerErr
	}()
	select {
	case <-adapter.entered:
	case <-time.After(time.Second):
		t.Fatal("provider command did not enter the deterministic race barrier")
	}
	if err := adapter.Emulator.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * externalVoiceTestReconnectDelay)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("supervisor reconnected before in-flight outcome settled: attempts=%d", got)
	}
	close(adapter.release)
	select {
	case answerErr := <-answerDone:
		if !errors.Is(answerErr, sipgateway.ErrCommandOutcomeUnknown) {
			t.Fatalf("answer error=%v", answerErr)
		}
	case <-time.After(time.Second):
		t.Fatal("ambiguous provider command did not finish")
	}
	waitExternalVoiceSupervisorStatus(t, supervisor, func(status externalVoiceStatus) bool {
		return status.RecoveryRequired && status.Health == "manual_recovery_required"
	})
	time.Sleep(4 * externalVoiceTestReconnectDelay)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("supervisor reconnected after concurrent unknown: attempts=%d", got)
	}
}

const externalVoiceTestReconnectDelay = 10 * time.Millisecond

func newSyntheticSIPVoiceSupervisor(
	t *testing.T,
	factory func(context.Context) (sipgateway.Adapter, error),
) *sipVoiceSupervisor {
	t.Helper()
	store, err := openExternalVoiceRecoveryStore(
		filepath.Join(t.TempDir(), "external-voice", "recovery.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newExternalVoiceMutationJournal(store, "synthetic-home-gateway", nil)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	supervisor, err := newSIPVoiceSupervisor(context.Background(), sipVoiceSupervisorConfig{
		Runtime:        syntheticSIPVoiceRuntimeConfig(journal),
		AdapterFactory: factory,
		RecoveryInspectorFactory: func(context.Context) (sipgateway.RecoveryInspector, error) {
			return &syntheticExternalVoiceRecoveryInspector{state: sipgateway.ProviderCallEnded}, nil
		},
		RecoveryStore: store, RecoveryJournal: journal,
		ReconnectMin: externalVoiceTestReconnectDelay,
		ReconnectMax: 2 * externalVoiceTestReconnectDelay,
	})
	if err != nil {
		_ = journal.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor
}

func syntheticSIPVoiceRuntimeConfig(journal sipgateway.MutationJournal) sipVoiceRuntimeConfig {
	return sipVoiceRuntimeConfig{
		GatewayID: "synthetic-home-gateway",
		Journal:   journal, Evidence: syntheticExternalVoiceMutationEvidence,
		Network: remotevoice.Config{
			AllowedInterfaces: []string{"synthetic-tailnet-interface"},
			AllowedLocalCIDRs: []string{"100.64.10.1/32"}, AllowedRemoteCIDRs: []string{"100.64.10.2/32"},
			UDPMin: 41000, UDPMax: 41015,
		},
		AllowAnswer: true, AllowEnd: true,
		WebRTCAnswerer: func(
			context.Context,
			[]byte,
			remotevoice.Config,
			*remotevoice.FramePort,
		) (remotevoice.MediaPeer, []byte, error) {
			return &remoteMediaTestPeer{done: make(chan struct{}), prepared: true},
				[]byte("synthetic-sdp-answer"), nil
		},
	}
}

func newSyntheticSIPVoiceEmulator(t *testing.T, bootEpoch string) *sipgateway.Emulator {
	t.Helper()
	emulator, err := sipgateway.NewEmulator(sipgateway.EmulatorConfig{
		GatewayID: "synthetic-home-gateway", BootEpoch: bootEpoch,
		Capabilities: sipgateway.Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return emulator
}

func waitSyntheticSupervisorAdapter(t *testing.T, adapters <-chan *sipgateway.Emulator) *sipgateway.Emulator {
	t.Helper()
	select {
	case adapter := <-adapters:
		return adapter
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not construct an adapter")
		return nil
	}
}

func waitExternalVoiceSupervisorStatus(
	t *testing.T,
	supervisor *sipVoiceSupervisor,
	predicate func(externalVoiceStatus) bool,
) externalVoiceStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := supervisor.Status("synthetic-owner@example.invalid")
		if predicate(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status := supervisor.Status("synthetic-owner@example.invalid")
	t.Fatalf("supervisor status did not reach expected state: %+v", status)
	return externalVoiceStatus{}
}

func TestSIPVoiceSupervisorFormattingIsRedacted(t *testing.T) {
	config := sipVoiceSupervisorConfig{Runtime: sipVoiceRuntimeConfig{GatewayID: "forbidden-gateway-marker"}}
	for _, formatted := range []string{
		fmt.Sprintf("%v", config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config),
	} {
		if strings.Contains(formatted, "forbidden-gateway-marker") {
			t.Fatalf("supervisor configuration formatting leaked identity: %s", formatted)
		}
	}
}

type blockingUnknownSIPAdapter struct {
	*sipgateway.Emulator
	entered chan struct{}
	release chan struct{}
}

func (a *blockingUnknownSIPAdapter) AnswerIncoming(
	ctx context.Context,
	request sipgateway.AnswerIncomingRequest,
) (sipgateway.CommandResult, error) {
	close(a.entered)
	select {
	case <-ctx.Done():
		return sipgateway.CommandResult{}, ctx.Err()
	case <-a.release:
		return sipgateway.CommandResult{CommandID: request.CommandID, Outcome: sipgateway.CommandUnknown},
			errors.New("synthetic response loss")
	}
}
