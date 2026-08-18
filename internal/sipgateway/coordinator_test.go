package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var validTestMutationEvidence = MutationEvidence{
	OwnerDigest:   [32]byte{0x11},
	RequestDigest: [32]byte{0x22},
}

type testOrderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *testOrderLog) add(event string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *testOrderLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type memoryMutationJournal struct {
	mu           sync.Mutex
	next         uint64
	armErr       error
	resolveErr   error
	armCalls     int
	resolveCalls int
	pending      map[string]PendingMutation
	resolved     map[string]MutationResolution
	armed        []PendingMutation
	order        *testOrderLog
}

func newMemoryMutationJournal(order *testOrderLog) *memoryMutationJournal {
	return &memoryMutationJournal{
		pending: make(map[string]PendingMutation), resolved: make(map[string]MutationResolution),
		order: order,
	}
}

func (j *memoryMutationJournal) Arm(pending PendingMutation) (string, error) {
	j.order.add("journal.arm")
	if err := pending.Validate(); err != nil {
		return "", err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.armCalls++
	if j.armErr != nil {
		return "", j.armErr
	}
	j.next++
	recoveryID := fmt.Sprintf("recovery-%08d", j.next)
	j.pending[recoveryID] = pending
	j.armed = append(j.armed, pending)
	return recoveryID, nil
}

func (j *memoryMutationJournal) Resolve(recoveryID string, resolution MutationResolution) error {
	j.order.add("journal.resolve")
	if !validOpaque(recoveryID, 1, MaxMutationRecoveryIDBytes) || resolution.Validate() != nil {
		return ErrInvalidMutationJournalRecord
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.resolveCalls++
	if j.resolveErr != nil {
		return j.resolveErr
	}
	if _, ok := j.pending[recoveryID]; !ok {
		return ErrInvalidMutationJournalRecord
	}
	delete(j.pending, recoveryID)
	j.resolved[recoveryID] = resolution
	return nil
}

func (j *memoryMutationJournal) counts() (arms, resolves, pending int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.armCalls, j.resolveCalls, len(j.pending)
}

func (j *memoryMutationJournal) lastArmed() (PendingMutation, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.armed) == 0 {
		return PendingMutation{}, false
	}
	return j.armed[len(j.armed)-1], true
}

func (j *memoryMutationJournal) setArmError(err error) {
	j.mu.Lock()
	j.armErr = err
	j.mu.Unlock()
}

func (j *memoryMutationJournal) setResolveError(err error) {
	j.mu.Lock()
	j.resolveErr = err
	j.mu.Unlock()
}

type orderedMutationAdapter struct {
	*Emulator
	order *testOrderLog
}

type adapterWithoutRecovery struct{ Adapter }

type malformedMutationAdapter struct{ *Emulator }

type failingMutationAdapter struct {
	*Emulator
	err error
}

func (a *malformedMutationAdapter) AnswerIncoming(
	ctx context.Context,
	request AnswerIncomingRequest,
) (CommandResult, error) {
	result, err := a.Emulator.AnswerIncoming(ctx, request)
	if err == nil {
		result.CommandID += "-malformed"
	}
	return result, err
}

func (a *failingMutationAdapter) AnswerIncoming(
	context.Context,
	AnswerIncomingRequest,
) (CommandResult, error) {
	return CommandResult{}, a.err
}

func (a *orderedMutationAdapter) RecoveryToken(ref CallRef) (RecoveryToken, error) {
	a.order.add("adapter.recovery_token")
	return a.Emulator.RecoveryToken(ref)
}

func (a *orderedMutationAdapter) AnswerIncoming(
	ctx context.Context,
	request AnswerIncomingRequest,
) (CommandResult, error) {
	a.order.add("adapter.answer")
	return a.Emulator.AnswerIncoming(ctx, request)
}

func (a *orderedMutationAdapter) EndActive(
	ctx context.Context,
	request EndActiveRequest,
) (CommandResult, error) {
	a.order.add("adapter.end")
	return a.Emulator.EndActive(ctx, request)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

type gatedObserveAdapter struct {
	*Emulator
	mu           sync.Mutex
	calls        int
	active       int
	maxActive    int
	entered      chan int
	releaseFirst chan struct{}
}

func (a *gatedObserveAdapter) Observe(ctx context.Context) (Event, error) {
	a.mu.Lock()
	a.calls++
	call := a.calls
	a.active++
	if a.active > a.maxActive {
		a.maxActive = a.active
	}
	a.mu.Unlock()
	a.entered <- call
	defer func() {
		a.mu.Lock()
		a.active--
		a.mu.Unlock()
	}()
	if call == 1 {
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-a.releaseFirst:
		}
	}
	return a.Emulator.Observe(ctx)
}

func (a *gatedObserveAdapter) maximumActive() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxActive
}

type notifyingObserveAdapter struct {
	*Emulator
	once    sync.Once
	entered chan struct{}
}

func (a *notifyingObserveAdapter) Observe(ctx context.Context) (Event, error) {
	a.once.Do(func() { close(a.entered) })
	return a.Emulator.Observe(ctx)
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

func newTestStack(t *testing.T, policy MutationPolicy) (*Emulator, *Coordinator, *testClock) {
	return newTestStackWithJournal(t, policy, newMemoryMutationJournal(nil), nil)
}

func newTestStackWithJournal(
	t *testing.T,
	policy MutationPolicy,
	journal MutationJournal,
	wrapAdapter func(*Emulator) Adapter,
) (*Emulator, *Coordinator, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
	capabilities := Capabilities{
		Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
		Codecs: []Codec{CodecPCMU},
	}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-0001",
		Capabilities: capabilities, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("NewEmulator: %v", err)
	}
	var adapter Adapter = emulator
	if wrapAdapter != nil {
		adapter = wrapAdapter(emulator)
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: adapter, Policy: policy,
		Journal: journal,
		Now:     clock.Now, IDReader: bytes.NewReader(bytes.Repeat([]byte{0xa5}, 4096)),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return emulator, coordinator, clock
}

func observeIncoming(t *testing.T, emulator *Emulator, coordinator *Coordinator, handle string) (CallRef, PublicCallSnapshot) {
	t.Helper()
	ref, err := emulator.Incoming(handle)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	observation, err := coordinator.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe incoming: %v", err)
	}
	if observation.Ignored || observation.Call == nil || observation.Call.Phase != PhaseIncomingRinging {
		t.Fatalf("incoming observation=%+v", observation)
	}
	return ref, *observation.Call
}

func preparePCMU(t *testing.T, coordinator *Coordinator, call PublicCallSnapshot) PublicCallSnapshot {
	t.Helper()
	prepared, err := coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: call.Call, ExpectedRevision: call.Revision, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatalf("PrepareMedia: %v", err)
	}
	if prepared.Phase != PhaseMediaReady || prepared.Media == nil || !prepared.Media.Prepared {
		t.Fatalf("prepared=%+v", prepared)
	}
	return prepared
}

func onlyEmulatedMedia(t *testing.T, emulator *Emulator) MediaSession {
	t.Helper()
	emulator.mu.Lock()
	defer emulator.mu.Unlock()
	if len(emulator.media) != 1 {
		t.Fatalf("emulator media sessions=%d, want 1", len(emulator.media))
	}
	for _, media := range emulator.media {
		return media
	}
	return nil
}

