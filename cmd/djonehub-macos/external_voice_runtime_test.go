package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

type externalVoiceRuntimeFixture struct {
	runtime       *sipVoiceRuntime
	emulator      *sipgateway.Emulator
	network       remotevoice.Config
	mu            sync.Mutex
	peers         []*remoteMediaTestPeer
	peerPrepared  bool
	answerStarted chan struct{}
	answerRelease chan struct{}
	answerNetwork remotevoice.Config
}

type syntheticExternalVoiceMutationJournal struct {
	mu      sync.Mutex
	next    uint64
	pending map[string]sipgateway.PendingMutation
}

func (j *syntheticExternalVoiceMutationJournal) Arm(pending sipgateway.PendingMutation) (string, error) {
	if err := pending.Validate(); err != nil {
		return "", err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.pending == nil {
		j.pending = make(map[string]sipgateway.PendingMutation)
	}
	j.next++
	id := fmt.Sprintf("synthetic-recovery-%032d", j.next)
	j.pending[id] = pending
	return id, nil
}

func (j *syntheticExternalVoiceMutationJournal) Resolve(
	recoveryID string,
	resolution sipgateway.MutationResolution,
) error {
	if err := resolution.Validate(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.pending[recoveryID]; !ok {
		return sipgateway.ErrInvalidMutationJournalRecord
	}
	delete(j.pending, recoveryID)
	return nil
}

func syntheticExternalVoiceMutationEvidence(
	kind sipgateway.CommandKind,
	identity string,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationEvidence, error) {
	owner := sha256.Sum256([]byte("synthetic-owner\x00" + identity))
	request := sha256.New()
	_, _ = request.Write([]byte("synthetic-request\x00" + string(kind) + "\x00"))
	_, _ = request.Write(bodyHash[:])
	var evidence sipgateway.MutationEvidence
	evidence.OwnerDigest = owner
	copy(evidence.RequestDigest[:], request.Sum(nil))
	return evidence, evidence.Validate()
}

func newExternalVoiceRuntimeFixture(t *testing.T) *externalVoiceRuntimeFixture {
	return newExternalVoiceRuntimeFixtureWithParent(t, context.Background())
}

func newExternalVoiceDialRuntimeFixture(t *testing.T) *externalVoiceRuntimeFixture {
	return newExternalVoiceRuntimeFixtureWithOptions(t, context.Background(), true)
}

func newExternalVoiceRuntimeFixtureWithParent(
	t *testing.T,
	parent context.Context,
) *externalVoiceRuntimeFixture {
	return newExternalVoiceRuntimeFixtureWithOptions(t, parent, false)
}

func newExternalVoiceRuntimeFixtureWithOptions(
	t *testing.T,
	parent context.Context,
	allowDial bool,
) *externalVoiceRuntimeFixture {
	t.Helper()
	emulator, err := sipgateway.NewEmulator(sipgateway.EmulatorConfig{
		GatewayID: "synthetic-home-gateway", BootEpoch: "synthetic-boot-epoch-1",
		Capabilities: sipgateway.Capabilities{
			Incoming: true, Dial: allowDial, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &externalVoiceRuntimeFixture{emulator: emulator, peerPrepared: true}
	answerer := func(
		_ context.Context,
		_ []byte,
		network remotevoice.Config,
		_ *remotevoice.FramePort,
	) (remotevoice.MediaPeer, []byte, error) {
		fixture.mu.Lock()
		started := fixture.answerStarted
		release := fixture.answerRelease
		prepared := fixture.peerPrepared
		fixture.answerStarted = nil
		fixture.answerRelease = nil
		fixture.answerNetwork = cloneExternalVoiceNetwork(network)
		fixture.mu.Unlock()
		if started != nil {
			close(started)
		}
		if release != nil {
			<-release
		}
		peer := &remoteMediaTestPeer{done: make(chan struct{}), prepared: prepared}
		fixture.mu.Lock()
		fixture.peers = append(fixture.peers, peer)
		fixture.mu.Unlock()
		return peer, []byte("synthetic-sdp-answer"), nil
	}
	network := remotevoice.Config{
		AllowedInterfaces:  []string{"synthetic-tailnet-interface"},
		AllowedLocalCIDRs:  []string{"100.64.10.1/32"},
		AllowedRemoteCIDRs: []string{"100.64.10.2/32"},
		UDPMin:             41000,
		UDPMax:             41015,
	}
	fixture.network = network
	runtime, err := newExternalVoiceRuntime(parent, sipVoiceRuntimeConfig{
		GatewayID: "synthetic-home-gateway", Adapter: emulator,
		Journal:     &syntheticExternalVoiceMutationJournal{},
		Evidence:    syntheticExternalVoiceMutationEvidence,
		Network:     network,
		AllowAnswer: true, AllowEnd: true, AllowDial: allowDial, WebRTCAnswerer: answerer,
		IDReader:    syntheticExternalVoiceRandomStream(0x31, 32),
		TokenReader: syntheticExternalVoiceRandomStream(0x71, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = runtime
	t.Cleanup(func() { _ = runtime.Close() })
	return fixture
}

func TestExternalVoiceRuntimeOfferReturnsTransportPreparingBeforeBrowserProof(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	fixture.mu.Lock()
	fixture.peerPrepared = false
	fixture.mu.Unlock()
	incoming := fixture.incoming(t)
	result, err := fixture.runtime.Offer(
		context.Background(),
		"synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_preparing"),
		sha256.Sum256([]byte("synthetic-offer-body-preparing")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Call.Phase != sipgateway.PhaseMediaPreparing || result.Call.Media == nil ||
		result.Call.Media.Prepared || result.Call.Media.Activated {
		t.Fatalf("offer returned premature transport proof: %+v", result.Call)
	}
}

func TestExternalVoiceNetworkBaseSupportsTailnetOrPerOfferPublicRelay(t *testing.T) {
	tests := []struct {
		name string
		cfg  remotevoice.Config
		want bool
	}{
		{
			name: "tailnet",
			cfg: remotevoice.Config{
				AllowedInterfaces: []string{"utun42"}, AllowedLocalCIDRs: []string{"100.64.1.1/32"},
				AllowedRemoteCIDRs: []string{"100.64.1.2/32"}, UDPMin: 41000, UDPMax: 41015,
			},
			want: true,
		},
		{name: "public relay base", cfg: remotevoice.Config{UDPMin: 41000, UDPMax: 41015}, want: true},
		{
			name: "partial tailnet",
			cfg:  remotevoice.Config{AllowedInterfaces: []string{"utun42"}, UDPMin: 41000, UDPMax: 41015},
		},
		{name: "missing UDP range", cfg: remotevoice.Config{}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validExternalVoiceNetworkBase(test.cfg); got != test.want {
				t.Fatalf("validExternalVoiceNetworkBase=%t, want %t", got, test.want)
			}
		})
	}
}

func TestExternalVoiceRuntimeDialPublishesOutgoingCallAndReplays(t *testing.T) {
	fixture := newExternalVoiceDialRuntimeFixture(t)
	bodyHash := sha256.Sum256([]byte(`{"version":2,"number":"+8613800138000"}`))
	request := externalVoiceDialRequest{Version: externalVoiceAPISchemaVersion, Number: "+8613800138000"}
	first, err := fixture.runtime.Dial(
		context.Background(), "dialer@example.invalid", "dial-command-0001", request, bodyHash,
	)
	if err != nil {
		t.Fatalf("dial=%v", err)
	}
	if first.Direction != sipgateway.CallDirectionOutgoing || first.Phase != sipgateway.PhaseIncomingRinging {
		t.Fatalf("unexpected outgoing snapshot=%+v", first)
	}
	status := fixture.runtime.Status("dialer@example.invalid")
	if !status.DialEnabled || status.Call == nil || status.Call.Call != first.Call {
		t.Fatalf("dial status=%+v", status)
	}
	replayed, err := fixture.runtime.Dial(
		context.Background(), "dialer@example.invalid", "dial-command-0001", request, bodyHash,
	)
	if err != nil || replayed.Call != first.Call {
		t.Fatalf("dial replay=%+v err=%v", replayed, err)
	}
	if _, err := fixture.runtime.Dial(
		context.Background(), "dialer@example.invalid", "dial-command-0001", request,
		sha256.Sum256([]byte(`{"version":2,"number":"+8613800138001"}`)),
	); !errors.Is(err, errExternalVoiceConflict) {
		t.Fatalf("dial body conflict err=%v", err)
	}
}

func TestExternalVoiceRuntimeOfferCannotPublishAcrossNewerObservation(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.mu.Lock()
	fixture.answerStarted = started
	fixture.answerRelease = release
	fixture.mu.Unlock()
	type offerResult struct {
		result externalVoiceOfferResult
		err    error
	}
	done := make(chan offerResult, 1)
	go func() {
		result, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
			syntheticExternalVoiceOffer(incoming, "synthetic_nonce_7001"),
			sha256.Sum256([]byte("synthetic-offer-body-7001")))
		done <- offerResult{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("WebRTC answer did not reach the publication barrier")
	}
	fixture.runtime.mu.Lock()
	newer := cloneExternalVoiceCall(*fixture.runtime.current)
	newer.Revision++
	fixture.runtime.current = &newer
	fixture.runtime.mu.Unlock()
	close(release)
	select {
	case got := <-done:
		if !errors.Is(got.err, errExternalVoiceStale) {
			t.Fatalf("offer crossed newer observation: result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale offer did not terminate")
	}
	fixture.runtime.mu.Lock()
	owner := fixture.runtime.owner
	offers := len(fixture.runtime.offers)
	fixture.runtime.mu.Unlock()
	if owner != "" || offers != 0 {
		t.Fatalf("stale offer published owner/replay: owner=%q offers=%d", owner, offers)
	}
}

func TestExternalVoiceRuntimeOwnsNetworkPolicyAndRedactsSDPFormatting(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	fixture.network.AllowedInterfaces[0] = "mutated-interface"
	fixture.network.AllowedLocalCIDRs[0] = "100.64.99.1/32"
	fixture.network.AllowedRemoteCIDRs[0] = "100.64.99.2/32"
	if got := fixture.runtime.network; got.AllowedInterfaces[0] != "synthetic-tailnet-interface" ||
		got.AllowedLocalCIDRs[0] != "100.64.10.1/32" || got.AllowedRemoteCIDRs[0] != "100.64.10.2/32" {
		t.Fatalf("constructor retained mutable network aliases: %+v", got)
	}
	const marker = "forbidden-sdp-credential-marker"
	values := []any{
		externalVoiceOfferRequest{SDPOffer: marker},
		externalVoiceOfferResult{AnswerSDP: marker},
		externalVoiceOfferReplay{answerSDP: marker},
		externalVoiceCallRequest{MediaLeaseID: marker},
	}
	for _, value := range values {
		formatted := fmt.Sprintf("%v %+v %#v", value, value, value)
		if strings.Contains(formatted, marker) {
			t.Fatalf("formatting leaked external voice material: %s", formatted)
		}
	}
}

func TestExternalVoiceRuntimeRefusesMutationAfterParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	fixture := newExternalVoiceRuntimeFixtureWithParent(t, parent)
	incoming := fixture.incoming(t)
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_6001"),
		sha256.Sum256([]byte("synthetic-offer-body-6001")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	cancel()
	request := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	if _, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-after-cancel-1", request,
		sha256.Sum256([]byte("synthetic-answer-after-cancel-body"))); !errors.Is(err, errExternalVoiceUnavailable) {
		t.Fatalf("answer after parent cancellation error=%v", err)
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 0 {
		t.Fatalf("provider mutation executed after runtime cancellation: %d", got)
	}
}

func syntheticExternalVoiceRandomStream(start byte, blocks int) *bytes.Reader {
	data := make([]byte, 0, blocks*externalVoiceOfferTokenBytes)
	for index := 0; index < blocks; index++ {
		data = append(data, bytes.Repeat([]byte{start + byte(index)}, externalVoiceOfferTokenBytes)...)
	}
	return bytes.NewReader(data)
}

func (f *externalVoiceRuntimeFixture) incoming(t *testing.T) sipgateway.PublicCallSnapshot {
	t.Helper()
	if _, err := f.emulator.Incoming("synthetic-provider-dialog-1"); err != nil {
		t.Fatal(err)
	}
	return waitExternalVoiceCall(t, f.runtime, func(call sipgateway.PublicCallSnapshot) bool {
		return call.Phase == sipgateway.PhaseIncomingRinging
	})
}

func (f *externalVoiceRuntimeFixture) lastPeer(t *testing.T) *remoteMediaTestPeer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.peers) == 0 {
		t.Fatal("WebRTC answerer did not publish a peer")
	}
	return f.peers[len(f.peers)-1]
}

func waitExternalVoiceCall(
	t *testing.T,
	runtime *sipVoiceRuntime,
	predicate func(sipgateway.PublicCallSnapshot) bool,
) sipgateway.PublicCallSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := runtime.Status("synthetic-owner@example.invalid")
		if status.Call != nil && predicate(*status.Call) {
			return *status.Call
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("external voice call did not reach the expected state")
	return sipgateway.PublicCallSnapshot{}
}

func syntheticExternalVoiceOffer(call sipgateway.PublicCallSnapshot, nonce string) externalVoiceOfferRequest {
	return externalVoiceOfferRequest{
		Version: 2, Call: call.Call, ExpectedRevision: call.Revision,
		ClientNonce: nonce, SDPOffer: "synthetic-sdp-offer",
	}
}

func TestExternalVoiceRuntimeOwnsReplaysAnswersAndEndsOneExactCall(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	request := syntheticExternalVoiceOffer(incoming, "synthetic_nonce_0001")
	bodyHash := sha256.Sum256([]byte("synthetic-offer-body-1"))
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid", request, bodyHash)
	if err != nil || offer.AnswerSDP != "synthetic-sdp-answer" || offer.Call.Media == nil ||
		!offer.Call.Media.Prepared {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	replay, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid", request, bodyHash)
	if err != nil || replay.AnswerSDP != offer.AnswerSDP || replay.Call.Media == nil ||
		replay.Call.Media.LeaseID != offer.Call.Media.LeaseID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	conflictingNonce := request
	conflictingNonce.ClientNonce = "synthetic_nonce_0002"
	if _, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid", conflictingNonce,
		sha256.Sum256([]byte("synthetic-offer-body-2"))); !errors.Is(err, errExternalVoiceConflict) {
		t.Fatalf("different nonce on live lease error=%v", err)
	}
	if _, err := fixture.runtime.Offer(context.Background(), "other-owner@example.invalid", conflictingNonce,
		sha256.Sum256([]byte("synthetic-offer-body-3"))); !errors.Is(err, errExternalVoiceConflict) {
		t.Fatalf("different owner error=%v", err)
	}
	answerRequest := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	answered, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-command-1", answerRequest,
		sha256.Sum256([]byte("synthetic-answer-command-body-1")))
	if err != nil || answered.Outcome != sipgateway.CommandApplied ||
		answered.Call.Phase != sipgateway.PhaseActiveUnverified {
		t.Fatalf("answered=%+v err=%v", answered, err)
	}
	if executions := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); executions != 1 {
		t.Fatalf("answer executions=%d", executions)
	}
	endRequest := externalVoiceCallRequest{
		Version: 2, Call: answered.Call.Call, ExpectedRevision: answered.Call.Revision,
		MediaLeaseID: answered.Call.Media.LeaseID,
	}
	ended, err := fixture.runtime.End(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-end-command-1", endRequest,
		sha256.Sum256([]byte("synthetic-end-command-body-1")))
	if err != nil || ended.Outcome != sipgateway.CommandApplied || ended.Call.Phase != sipgateway.PhaseEnded {
		t.Fatalf("ended=%+v err=%v", ended, err)
	}
	if executions := fixture.emulator.CommandExecutions(sipgateway.CommandEndActive); executions != 1 {
		t.Fatalf("end executions=%d", executions)
	}
}

func TestExternalVoiceRuntimeReleasesClosedRingingPeerAndAllowsSameOwnerReprepare(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	firstRequest := syntheticExternalVoiceOffer(incoming, "synthetic_nonce_1001")
	first, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid", firstRequest,
		sha256.Sum256([]byte("synthetic-offer-body-1001")))
	if err != nil || first.Call.Media == nil {
		t.Fatalf("first offer=%+v err=%v", first, err)
	}
	if err := fixture.lastPeer(t).Close(); err != nil {
		t.Fatal(err)
	}
	released := waitExternalVoiceCall(t, fixture.runtime, func(call sipgateway.PublicCallSnapshot) bool {
		return call.Phase == sipgateway.PhaseIncomingRinging && call.Media == nil
	})
	secondRequest := syntheticExternalVoiceOffer(released, "synthetic_nonce_1002")
	second, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid", secondRequest,
		sha256.Sum256([]byte("synthetic-offer-body-1002")))
	if err != nil || second.Call.Media == nil || second.Call.Media.LeaseID == first.Call.Media.LeaseID {
		t.Fatalf("second offer=%+v err=%v", second, err)
	}
	if status := fixture.runtime.Status("other-owner@example.invalid"); status.OwnedByRequester {
		t.Fatal("ringing peer release transferred call ownership")
	}
}

func TestExternalVoiceRuntimeUnknownAnswerRequiresReadOnlyReconcile(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_2001"),
		sha256.Sum256([]byte("synthetic-offer-body-2001")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	if err := fixture.emulator.SetNextCommandFault(sipgateway.CommandAnswerIncoming, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: true,
	}); err != nil {
		t.Fatal(err)
	}
	request := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	unknown, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-unknown-1", request,
		sha256.Sum256([]byte("synthetic-answer-unknown-body-1")))
	if !errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) || !unknown.Call.ReconcileRequired {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, err := fixture.runtime.Reconcile(context.Background(), "synthetic-owner@example.invalid",
		request, false, true); !errors.Is(err, errExternalVoiceConflict) {
		t.Fatalf("reconcile without calls.control error=%v", err)
	}
	reconciled, err := fixture.runtime.Reconcile(context.Background(), "synthetic-owner@example.invalid",
		request, true, false)
	if err != nil || reconciled.ReconcileRequired || reconciled.Phase != sipgateway.PhaseActiveUnverified {
		t.Fatalf("reconciled=%+v err=%v", reconciled, err)
	}
	if executions := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); executions != 1 {
		t.Fatalf("reconcile repeated answer: executions=%d", executions)
	}
}

func TestExternalVoiceRuntimeKeepsUnknownAnswerReconcileAuthorityUntilResolved(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_3001"),
		sha256.Sum256([]byte("synthetic-offer-body-3001")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	if err := fixture.emulator.SetNextCommandFault(sipgateway.CommandAnswerIncoming, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: false,
	}); err != nil {
		t.Fatal(err)
	}
	request := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	if _, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-unknown-noapply-1", request,
		sha256.Sum256([]byte("synthetic-answer-unknown-noapply-body-1"))); !errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		t.Fatalf("answer error=%v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		reconciled, err := fixture.runtime.Reconcile(context.Background(), "synthetic-owner@example.invalid",
			request, true, false)
		if err != nil || !reconciled.ReconcileRequired || reconciled.Phase != sipgateway.PhaseReconciling {
			t.Fatalf("reconcile attempt %d=%+v err=%v", attempt, reconciled, err)
		}
	}
	if executions := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); executions != 1 {
		t.Fatalf("reconcile repeated answer: executions=%d", executions)
	}
}

