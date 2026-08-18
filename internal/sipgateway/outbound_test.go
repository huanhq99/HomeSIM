package sipgateway

import (
	"context"
	"errors"
	"testing"
)

func TestCoordinatorDialUsesExplicitOutgoingCapability(t *testing.T) {
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "gateway", BootEpoch: "boot-epoch-1",
		Capabilities: Capabilities{Incoming: true, Dial: true, PrepareMedia: true,
			AnswerIncoming: true, EndActive: true, Codecs: []Codec{CodecPCMU}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer emulator.Close()
	coordinator, err := NewCoordinator(CoordinatorConfig{GatewayID: "gateway", Adapter: emulator})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	call, err := coordinator.Dial(context.Background(), "+8613800138000")
	if err != nil {
		t.Fatal(err)
	}
	if call.Direction != CallDirectionOutgoing || call.Phase != PhaseIncomingRinging {
		t.Fatalf("unexpected outbound call: %+v", call)
	}
	if _, err := coordinator.Dial(context.Background(), "10010"); !errors.Is(err, ErrConcurrentCall) {
		t.Fatalf("second dial error=%v, want concurrent-call rejection", err)
	}
	observation, err := coordinator.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Ignored || observation.Call == nil || observation.Call.Direction != CallDirectionOutgoing {
		t.Fatalf("duplicate provider observation was not safely ignored: %+v", observation)
	}
}

func TestCoordinatorDialRejectsUnsafeNumber(t *testing.T) {
	emulator, err := NewEmulator(EmulatorConfig{
		GatewayID: "gateway", BootEpoch: "boot-epoch-1",
		Capabilities: Capabilities{Incoming: true, Dial: true, PrepareMedia: true, Codecs: []Codec{CodecPCMU}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer emulator.Close()
	coordinator, err := NewCoordinator(CoordinatorConfig{GatewayID: "gateway", Adapter: emulator})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if _, err := coordinator.Dial(context.Background(), "sip:10086@example.invalid"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("unsafe dial error=%v", err)
	}
}