func TestCoordinatorMutationPolicyRequiresJournalAndRecoveryAdapter(t *testing.T) {
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "journal-gateway", BootEpoch: "journal-boot-0001",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []Codec{CodecPCMU},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := MutationPolicy{AllowAnswerIncoming: true}
	if _, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "journal-gateway", Adapter: emulator, Policy: policy,
	}); !errors.Is(err, ErrMutationJournalRequired) {
		t.Fatalf("missing journal error=%v", err)
	}
	withoutRecovery := &adapterWithoutRecovery{Adapter: emulator}
	if _, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "journal-gateway", Adapter: withoutRecovery, Policy: policy,
		Journal: newMemoryMutationJournal(nil),
	}); !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("missing RecoveryAdapter error=%v", err)
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "journal-gateway", Adapter: withoutRecovery,
	})
	if err != nil {
		t.Fatalf("read-only policy unexpectedly required recovery: %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
}

func TestMutationJournalRecordsValidateAndFormatRedacted(t *testing.T) {
	var token RecoveryToken
	if err := token.UnmarshalBinary([]byte("provider-token-secret")); err != nil {
		t.Fatal(err)
	}
	pending := PendingMutation{
		Kind: CommandAnswerIncoming, CommandID: "command-secret-0001",
		Call:             PublicCallRef{PublicCallID: strings.Repeat("a", 43), Generation: 1},
		ExpectedRevision: 1, PublicMediaLeaseID: "public-media-secret",
		ProviderToken: token, Evidence: validTestMutationEvidence,
	}
	resolution := MutationResolution{Outcome: CommandApplied, State: ProviderCallActive}
	if err := pending.Validate(); err != nil {
		t.Fatalf("pending validation: %v", err)
	}
	if err := resolution.Validate(); err != nil {
		t.Fatalf("resolution validation: %v", err)
	}
	answerRequest := AnswerRequest{
		Call: pending.Call, ExpectedRevision: pending.ExpectedRevision,
		CommandID: pending.CommandID, MediaLeaseID: pending.PublicMediaLeaseID,
		Evidence: pending.Evidence,
	}
	endRequest := EndRequest{
		Call: pending.Call, ExpectedRevision: pending.ExpectedRevision,
		CommandID: pending.CommandID, MediaLeaseID: pending.PublicMediaLeaseID,
		Evidence: pending.Evidence,
	}
	formatted := fmt.Sprintf("%v|%#v|%v|%#v|%v|%#v|%v|%#v|%v|%#v",
		pending.Evidence, pending.Evidence, pending, pending, resolution, resolution,
		answerRequest, answerRequest, endRequest, endRequest)
	for _, forbidden := range []string{"provider-token-secret", "command-secret-0001", "public-media-secret"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("journal formatting leaked %q: %s", forbidden, formatted)
		}
	}
	missingEvidence := pending
	missingEvidence.Evidence = MutationEvidence{}
	if !errors.Is(missingEvidence.Validate(), ErrInvalidMutationJournalRecord) {
		t.Fatal("zero evidence was accepted")
	}
	endWithoutLease := pending
	endWithoutLease.Kind = CommandEndActive
	endWithoutLease.PublicMediaLeaseID = ""
	if !errors.Is(endWithoutLease.Validate(), ErrInvalidMutationJournalRecord) {
		t.Fatal("end mutation without exact public media lease was accepted")
	}
	if err := (MutationResolution{Outcome: CommandUnknown, State: ProviderCallIncoming}).Validate(); !errors.Is(err, ErrInvalidMutationJournalRecord) {
		t.Fatalf("non-terminal unknown resolution error=%v", err)
	}
}