func TestExternalVoiceRuntimeKeepsUnknownEndReconcileAuthorityUntilResolved(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_4001"),
		sha256.Sum256([]byte("synthetic-offer-body-4001")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	answerRequest := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	answered, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-before-unknown-end-1", answerRequest,
		sha256.Sum256([]byte("synthetic-answer-before-unknown-end-body-1")))
	if err != nil || answered.Call.Media == nil {
		t.Fatalf("answer=%+v err=%v", answered, err)
	}
	if err := fixture.emulator.SetNextCommandFault(sipgateway.CommandEndActive, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: false,
	}); err != nil {
		t.Fatal(err)
	}
	endRequest := externalVoiceCallRequest{
		Version: 2, Call: answered.Call.Call, ExpectedRevision: answered.Call.Revision,
		MediaLeaseID: answered.Call.Media.LeaseID,
	}
	if _, err := fixture.runtime.End(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-end-unknown-noapply-1", endRequest,
		sha256.Sum256([]byte("synthetic-end-unknown-noapply-body-1"))); !errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		t.Fatalf("end error=%v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		reconciled, err := fixture.runtime.Reconcile(context.Background(), "synthetic-owner@example.invalid",
			endRequest, false, true)
		if err != nil || !reconciled.ReconcileRequired || reconciled.Phase != sipgateway.PhaseReconciling {
			t.Fatalf("reconcile attempt %d=%+v err=%v", attempt, reconciled, err)
		}
	}
	if executions := fixture.emulator.CommandExecutions(sipgateway.CommandEndActive); executions != 1 {
		t.Fatalf("reconcile repeated end: executions=%d", executions)
	}
}

