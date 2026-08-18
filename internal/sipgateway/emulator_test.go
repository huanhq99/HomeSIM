package sipgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEmulatorLifecycleInspectAndCommandConflict(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "synthetic-gateway", BootEpoch: "synthetic-boot-1",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emulator.Close() })

	ref, err := emulator.Incoming("synthetic-dialog")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspected, err := emulator.Inspect(context.Background(), ref)
	if err != nil || inspected.State != ProviderCallIncoming || inspected.Ref != ref {
		t.Fatalf("Inspect=%+v err=%v", inspected, err)
	}
	media, err := emulator.PrepareMedia(context.Background(), PrepareMediaRequest{
		Call: ref, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := AnswerIncomingRequest{
		Call: ref, CommandID: "synthetic-answer-1", MediaLeaseID: media.ID(),
	}
	result, err := emulator.AnswerIncoming(context.Background(), answer)
	if err != nil || result.Outcome != CommandApplied || result.Current == nil ||
		result.Current.State != ProviderCallActive {
		t.Fatalf("AnswerIncoming=%+v err=%v", result, err)
	}
	if _, err := emulator.AnswerIncoming(context.Background(), answer); err != nil {
		t.Fatalf("idempotent adapter replay: %v", err)
	}
	conflict := answer
	conflict.MediaLeaseID = "other-synthetic-media"
	if _, err := emulator.AnswerIncoming(context.Background(), conflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("adapter conflict error=%v", err)
	}

	active := *result.Current
	ended, err := emulator.EndActive(context.Background(), EndActiveRequest{
		Call: active.Ref, CommandID: "synthetic-end-0001",
	})
	if err != nil || ended.Outcome != CommandApplied || ended.Current == nil ||
		ended.Current.State != ProviderCallEnded {
		t.Fatalf("EndActive=%+v err=%v", ended, err)
	}
	if _, err := emulator.Inspect(context.Background(), ref); err != nil {
		t.Fatalf("stale reference must still locate dialog for reconciliation: %v", err)
	}
}

func TestEmulatorRecoveryTokenIsBoundedRedactedAndReadOnly(t *testing.T) {
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "recovery-gateway", BootEpoch: "recovery-boot-0001",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, AnswerIncoming: true, EndActive: true,
			Codecs: []Codec{CodecPCMU},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emulator.Close() })
	ref, err := emulator.Incoming("recovery-provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	token, err := emulator.RecoveryToken(ref)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := token.MarshalBinary()
	if err != nil || len(encoded) == 0 || len(encoded) > MaxRecoveryTokenBytes {
		t.Fatalf("encoded token bytes=%d err=%v", len(encoded), err)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", token), fmt.Sprintf("%+v", token), fmt.Sprintf("%#v", token),
	} {
		for _, secret := range []string{ref.GatewayID, ref.BootEpoch, ref.ProviderHandle, string(encoded)} {
			if strings.Contains(formatted, secret) {
				t.Fatalf("RecoveryToken formatting leaked %q: %s", secret, formatted)
			}
		}
	}
	jsonToken, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{ref.GatewayID, ref.BootEpoch, ref.ProviderHandle, string(encoded)} {
		if strings.Contains(string(jsonToken), secret) {
			t.Fatalf("RecoveryToken JSON leaked %q: %s", secret, jsonToken)
		}
	}
	before, err := emulator.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := emulator.InspectRecovery(context.Background(), token)
	if err != nil || recovered.State != ProviderCallIncoming {
		t.Fatalf("InspectRecovery=%+v err=%v", recovered, err)
	}
	after, err := emulator.Inspect(context.Background(), ref)
	if err != nil || after != before {
		t.Fatalf("InspectRecovery mutated call: before=%+v after=%+v err=%v", before, after, err)
	}

	var malformed RecoveryToken
	if err := malformed.UnmarshalBinary([]byte("not-an-emulator-recovery-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.InspectRecovery(context.Background(), malformed); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("malformed recovery token error=%v", err)
	}
	other, err := NewEmulator(EmulatorConfig{
		GatewayID: "recovery-gateway", BootEpoch: "other-recovery-boot",
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.InspectRecovery(context.Background(), token); !errors.Is(err, ErrBootEpochMismatch) {
		t.Fatalf("wrong provider incarnation error=%v", err)
	}
	peer, err := NewEmulator(EmulatorConfig{
		GatewayID: ref.GatewayID, BootEpoch: ref.BootEpoch,
		Capabilities: Capabilities{Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := peer.Incoming(ref.ProviderHandle); err != nil {
		t.Fatal(err)
	}
	peerRecovered, err := peer.InspectRecovery(context.Background(), token)
	if err != nil || peerRecovered.State != ProviderCallIncoming {
		t.Fatalf("cross-instance InspectRecovery=%+v err=%v", peerRecovered, err)
	}
}

func TestEmulatorCancelRestartAndQueueBounds(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC)}
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "bounded-gateway", BootEpoch: "bounded-boot-01",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU},
		},
		Now: clock.Now, EventCapacity: 1, MediaQueueCapacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emulator.Close() })
	ref, err := emulator.Incoming("bounded-dialog")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := Event{
		Kind: EventCallChanged, Call: &CallSnapshot{Ref: ref, State: ProviderCallIncoming},
		ObservedAt: clock.Now(),
	}
	if err := emulator.InjectEvent(duplicate); !errors.Is(err, ErrObservationQueueFull) {
		t.Fatalf("full event queue error=%v", err)
	}
	if _, err := emulator.Cancel(ref); !errors.Is(err, ErrObservationQueueFull) {
		t.Fatalf("full-queue Cancel error=%v", err)
	}
	if inspected, err := emulator.Inspect(context.Background(), ref); err != nil ||
		inspected.State != ProviderCallIncoming || inspected.Ref != ref {
		t.Fatalf("failed Cancel changed state: inspected=%+v err=%v", inspected, err)
	}
	if err := emulator.Restart("bounded-boot-02"); !errors.Is(err, ErrObservationQueueFull) {
		t.Fatalf("full-queue Restart error=%v", err)
	}
	if inspected, err := emulator.Inspect(context.Background(), ref); err != nil || inspected.Ref != ref {
		t.Fatalf("failed Restart changed epoch/state: inspected=%+v err=%v", inspected, err)
	}
	if _, err := emulator.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	ended, err := emulator.Cancel(ref)
	if err != nil || ended.Revision != ref.Revision+1 {
		t.Fatalf("Cancel ref=%+v err=%v", ended, err)
	}
	if _, err := emulator.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := emulator.Restart("bounded-boot-02"); err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.Inspect(context.Background(), ended); !errors.Is(err, ErrBootEpochMismatch) {
		t.Fatalf("old epoch Inspect error=%v", err)
	}
	restart, err := emulator.Observe(context.Background())
	if err != nil || restart.Kind != EventGatewayRestart {
		t.Fatalf("restart=%+v err=%v", restart, err)
	}
	mediaRef, err := emulator.Incoming("bounded-media-dialog")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	media, err := emulator.PrepareMedia(context.Background(), PrepareMediaRequest{
		Call: mediaRef, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{
		Sequence: 1, Payload: []byte{1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{
		Sequence: 2, Payload: []byte{2},
	}); !errors.Is(err, ErrMediaQueueFull) {
		t.Fatalf("full media queue error=%v", err)
	}
	if frame, err := media.ReadGatewayFrame(context.Background()); err != nil || frame.Sequence != 1 {
		t.Fatalf("queued frame=%+v err=%v", frame, err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{
		Sequence: 2, Payload: []byte{2},
	}); err != nil {
		t.Fatalf("media queue did not recover: %v", err)
	}
}

func TestAdapterPrivateOwnersRedactFormattingAndJSON(t *testing.T) {
	ref := CallRef{
		GatewayID: "gateway-format-secret", BootEpoch: "boot-format-secret",
		ProviderHandle: "provider-format-secret", Revision: 7,
	}
	snapshot := CallSnapshot{Ref: ref, State: ProviderCallIncoming}
	event := Event{
		Kind: EventCallChanged, Call: &snapshot,
		ObservedAt: time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC),
	}
	media := MediaSnapshot{LeaseID: "lease-format", Call: ref, Codec: CodecPCMU, Prepared: true}
	values := []any{
		ref,
		snapshot,
		event,
		media,
		PrepareMediaRequest{Call: ref, Codecs: []Codec{CodecPCMU}},
		AnswerIncomingRequest{Call: ref, CommandID: "command-format", MediaLeaseID: "lease-format"},
		EndActiveRequest{Call: ref, CommandID: "command-format"},
		CommandResult{CommandID: "command-format", Outcome: CommandApplied, Current: &snapshot},
	}
	secrets := []string{ref.GatewayID, ref.BootEpoch, ref.ProviderHandle}
	for _, value := range values {
		for _, formatted := range []string{
			fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value),
		} {
			for _, secret := range secrets {
				if strings.Contains(formatted, secret) {
					t.Fatalf("%T formatting leaked %q: %s", value, secret, formatted)
				}
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %T: %v", value, err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("%T JSON leaked %q: %s", value, secret, encoded)
			}
		}
	}
}

func TestEmulatorCloseRejectsBufferedObservationsAndMedia(t *testing.T) {
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "closed-gateway", BootEpoch: "closed-boot-0001",
		Capabilities: Capabilities{
			Incoming: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := emulator.Incoming("closed-dialog")
	if err != nil {
		t.Fatal(err)
	}
	media, err := emulator.PrepareMedia(context.Background(), PrepareMediaRequest{
		Call: ref, Codecs: []Codec{CodecPCMU},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{
		Sequence: 1, Payload: []byte{1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := emulator.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := emulator.Observe(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("buffered Observe after Close error=%v", err)
	}
	if _, err := media.ReadGatewayFrame(context.Background()); !errors.Is(err, ErrMediaClosed) {
		t.Fatalf("buffered media read after Close error=%v", err)
	}
	if err := emulator.InjectGatewayFrame(media.ID(), MediaFrame{
		Sequence: 2, Payload: []byte{2},
	}); !errors.Is(err, ErrMediaClosed) {
		t.Fatalf("media inject after Close error=%v", err)
	}
	if err := media.WriteGatewayFrame(context.Background(), MediaFrame{
		Sequence: 2, Payload: []byte{2},
	}); !errors.Is(err, ErrMediaClosed) {
		t.Fatalf("media write after Close error=%v", err)
	}
}