func TestCoordinatorDurableMutationOrderAndExactEndLease(t *testing.T) {
	order := &testOrderLog{}
	journal := newMemoryMutationJournal(order)
	emulator, coordinator, _ := newTestStackWithJournal(t, MutationPolicy{
		AllowAnswerIncoming: true, AllowEndActive: true,
	}, journal, func(emulator *Emulator) Adapter {
		return &orderedMutationAdapter{Emulator: emulator, order: order}
	})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-journal-order")
	prepared := preparePCMU(t, coordinator, incoming)
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-journal-order", MediaLeaseID: prepared.Media.LeaseID,
	}
	if _, err := coordinator.AnswerIncoming(context.Background(), request); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("missing evidence error=%v", err)
	}
	if got := order.snapshot(); len(got) != 0 {
		t.Fatalf("invalid request crossed recovery boundary: %v", got)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 0 {
		t.Fatalf("invalid request executions=%d", got)
	}

	request.Evidence = validTestMutationEvidence
	answered, err := coordinator.AnswerIncoming(context.Background(), request)
	if err != nil || answered.Outcome != CommandApplied {
		t.Fatalf("answer=%+v err=%v", answered, err)
	}
	wantAnswerOrder := "adapter.recovery_token,journal.arm,adapter.answer,journal.resolve"
	if got := strings.Join(order.snapshot(), ","); got != wantAnswerOrder {
		t.Fatalf("answer order=%q want=%q", got, wantAnswerOrder)
	}
	armed, ok := journal.lastArmed()
	if !ok || armed.Call != prepared.Call || armed.ExpectedRevision != prepared.Revision ||
		armed.PublicMediaLeaseID != prepared.Media.LeaseID || armed.Evidence != validTestMutationEvidence {
		t.Fatalf("armed answer record=%+v", armed)
	}

	wrongEnd := EndRequest{
		Call: answered.Call.Call, ExpectedRevision: answered.Call.Revision,
		CommandID: "end-wrong-public-lease", MediaLeaseID: "wrong-public-media-lease",
		Evidence: validTestMutationEvidence,
	}
	if _, err := coordinator.EndActive(context.Background(), wrongEnd); !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("wrong end lease error=%v", err)
	}
	if got := strings.Join(order.snapshot(), ","); got != wantAnswerOrder {
		t.Fatalf("wrong end lease crossed durable boundary: %q", got)
	}
	correctEnd := wrongEnd
	correctEnd.CommandID = "end-journal-order"
	correctEnd.MediaLeaseID = answered.Call.Media.LeaseID
	ended, err := coordinator.EndActive(context.Background(), correctEnd)
	if err != nil || ended.Outcome != CommandApplied || ended.Call.Phase != PhaseEnded {
		t.Fatalf("end=%+v err=%v", ended, err)
	}
	wantFullOrder := wantAnswerOrder + ",adapter.recovery_token,journal.arm,adapter.end,journal.resolve"
	if got := strings.Join(order.snapshot(), ","); got != wantFullOrder {
		t.Fatalf("full mutation order=%q want=%q", got, wantFullOrder)
	}
	armed, ok = journal.lastArmed()
	if !ok || armed.Kind != CommandEndActive || armed.PublicMediaLeaseID != correctEnd.MediaLeaseID {
		t.Fatalf("armed end record=%+v", armed)
	}
	if arms, resolves, pending := journal.counts(); arms != 2 || resolves != 2 || pending != 0 {
		t.Fatalf("journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
}

func TestCoordinatorArmFailureExecutesNoProviderMutation(t *testing.T) {
	journal := newMemoryMutationJournal(nil)
	armFailure := errors.New("synthetic journal arm failure")
	journal.setArmError(armFailure)
	emulator, coordinator, _ := newTestStackWithJournal(
		t, MutationPolicy{AllowAnswerIncoming: true}, journal, nil,
	)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-arm-failure")
	prepared := preparePCMU(t, coordinator, incoming)
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-arm-failure", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	if _, err := coordinator.AnswerIncoming(context.Background(), request); !errors.Is(err, armFailure) {
		t.Fatalf("Arm failure error=%v", err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 0 {
		t.Fatalf("provider executions after Arm failure=%d", got)
	}
	current, err := coordinator.Snapshot(prepared.Call)
	if err != nil || current.Phase != PhaseMediaReady || current.ReconcileRequired {
		t.Fatalf("state after Arm failure=%+v err=%v", current, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 0 || pending != 0 {
		t.Fatalf("journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
	journal.setArmError(nil)
	result, err := coordinator.AnswerIncoming(context.Background(), request)
	if err != nil || result.Outcome != CommandApplied {
		t.Fatalf("safe retry after Arm failure=%+v err=%v", result, err)
	}
}

func TestCoordinatorResolveFailureStaysArmedUntilExplicitReconcile(t *testing.T) {
	journal := newMemoryMutationJournal(nil)
	resolveFailure := errors.New("synthetic journal resolve failure")
	journal.setResolveError(resolveFailure)
	emulator, coordinator, _ := newTestStackWithJournal(
		t, MutationPolicy{AllowAnswerIncoming: true}, journal, nil,
	)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-resolve-failure")
	prepared := preparePCMU(t, coordinator, incoming)
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-resolve-failure", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	unknown, err := coordinator.AnswerIncoming(context.Background(), request)
	if !errors.Is(err, ErrCommandOutcomeUnknown) || !errors.Is(err, resolveFailure) ||
		unknown.Outcome != CommandUnknown || unknown.Call.Phase != PhaseReconciling ||
		!unknown.Call.ReconcileRequired || unknown.Call.Revision != prepared.Revision {
		t.Fatalf("resolve failure result=%+v err=%v", unknown, err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 1 {
		t.Fatalf("answer executions=%d", got)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 1 || pending != 1 {
		t.Fatalf("journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}

	stillReconciling, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if !errors.Is(err, ErrReconcileRequired) || !errors.Is(err, resolveFailure) ||
		stillReconciling.Phase != PhaseReconciling || !stillReconciling.ReconcileRequired {
		t.Fatalf("failed reconcile=%+v err=%v", stillReconciling, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 2 || pending != 1 {
		t.Fatalf("failed reconcile counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}

	journal.setResolveError(nil)
	reconciled, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if err != nil || reconciled.Phase != PhaseActiveUnverified || reconciled.ControllerOwned ||
		reconciled.ReconcileRequired {
		t.Fatalf("resolved reconcile=%+v err=%v", reconciled, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 3 || pending != 0 {
		t.Fatalf("resolved reconcile counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
}

func TestCoordinatorRejectedResultIsNotPublishedWhenResolveFails(t *testing.T) {
	journal := newMemoryMutationJournal(nil)
	journal.setResolveError(errors.New("synthetic rejected resolve failure"))
	emulator, coordinator, _ := newTestStackWithJournal(
		t, MutationPolicy{AllowAnswerIncoming: true}, journal, nil,
	)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-rejected-resolve-failure")
	prepared := preparePCMU(t, coordinator, incoming)
	if err := emulator.SetNextCommandFault(CommandAnswerIncoming, CommandFault{
		Outcome: CommandRejected,
	}); err != nil {
		t.Fatal(err)
	}
	unknown, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-rejected-resolve-failure", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrCommandOutcomeUnknown) || unknown.Outcome != CommandUnknown ||
		unknown.Call.Phase != PhaseReconciling || !unknown.Call.ReconcileRequired {
		t.Fatalf("rejected Resolve failure=%+v err=%v", unknown, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 1 || pending != 1 {
		t.Fatalf("rejected journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
	stillUnknown, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if err != nil || stillUnknown.Phase != PhaseReconciling || !stillUnknown.ReconcileRequired {
		t.Fatalf("incoming state inferred rejection=%+v err=%v", stillUnknown, err)
	}
	if _, resolves, pending := journal.counts(); resolves != 1 || pending != 1 {
		t.Fatalf("incoming reconcile unexpectedly resolved: resolves=%d pending=%d", resolves, pending)
	}
}

func TestCoordinatorAdapterErrorAfterArmRemainsUnknownAndArmed(t *testing.T) {
	journal := newMemoryMutationJournal(nil)
	adapterFailure := errors.New("synthetic private adapter failure")
	emulator, coordinator, _ := newTestStackWithJournal(
		t, MutationPolicy{AllowAnswerIncoming: true}, journal,
		func(emulator *Emulator) Adapter {
			return &failingMutationAdapter{Emulator: emulator, err: adapterFailure}
		},
	)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-adapter-failure")
	prepared := preparePCMU(t, coordinator, incoming)
	unknown, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-adapter-failure", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrCommandOutcomeUnknown) || errors.Is(err, adapterFailure) ||
		unknown.Call.Phase != PhaseReconciling || !unknown.Call.ReconcileRequired {
		t.Fatalf("adapter failure=%+v err=%v", unknown, err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 0 {
		t.Fatalf("underlying emulator executions=%d", got)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 0 || pending != 1 {
		t.Fatalf("adapter failure journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
}

func TestCoordinatorMalformedResultAndObserveCannotBypassDurableResolve(t *testing.T) {
	journal := newMemoryMutationJournal(nil)
	emulator, coordinator, _ := newTestStackWithJournal(
		t, MutationPolicy{AllowAnswerIncoming: true}, journal,
		func(emulator *Emulator) Adapter { return &malformedMutationAdapter{Emulator: emulator} },
	)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-malformed-result")
	prepared := preparePCMU(t, coordinator, incoming)
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-malformed-result", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	unknown, err := coordinator.AnswerIncoming(context.Background(), request)
	if !errors.Is(err, ErrCommandOutcomeUnknown) || unknown.Call.Phase != PhaseReconciling {
		t.Fatalf("malformed result=%+v err=%v", unknown, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 0 || pending != 1 {
		t.Fatalf("malformed journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
	observed, err := coordinator.Observe(context.Background())
	if err != nil || observed.Call == nil || observed.Call.Phase != PhaseReconciling ||
		!observed.Call.ReconcileRequired {
		t.Fatalf("async observation bypassed journal: observation=%+v err=%v", observed, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 0 || pending != 1 {
		t.Fatalf("observe journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
	reconciled, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if err != nil || reconciled.Phase != PhaseActiveUnverified || reconciled.ReconcileRequired {
		t.Fatalf("explicit reconcile=%+v err=%v", reconciled, err)
	}
	if arms, resolves, pending := journal.counts(); arms != 1 || resolves != 1 || pending != 0 {
		t.Fatalf("resolved journal counts arms=%d resolves=%d pending=%d", arms, resolves, pending)
	}
}

type testGatedMedia struct {
	MediaSession
	prepared        bool
	snapshotLeaseID string
}

type testMismatchedSnapshotMedia struct{ MediaSession }

func (m *testMismatchedSnapshotMedia) Snapshot() MediaSnapshot {
	snapshot := m.MediaSession.Snapshot()
	snapshot.LeaseID = "wrong-private-media-owner"
	return snapshot
}

type testCloseTrackingMedia struct {
	MediaSession
	mu     sync.Mutex
	closed bool
}

func (m *testCloseTrackingMedia) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

func (m *testCloseTrackingMedia) wasClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

type testBlockingActivationMedia struct {
	MediaSession
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func (m *testBlockingActivationMedia) Activate(epoch uint64) (MediaSnapshot, error) {
	m.startedOnce.Do(func() { close(m.started) })
	<-m.release
	return m.MediaSession.Activate(epoch)
}

func (m *testBlockingActivationMedia) Close() error {
	m.releaseOnce.Do(func() { close(m.release) })
	return m.MediaSession.Close()
}

func (m *testGatedMedia) Snapshot() MediaSnapshot {
	snapshot := m.MediaSession.Snapshot()
	snapshot.Prepared = snapshot.Prepared && m.prepared
	if m.snapshotLeaseID != "" {
		snapshot.LeaseID = m.snapshotLeaseID
	}
	return snapshot
}

func TestCoordinatorWrapsAndReturnsExactInProcessMedia(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-1",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wrapped *testGatedMedia
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb4}, 4096)),
		WrapMedia: func(source MediaSession) (MediaSession, error) {
			wrapped = &testGatedMedia{MediaSession: source}
			return wrapped, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-media")
	prepared, err := coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatalf("PrepareMedia: %v", err)
	}
	if prepared.Phase != PhaseMediaPreparing || prepared.Media == nil || prepared.Media.Prepared {
		t.Fatalf("wrapped media must remain fail-closed before network preparation: %+v", prepared.Media)
	}
	got, err := coordinator.MediaSession(prepared.Call, prepared.Media.LeaseID)
	if err != nil || got != wrapped {
		t.Fatalf("MediaSession got=%T err=%v", got, err)
	}
	if _, err := coordinator.MediaSession(prepared.Call, prepared.Media.LeaseID+"x"); !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("wrong public lease error=%v", err)
	}
	wrapped.prepared = true
	current, err := coordinator.Snapshot(prepared.Call)
	if err != nil || current.Phase != PhaseMediaReady || current.Media == nil || !current.Media.Prepared {
		t.Fatalf("prepared wrapped snapshot=%+v err=%v", current.Media, err)
	}
}

func TestCoordinatorReleasesOnlyExactRingingMediaLease(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	identifierBytes := make([]byte, 0, 3*publicMediaIDBytes)
	for _, value := range []byte{0xa5, 0xa6, 0xa7} {
		identifierBytes = append(identifierBytes, bytes.Repeat([]byte{value}, publicMediaIDBytes)...)
	}
	coordinator.idReader = bytes.NewReader(identifierBytes)
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-release-media")
	first := preparePCMU(t, coordinator, incoming)
	firstMedia, err := coordinator.MediaSession(first.Call, first.Media.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := coordinator.ReleaseMedia(context.Background(), ReleaseRequest{
		Call: first.Call, ExpectedRevision: first.Revision, MediaLeaseID: first.Media.LeaseID,
	})
	if err != nil || released.Phase != PhaseIncomingRinging || released.Media != nil {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	if !firstMedia.Snapshot().Closed {
		t.Fatal("released private media remained open")
	}

	second := preparePCMU(t, coordinator, released)
	if second.Media == nil || second.Media.LeaseID == first.Media.LeaseID {
		t.Fatalf("replacement media=%+v", second.Media)
	}
	if _, err := coordinator.ReleaseMedia(context.Background(), ReleaseRequest{
		Call: second.Call, ExpectedRevision: second.Revision, MediaLeaseID: first.Media.LeaseID,
	}); !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("stale release error=%v", err)
	}
	current, err := coordinator.Snapshot(second.Call)
	if err != nil || current.Media == nil || current.Media.LeaseID != second.Media.LeaseID {
		t.Fatalf("stale release changed replacement: current=%+v err=%v", current, err)
	}
}

func TestCoordinatorCannotReleaseMediaAfterAnswerStarts(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-release-active")
	prepared := preparePCMU(t, coordinator, incoming)
	answered, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-before-release", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ReleaseMedia(context.Background(), ReleaseRequest{
		Call: answered.Call.Call, ExpectedRevision: answered.Call.Revision,
		MediaLeaseID: prepared.Media.LeaseID,
	}); !errors.Is(err, ErrWrongCallPhase) {
		t.Fatalf("active release error=%v", err)
	}
	if current, err := coordinator.Snapshot(answered.Call.Call); err != nil || current.Media == nil {
		t.Fatalf("active media was detached: current=%+v err=%v", current, err)
	}
}

func TestCoordinatorAnswerRevalidatesWrappedMediaIdentity(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 5, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-answer-1",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wrapped *testGatedMedia
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		Policy:   MutationPolicy{AllowAnswerIncoming: true},
		Journal:  newMemoryMutationJournal(nil),
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb5}, 4096)),
		WrapMedia: func(source MediaSession) (MediaSession, error) {
			wrapped = &testGatedMedia{MediaSession: source}
			return wrapped, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-answer")
	prepared, err := coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapped.prepared = true
	ready, err := coordinator.Snapshot(prepared.Call)
	if err != nil || ready.Media == nil || !ready.Media.Prepared {
		t.Fatalf("ready snapshot=%+v err=%v", ready, err)
	}
	wrapped.snapshotLeaseID = "drifted-private-media-owner"
	_, err = coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: ready.Call, ExpectedRevision: ready.Revision,
		MediaLeaseID: ready.Media.LeaseID, CommandID: "cmd-wrap-answer-drift",
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("AnswerIncoming error=%v, want media lease mismatch", err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 0 {
		t.Fatalf("adapter answer executions=%d, want 0", got)
	}
}

func TestCoordinatorWrapFailureIsRedactedAndClosesSource(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 15, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-2",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var source MediaSession
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb5}, 4096)),
		WrapMedia: func(candidate MediaSession) (MediaSession, error) {
			source = candidate
			return nil, errors.New("synthetic-private-endpoint-marker")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-error")
	_, err = coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if !errors.Is(err, ErrMediaNotPrepared) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("wrap error was not stable and redacted: %v", err)
	}
	if source == nil || !source.Snapshot().Closed {
		t.Fatal("source remained open after wrapper failure")
	}
}

func TestCoordinatorWrapErrorClosesReturnedWrapperAndSource(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 20, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-2b",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var source MediaSession
	var wrapper *testCloseTrackingMedia
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb8}, 4096)),
		WrapMedia: func(candidate MediaSession) (MediaSession, error) {
			source = candidate
			wrapper = &testCloseTrackingMedia{MediaSession: candidate}
			return wrapper, errors.New("synthetic-private-wrapper-marker")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-error-return")
	_, err = coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if !errors.Is(err, ErrMediaNotPrepared) {
		t.Fatalf("wrap error=%v", err)
	}
	if wrapper == nil || !wrapper.wasClosed() || source == nil || !source.Snapshot().Closed {
		t.Fatal("wrapper error did not close both returned wrapper and source")
	}
}

func TestCoordinatorRejectsNilWrapperAndClosesSource(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 30, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-3",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var source MediaSession
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb6}, 4096)),
		WrapMedia: func(candidate MediaSession) (MediaSession, error) {
			source = candidate
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-nil")
	_, err = coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("nil wrapper error=%v", err)
	}
	if source == nil || !source.Snapshot().Closed {
		t.Fatal("source remained open after nil wrapper")
	}
}

func TestCoordinatorRejectsMismatchedWrapperSnapshot(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 12, 45, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-wrap-4",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var source MediaSession
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb7}, 4096)),
		WrapMedia: func(candidate MediaSession) (MediaSession, error) {
			source = candidate
			return &testMismatchedSnapshotMedia{MediaSession: candidate}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-wrap-mismatch")
	_, err = coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision, Codecs: []Codec{CodecPCMU},
	})
	if !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("mismatched wrapper error=%v", err)
	}
	if source == nil || !source.Snapshot().Closed {
		t.Fatal("source remained open after mismatched wrapper")
	}
}

func TestCoordinatorCloseUnblocksConcurrentMediaActivation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "home-gateway", BootEpoch: "boot-epoch-close-activation",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wrapper *testBlockingActivationMedia
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "home-gateway", Adapter: emulator, Now: clock.Now,
		Policy:   MutationPolicy{AllowAnswerIncoming: true},
		Journal:  newMemoryMutationJournal(nil),
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb9}, 4096)),
		WrapMedia: func(source MediaSession) (MediaSession, error) {
			wrapper = &testBlockingActivationMedia{
				MediaSession: source, started: make(chan struct{}), release: make(chan struct{}),
			}
			return wrapper, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-close-activation")
	prepared := preparePCMU(t, coordinator, incoming)
	answerDone := make(chan error, 1)
	go func() {
		_, answerErr := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
			Call: prepared.Call, ExpectedRevision: prepared.Revision,
			CommandID: "command-close-activation", MediaLeaseID: prepared.Media.LeaseID,
			Evidence: validTestMutationEvidence,
		})
		answerDone <- answerErr
	}()
	select {
	case <-wrapper.started:
	case <-time.After(time.Second):
		t.Fatal("media activation did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- coordinator.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Coordinator.Close deadlocked behind media activation")
	}
	select {
	case <-answerDone:
	case <-time.After(time.Second):
		t.Fatal("answer remained blocked after Coordinator.Close")
	}
}

func TestCoordinatorMutationPolicyDefaultsToDeny(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{})
	ref, incoming := observeIncoming(t, emulator, coordinator, "dialog-default-deny")
	prepared := preparePCMU(t, coordinator, incoming)

	_, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-default-deny", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrMutationDisabled) {
		t.Fatalf("AnswerIncoming error=%v, want mutation disabled", err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 0 {
		t.Fatalf("answer executions=%d, want 0", got)
	}

	activeRef, err := emulator.ActivateExternal(ref)
	if err != nil {
		t.Fatalf("ActivateExternal: %v", err)
	}
	observation, err := coordinator.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe active: %v", err)
	}
	if observation.Call == nil || observation.Call.Phase != PhaseActiveUnmanaged {
		t.Fatalf("active observation=%+v", observation)
	}
	_, err = coordinator.EndActive(context.Background(), EndRequest{
		Call: observation.Call.Call, ExpectedRevision: activeRef.Revision, CommandID: "end-default-deny",
		MediaLeaseID: "unmanaged-media-lease",
		Evidence:     validTestMutationEvidence,
	})
	if !errors.Is(err, ErrMutationDisabled) {
		t.Fatalf("EndActive error=%v, want mutation disabled", err)
	}
	if got := emulator.CommandExecutions(CommandEndActive); got != 0 {
		t.Fatalf("end executions=%d, want 0", got)
	}
}

func TestCoordinatorOpaqueIdentityRevisionAndIdempotency(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{
		AllowAnswerIncoming: true, AllowEndActive: true,
	})
	providerRef, incoming := observeIncoming(t, emulator, coordinator, "provider-dialog-secret")
	if incoming.Call.Generation == 0 || incoming.Call.PublicCallID == "" ||
		strings.Contains(incoming.Call.PublicCallID, providerRef.ProviderHandle) {
		t.Fatalf("public identity is not opaque: %+v", incoming.Call)
	}
	encoded, err := json.Marshal(incoming)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{providerRef.ProviderHandle, providerRef.BootEpoch, providerRef.GatewayID} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public snapshot leaked adapter identity %q: %s", forbidden, encoded)
		}
	}

	_, err = coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision + 1, Codecs: []Codec{CodecPCMU},
	})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale PrepareMedia error=%v", err)
	}
	prepared := preparePCMU(t, coordinator, incoming)
	adapterMedia := onlyEmulatedMedia(t, emulator)
	if !strings.HasPrefix(prepared.Media.LeaseID, publicMediaIDPrefix) ||
		prepared.Media.LeaseID == adapterMedia.ID() {
		t.Fatalf("public lease=%q adapter lease=%q", prepared.Media.LeaseID, adapterMedia.ID())
	}
	preparedJSON, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{adapterMedia.ID(), providerRef.ProviderHandle, providerRef.BootEpoch} {
		if strings.Contains(string(preparedJSON), forbidden) {
			t.Fatalf("prepared snapshot leaked adapter identity %q: %s", forbidden, preparedJSON)
		}
	}

	_, err = coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-idempotent-0001", MediaLeaseID: "another-media-lease",
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrMediaLeaseMismatch) {
		t.Fatalf("wrong lease error=%v", err)
	}
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-idempotent-0001", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	result, err := coordinator.AnswerIncoming(context.Background(), request)
	if err != nil || result.Outcome != CommandApplied || result.Call.Phase != PhaseActiveUnverified ||
		!result.Call.ControllerOwned || result.Call.Revision != providerRef.Revision+1 {
		t.Fatalf("AnswerIncoming result=%+v err=%v", result, err)
	}
	replayed, err := coordinator.AnswerIncoming(context.Background(), request)
	if err != nil || replayed.Outcome != CommandApplied {
		t.Fatalf("replayed answer=%+v err=%v", replayed, err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 1 {
		t.Fatalf("answer executions=%d, want 1", got)
	}

	evidenceConflict := request
	evidenceConflict.Evidence.RequestDigest[0] ^= 0xff
	if _, err := coordinator.AnswerIncoming(context.Background(), evidenceConflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("conflicting evidence error=%v", err)
	}
	conflict := request
	conflict.MediaLeaseID = "different-media-lease"
	if _, err := coordinator.AnswerIncoming(context.Background(), conflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("conflicting command error=%v", err)
	}
	if _, err := coordinator.EndActive(context.Background(), EndRequest{
		Call: result.Call.Call, ExpectedRevision: result.Call.Revision - 1,
		CommandID: "end-stale-revision", MediaLeaseID: result.Call.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale EndActive error=%v", err)
	}

	endRequest := EndRequest{
		Call: result.Call.Call, ExpectedRevision: result.Call.Revision,
		CommandID: "end-idempotent-0001", MediaLeaseID: result.Call.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	ended, err := coordinator.EndActive(context.Background(), endRequest)
	if err != nil || ended.Outcome != CommandApplied || ended.Call.Phase != PhaseEnded {
		t.Fatalf("EndActive=%+v err=%v", ended, err)
	}
	if got := emulator.CommandExecutions(CommandEndActive); got != 1 {
		t.Fatalf("end executions=%d, want 1", got)
	}
	if _, err := coordinator.EndActive(context.Background(), endRequest); err != nil {
		t.Fatalf("idempotent EndActive replay: %v", err)
	}
}

func TestCoordinatorUnknownOutcomeNeverRetries(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-unknown-stays-ringing")
	prepared := preparePCMU(t, coordinator, incoming)
	if err := emulator.SetNextCommandFault(CommandAnswerIncoming, CommandFault{
		Outcome: CommandUnknown, Apply: false, EmitEvent: false,
	}); err != nil {
		t.Fatal(err)
	}
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-unknown-0001", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	result, err := coordinator.AnswerIncoming(context.Background(), request)
	if !errors.Is(err, ErrCommandOutcomeUnknown) || result.Outcome != CommandUnknown ||
		result.Call.Phase != PhaseReconciling || !result.Call.ReconcileRequired {
		t.Fatalf("unknown result=%+v err=%v", result, err)
	}
	if _, err := coordinator.AnswerIncoming(context.Background(), request); !errors.Is(err, ErrCommandOutcomeUnknown) {
		t.Fatalf("unknown replay error=%v", err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 1 {
		t.Fatalf("answer executions after replay=%d, want 1", got)
	}

	newCommand := request
	newCommand.CommandID = "answer-unknown-0002"
	if _, err := coordinator.AnswerIncoming(context.Background(), newCommand); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("new command before reconciliation error=%v", err)
	}
	reconciled, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if err != nil || reconciled.Phase != PhaseReconciling || !reconciled.ReconcileRequired {
		t.Fatalf("reconciled=%+v err=%v", reconciled, err)
	}
	if _, err := coordinator.AnswerIncoming(context.Background(), newCommand); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("new command after unchanged inspection error=%v", err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 1 {
		t.Fatalf("answer executions after reconciliation=%d, want 1", got)
	}
}

func TestCoordinatorUnknownAppliedRequiresInspectButDoesNotClaimControl(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{
		AllowAnswerIncoming: true, AllowEndActive: true,
	})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-unknown-applied")
	prepared := preparePCMU(t, coordinator, incoming)
	if err := emulator.SetNextCommandFault(CommandAnswerIncoming, CommandFault{
		Outcome: CommandUnknown, Apply: true, EmitEvent: false,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-unknown-applied", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if !errors.Is(err, ErrCommandOutcomeUnknown) {
		t.Fatalf("unknown answer error=%v", err)
	}
	reconciled, err := coordinator.Reconcile(context.Background(), prepared.Call)
	if err != nil || reconciled.Phase != PhaseActiveUnverified || reconciled.ControllerOwned ||
		reconciled.ReconcileRequired || reconciled.Media == nil || !reconciled.Media.Activated {
		t.Fatalf("reconciled active=%+v err=%v", reconciled, err)
	}
	if _, err := coordinator.EndActive(context.Background(), EndRequest{
		Call: reconciled.Call, ExpectedRevision: reconciled.Revision,
		CommandID: "end-unowned-active", MediaLeaseID: reconciled.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}); !errors.Is(err, ErrWrongCallPhase) {
		t.Fatalf("unowned EndActive error=%v", err)
	}
	if got := emulator.CommandExecutions(CommandEndActive); got != 0 {
		t.Fatalf("unowned end executions=%d", got)
	}
}

func TestCoordinatorExplicitRejectionCanUseANewCommand(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-explicit-rejection")
	prepared := preparePCMU(t, coordinator, incoming)
	if err := emulator.SetNextCommandFault(CommandAnswerIncoming, CommandFault{
		Outcome: CommandRejected,
	}); err != nil {
		t.Fatal(err)
	}
	rejected, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-rejected-0001", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil || rejected.Outcome != CommandRejected || rejected.Call.Phase != PhaseMediaReady {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
	applied, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: rejected.Call.Call, ExpectedRevision: rejected.Call.Revision,
		CommandID: "answer-after-reject", MediaLeaseID: rejected.Call.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil || applied.Outcome != CommandApplied {
		t.Fatalf("answer after explicit rejection=%+v err=%v", applied, err)
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 2 {
		t.Fatalf("answer executions=%d, want 2 explicit attempts", got)
	}
}

func TestCoordinatorUnknownEndOnlyReconcilesAndNeverRetries(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{
		AllowAnswerIncoming: true, AllowEndActive: true,
	})
	providerRef, incoming := observeIncoming(t, emulator, coordinator, "dialog-unknown-end")
	prepared := preparePCMU(t, coordinator, incoming)
	active, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-before-unknown-end", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := emulator.SetNextCommandFault(CommandEndActive, CommandFault{
		Outcome: CommandUnknown, Apply: false, EmitEvent: false,
	}); err != nil {
		t.Fatal(err)
	}
	request := EndRequest{
		Call: active.Call.Call, ExpectedRevision: active.Call.Revision,
		CommandID: "end-unknown-0001", MediaLeaseID: active.Call.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	unknown, err := coordinator.EndActive(context.Background(), request)
	if !errors.Is(err, ErrCommandOutcomeUnknown) || unknown.Call.Phase != PhaseReconciling {
		t.Fatalf("unknown end=%+v err=%v", unknown, err)
	}
	if _, err := coordinator.EndActive(context.Background(), request); !errors.Is(err, ErrCommandOutcomeUnknown) {
		t.Fatalf("unknown end replay error=%v", err)
	}
	newCommand := request
	newCommand.CommandID = "end-unknown-0002"
	if _, err := coordinator.EndActive(context.Background(), newCommand); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("new end before reconciliation error=%v", err)
	}
	if got := emulator.CommandExecutions(CommandEndActive); got != 1 {
		t.Fatalf("end executions=%d, want 1", got)
	}
	providerActive, err := emulator.Inspect(context.Background(), providerRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.EndExternal(providerActive.Ref); err != nil {
		t.Fatal(err)
	}
	ended, err := coordinator.Reconcile(context.Background(), active.Call.Call)
	if err != nil || ended.Phase != PhaseEnded || ended.ReconcileRequired {
		t.Fatalf("reconciled ended=%+v err=%v", ended, err)
	}
	if got := emulator.CommandExecutions(CommandEndActive); got != 1 {
		t.Fatalf("reconciliation retried end: executions=%d", got)
	}
}

func TestCoordinatorMediaProofIsPostActivationAndBidirectional(t *testing.T) {
	emulator, coordinator, clock := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-media-proof")
	prepared := preparePCMU(t, coordinator, incoming)
	media := onlyEmulatedMedia(t, emulator)
	if prepared.Media.LeaseID == media.ID() {
		t.Fatal("public media lease exposed adapter media identifier")
	}

	// This queued frame belongs to epoch zero even though it is read later.
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{Sequence: 1, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := media.WriteGatewayFrame(context.Background(), MediaFrame{Sequence: 1, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	answered, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-media-proof", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil {
		t.Fatalf("AnswerIncoming: %v", err)
	}
	if answered.Call.Media == nil || answered.Call.Media.BidirectionalFresh ||
		answered.Call.Media.ClientToGatewayBaseline != 1 {
		t.Fatalf("activation baseline=%+v", answered.Call.Media)
	}
	if _, err := media.ReadGatewayFrame(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterQueued, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if afterQueued.Media.GatewayToClientFresh {
		t.Fatal("queued pre-activation frame counted as fresh")
	}

	clock.Advance(20 * time.Millisecond)
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{Sequence: 2, Payload: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := media.ReadGatewayFrame(context.Background()); err != nil {
		t.Fatal(err)
	}
	oneWay, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if !oneWay.Media.GatewayToClientFresh || oneWay.Media.ClientToGatewayFresh ||
		oneWay.Media.BidirectionalFresh || oneWay.Phase != PhaseActiveUnverified {
		t.Fatalf("one-way proof=%+v", oneWay)
	}

	if err := media.WriteGatewayFrame(context.Background(), MediaFrame{Sequence: 2, Payload: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	twoWay, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if !twoWay.Media.BidirectionalFresh || twoWay.Phase != PhaseActiveTransportVerified {
		t.Fatalf("two-way proof=%+v", twoWay)
	}
	coordinator.mu.Lock()
	coordinator.call.mediaActivationFailed = true
	coordinator.mu.Unlock()
	failedActivation, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if failedActivation.Media == nil || !failedActivation.Media.ActivationFailed ||
		failedActivation.Media.Activated || failedActivation.Media.BidirectionalFresh ||
		failedActivation.Phase != PhaseActiveUnverified {
		t.Fatalf("failed activation was promoted=%+v", failedActivation)
	}
	coordinator.mu.Lock()
	coordinator.call.mediaActivationFailed = false
	coordinator.mu.Unlock()
	clock.Advance(3 * time.Second)
	stale, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Media.BidirectionalFresh || stale.Phase != PhaseActiveUnverified {
		t.Fatalf("stale proof=%+v", stale)
	}
}

func TestCoordinatorCanEndRenderedTransportVerifiedCall(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{
		AllowAnswerIncoming: true,
		AllowEndActive:      true,
	})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-end-verified")
	prepared := preparePCMU(t, coordinator, incoming)
	media := onlyEmulatedMedia(t, emulator)
	answered, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-end-verified", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil {
		t.Fatalf("AnswerIncoming: %v", err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{Sequence: 1, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := media.ReadGatewayFrame(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := media.WriteGatewayFrame(context.Background(), MediaFrame{Sequence: 1, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	verified, err := coordinator.Snapshot(answered.Call.Call)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Phase != PhaseActiveTransportVerified {
		t.Fatalf("verified phase=%q", verified.Phase)
	}
	ended, err := coordinator.EndActive(context.Background(), EndRequest{
		Call: verified.Call, ExpectedRevision: verified.Revision,
		CommandID: "end-transport-verified", MediaLeaseID: verified.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil || ended.Outcome != CommandApplied || ended.Call.Phase != PhaseEnded {
		t.Fatalf("EndActive=%+v err=%v", ended, err)
	}
}

func TestCoordinatorDuplicateReorderedCancelActiveEndAndRestart(t *testing.T) {
	emulator, coordinator, clock := newTestStack(t, MutationPolicy{})
	ref, incoming := observeIncoming(t, emulator, coordinator, "dialog-events-a")

	duplicate := Event{
		Kind: EventCallChanged, Call: &CallSnapshot{Ref: ref, State: ProviderCallIncoming},
		ObservedAt: clock.Now(),
	}
	if err := emulator.InjectEvent(duplicate); err != nil {
		t.Fatal(err)
	}
	observed, err := coordinator.Observe(context.Background())
	if err != nil || !observed.Ignored || observed.Call == nil || observed.Call.Call != incoming.Call {
		t.Fatalf("duplicate observation=%+v err=%v", observed, err)
	}

	activeRef, err := emulator.ActivateExternal(ref)
	if err != nil {
		t.Fatal(err)
	}
	observed, err = coordinator.Observe(context.Background())
	if err != nil || observed.Call == nil || observed.Call.Phase != PhaseActiveUnmanaged {
		t.Fatalf("active observation=%+v err=%v", observed, err)
	}
	if err := emulator.InjectEvent(duplicate); err != nil {
		t.Fatal(err)
	}
	reordered, err := coordinator.Observe(context.Background())
	if err != nil || !reordered.Ignored || reordered.Call.Revision != activeRef.Revision {
		t.Fatalf("reordered observation=%+v err=%v", reordered, err)
	}

	conflict := Event{
		Kind:       EventCallChanged,
		Call:       &CallSnapshot{Ref: activeRef, State: ProviderCallIncoming},
		ObservedAt: clock.Now(),
	}
	if err := emulator.InjectEvent(conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Observe(context.Background()); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("same-revision conflict error=%v", err)
	}

	endedRef, err := emulator.EndExternal(activeRef)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := coordinator.Observe(context.Background())
	if err != nil || ended.Call == nil || ended.Call.Phase != PhaseEnded || ended.Call.Revision != endedRef.Revision {
		t.Fatalf("ended observation=%+v err=%v", ended, err)
	}
	_, next := observeIncoming(t, emulator, coordinator, "dialog-events-b")
	if next.Call == incoming.Call || next.Call.Generation <= incoming.Call.Generation {
		t.Fatalf("new dialog reused public identity: old=%+v new=%+v", incoming.Call, next.Call)
	}

	prepared := preparePCMU(t, coordinator, next)
	media := onlyEmulatedMedia(t, emulator)
	if err := emulator.Restart("boot-epoch-0002"); err != nil {
		t.Fatal(err)
	}
	restart, err := coordinator.Observe(context.Background())
	if err != nil || restart.Kind != EventGatewayRestart || restart.Call != nil {
		t.Fatalf("restart=%+v err=%v", restart, err)
	}
	if _, err := coordinator.Snapshot(prepared.Call); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("old public call survived restart: %v", err)
	}
	if !media.Snapshot().Closed {
		t.Fatal("old media survived restart")
	}
	_, afterRestart := observeIncoming(t, emulator, coordinator, "dialog-events-c")
	if afterRestart.Call.Generation <= prepared.Call.Generation || afterRestart.Call == prepared.Call {
		t.Fatalf("restart reused public identity: old=%+v new=%+v", prepared.Call, afterRestart.Call)
	}
	if err := emulator.InjectEvent(Event{
		Kind: EventCallChanged, Call: &CallSnapshot{Ref: ref, State: ProviderCallIncoming},
		ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Observe(context.Background()); !errors.Is(err, ErrBootEpochMismatch) {
		t.Fatalf("old-epoch event error=%v", err)
	}
}

func TestCoordinatorIncomingRevisionAdvanceRevokesPreparedMedia(t *testing.T) {
	emulator, coordinator, clock := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	providerRef, incoming := observeIncoming(t, emulator, coordinator, "dialog-revision-advance")
	_ = preparePCMU(t, coordinator, incoming)
	media := onlyEmulatedMedia(t, emulator)
	advanced := providerRef
	advanced.Revision++
	if err := emulator.InjectEvent(Event{
		Kind: EventCallChanged, Call: &CallSnapshot{Ref: advanced, State: ProviderCallIncoming},
		ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	observation, err := coordinator.Observe(context.Background())
	if err != nil || observation.Call == nil || observation.Call.Phase != PhaseIncomingRinging ||
		observation.Call.Media != nil {
		t.Fatalf("advanced incoming=%+v err=%v", observation, err)
	}
	if !media.Snapshot().Closed {
		t.Fatal("old-revision media remained open")
	}
}

func TestCoordinatorCodecMismatchAndConcurrentDuplicateCommand(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	_, incoming := observeIncoming(t, emulator, coordinator, "dialog-codec")
	_, err := coordinator.PrepareMedia(context.Background(), PrepareRequest{
		Call: incoming.Call, ExpectedRevision: incoming.Revision,
		Codecs: []Codec{{Name: "OPUS", ClockRate: 48000, Channels: 2}},
	})
	if !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("codec mismatch error=%v", err)
	}
	prepared := preparePCMU(t, coordinator, incoming)
	request := AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-concurrent-idempotent", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	}
	const workers = 16
	var wait sync.WaitGroup
	errorsSeen := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := coordinator.AnswerIncoming(context.Background(), request)
			if err == nil && result.Outcome != CommandApplied {
				err = errors.New("unexpected non-applied outcome")
			}
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent answer: %v", err)
		}
	}
	if got := emulator.CommandExecutions(CommandAnswerIncoming); got != 1 {
		t.Fatalf("concurrent answer executions=%d, want 1", got)
	}
}

func TestCoordinatorRejectsSeenBootEpochRollback(t *testing.T) {
	emulator, coordinator, clock := newTestStack(t, MutationPolicy{})
	_, first := observeIncoming(t, emulator, coordinator, "dialog-boot-a")
	if err := emulator.Restart("boot-epoch-0002"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Observe(context.Background()); err != nil {
		t.Fatalf("Observe restart B: %v", err)
	}
	_, second := observeIncoming(t, emulator, coordinator, "dialog-boot-b")
	if first.Call == second.Call {
		t.Fatal("restart reused public call identity")
	}

	if err := emulator.InjectEvent(Event{
		Kind: EventGatewayRestart, GatewayID: "home-gateway",
		BootEpoch: "boot-epoch-0001", ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Observe(context.Background()); !errors.Is(err, ErrBootEpochMismatch) {
		t.Fatalf("stale restart error=%v", err)
	}
	if snapshot, err := coordinator.Snapshot(second.Call); err != nil || snapshot.Call != second.Call {
		t.Fatalf("stale restart changed live call: snapshot=%+v err=%v", snapshot, err)
	}
	if err := emulator.Restart("boot-epoch-0001"); !errors.Is(err, ErrBootEpochMismatch) {
		t.Fatalf("emulator reused old boot epoch: %v", err)
	}
}

func TestBootEpochStoresFailClosedAtCapacity(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 15, 30, 0, 0, time.UTC)}
	capabilities := Capabilities{
		Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU},
	}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "epoch-cap-gateway", BootEpoch: "epoch-cap-boot-a",
		Capabilities: capabilities, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "epoch-cap-gateway", Adapter: emulator, MaxBootEpochs: 1,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xc7}, 256)), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if err := emulator.InjectEvent(Event{
		Kind: EventCallChanged,
		Call: &CallSnapshot{Ref: CallRef{
			GatewayID: "epoch-cap-gateway", BootEpoch: "epoch-cap-stale-z",
			ProviderHandle: "epoch-cap-stale-dialog", Revision: 1,
		}, State: ProviderCallEnded},
		ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if observation, err := coordinator.Observe(context.Background()); err != nil || !observation.Ignored {
		t.Fatalf("initial terminal observation=%+v err=%v", observation, err)
	}
	_, incoming := observeIncoming(t, emulator, coordinator, "epoch-cap-dialog")
	if err := emulator.InjectEvent(Event{
		Kind: EventGatewayRestart, GatewayID: "epoch-cap-gateway",
		BootEpoch: "epoch-cap-boot-b", ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Observe(context.Background()); !errors.Is(err, ErrBootEpochStoreFull) {
		t.Fatalf("coordinator epoch capacity error=%v", err)
	}
	if snapshot, err := coordinator.Snapshot(incoming.Call); err != nil || snapshot.Call != incoming.Call {
		t.Fatalf("epoch capacity changed call: snapshot=%+v err=%v", snapshot, err)
	}

	bounded, err := NewEmulator(EmulatorConfig{
		GatewayID: "emulator-cap-gateway", BootEpoch: "emulator-cap-boot-a",
		Capabilities: capabilities, MaxBootEpochs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bounded.Close() })
	if err := bounded.Restart("emulator-cap-boot-b"); !errors.Is(err, ErrBootEpochStoreFull) {
		t.Fatalf("emulator epoch capacity error=%v", err)
	}
}

func TestCoordinatorObserveIsSingleConsumer(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "serial-gateway", BootEpoch: "serial-boot-0001",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := emulator.Incoming("serial-dialog")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.ActivateExternal(ref); err != nil {
		t.Fatal(err)
	}
	adapter := &gatedObserveAdapter{
		Emulator: emulator, entered: make(chan int, 2), releaseFirst: make(chan struct{}),
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "serial-gateway", Adapter: adapter,
		IDReader: bytes.NewReader(bytes.Repeat([]byte{0xb6}, 256)), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	type observeResult struct {
		observation Observation
		err         error
	}
	results := make(chan observeResult, 2)
	go func() {
		observation, err := coordinator.Observe(context.Background())
		results <- observeResult{observation: observation, err: err}
	}()
	if call := <-adapter.entered; call != 1 {
		t.Fatalf("first adapter Observe call=%d", call)
	}
	queuedContext, cancelQueued := context.WithCancel(context.Background())
	queuedStarted := make(chan struct{})
	queuedDone := make(chan error, 1)
	go func() {
		close(queuedStarted)
		_, err := coordinator.Observe(queuedContext)
		queuedDone <- err
	}()
	<-queuedStarted
	cancelQueued()
	select {
	case err := <-queuedDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Observe cancellation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued Observe ignored context cancellation")
	}
	select {
	case call := <-adapter.entered:
		t.Fatalf("canceled queued Observe reached adapter as call %d", call)
	default:
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		observation, err := coordinator.Observe(context.Background())
		results <- observeResult{observation: observation, err: err}
	}()
	<-secondStarted
	concurrentEntry := false
	select {
	case <-adapter.entered:
		concurrentEntry = true
	case <-time.After(50 * time.Millisecond):
	}
	close(adapter.releaseFirst)

	phases := make(map[CallPhase]bool)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil || result.observation.Call == nil {
				t.Fatalf("Observe result=%+v err=%v", result.observation, result.err)
			}
			phases[result.observation.Call.Phase] = true
		case <-time.After(2 * time.Second):
			t.Fatal("serialized Observe timed out")
		}
	}
	if concurrentEntry || adapter.maximumActive() != 1 {
		t.Fatalf("adapter Observe was concurrent: entered=%v max=%d", concurrentEntry, adapter.maximumActive())
	}
	if !phases[PhaseIncomingRinging] || !phases[PhaseActiveUnmanaged] {
		t.Fatalf("serialized phases=%v", phases)
	}
}

func TestCoordinatorCloseUnblocksObserve(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "close-gateway", BootEpoch: "close-boot-0001",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &notifyingObserveAdapter{Emulator: emulator, entered: make(chan struct{})}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		GatewayID: "close-gateway", Adapter: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	observeDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Observe(context.Background())
		observeDone <- err
	}()
	select {
	case <-adapter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter Observe was not entered")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- coordinator.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Observe")
	}
	select {
	case err := <-observeDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Observe after Close error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Observe remained blocked after Close")
	}
}

func TestCoordinatorMediaActivationReusesEpoch(t *testing.T) {
	emulator, coordinator, _ := newTestStack(t, MutationPolicy{AllowAnswerIncoming: true})
	providerRef, incoming := observeIncoming(t, emulator, coordinator, "dialog-activation-epoch")
	prepared := preparePCMU(t, coordinator, incoming)
	answered, err := coordinator.AnswerIncoming(context.Background(), AnswerRequest{
		Call: prepared.Call, ExpectedRevision: prepared.Revision,
		CommandID: "answer-activation-epoch", MediaLeaseID: prepared.Media.LeaseID,
		Evidence: validTestMutationEvidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if answered.Call.Media == nil || answered.Call.Media.ActivationEpoch == 0 {
		t.Fatalf("answered media=%+v", answered.Call.Media)
	}
	epoch := answered.Call.Media.ActivationEpoch
	lease := answered.Call.Media.LeaseID
	for range 2 {
		reconciled, err := coordinator.Reconcile(context.Background(), answered.Call.Call)
		if err != nil || reconciled.Media == nil || reconciled.Media.ActivationEpoch != epoch ||
			reconciled.Media.LeaseID != lease || reconciled.Media.ActivationFailed {
			t.Fatalf("idempotent Reconcile=%+v err=%v", reconciled, err)
		}
	}
	providerActive, err := emulator.Inspect(context.Background(), providerRef)
	if err != nil {
		t.Fatal(err)
	}
	providerActive.Ref.Revision++
	if err := emulator.InjectEvent(Event{
		Kind: EventCallChanged, Call: &providerActive,
		ObservedAt: time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	observed, err := coordinator.Observe(context.Background())
	if err != nil || observed.Call == nil || observed.Call.Media == nil ||
		observed.Call.Media.ActivationEpoch != epoch || observed.Call.Media.ActivationFailed {
		t.Fatalf("active revision media=%+v err=%v", observed.Call, err)
	}
}