func TestExternalVoiceRuntimeNeverPublishesOlderOrRestartedCallSnapshot(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	offer, err := fixture.runtime.Offer(context.Background(), "synthetic-owner@example.invalid",
		syntheticExternalVoiceOffer(incoming, "synthetic_nonce_5001"),
		sha256.Sum256([]byte("synthetic-offer-body-5001")))
	if err != nil || offer.Call.Media == nil {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	answered, err := fixture.runtime.Answer(context.Background(), "synthetic-owner@example.invalid",
		"synthetic-answer-revision-1", externalVoiceCallRequest{
			Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
			MediaLeaseID: offer.Call.Media.LeaseID,
		}, sha256.Sum256([]byte("synthetic-answer-revision-body-1")))
	if err != nil || answered.Call.Revision <= offer.Call.Revision {
		t.Fatalf("answer=%+v err=%v", answered, err)
	}
	fixture.runtime.publishCurrent(offer.Call)
	if status := fixture.runtime.Status("synthetic-owner@example.invalid"); status.Call == nil ||
		status.Call.Revision != answered.Call.Revision {
		t.Fatalf("older result rolled back current call: %+v", status.Call)
	}
	if err := fixture.emulator.Restart("synthetic-boot-epoch-2"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.runtime.Status("synthetic-owner@example.invalid").Call == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := fixture.runtime.Status("synthetic-owner@example.invalid"); status.Call != nil {
		t.Fatalf("restart did not clear current call: %+v", status.Call)
	}
	fixture.runtime.publishCurrent(answered.Call)
	if status := fixture.runtime.Status("synthetic-owner@example.invalid"); status.Call != nil {
		t.Fatalf("in-flight old result resurrected after restart: %+v", status.Call)
	}
}
