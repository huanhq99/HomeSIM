package asterisk

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

func cleanStartupConfig(fake *fakeAsterisk) Config {
	cfg := fake.config()
	cfg.RequireCleanStartup = true
	return cfg
}

func TestCleanStartupUsesPBXTimeAnchorsAroundChannelEnumeration(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()
	restart, err := adapter.Observe(context.Background())
	if err != nil || restart.Kind != sipgateway.EventGatewayRestart {
		t.Fatalf("restart=%+v err=%v", restart, err)
	}
	fake.mu.Lock()
	requests := append([]string(nil), fake.requestLog...)
	fake.mu.Unlock()
	if len(requests) != 6 || requests[0] != "GET asterisk/info" ||
		requests[1] != "GET asterisk/info" ||
		!strings.HasPrefix(requests[2], "POST events/user/"+cleanStartupMarkerPrefix) ||
		requests[3] != "GET channels" ||
		!strings.HasPrefix(requests[4], "POST events/user/"+cleanStartupMarkerPrefix) ||
		requests[5] != "GET asterisk/info" || requests[2] == requests[4] {
		t.Fatalf("startup request order=%q", requests)
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupRejectsDelayedPreCutoffIncoming(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := fake.incomingChannel("delayed-pre-cutoff", "Ring")
	stale.CreationTime = "2026-08-14T01:03:02.999+00:00"
	if err := fake.sendEvent(ariEvent{
		Type: "StasisStart", Args: []string{"incoming"}, Channel: &stale,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := adapter.Observe(ctx); !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("Observe error=%v", err)
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupAcceptsStrictlyPostCutoffIncoming(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.incoming("post-cutoff-incoming")
	event, err := adapter.Observe(context.Background())
	if err != nil || event.Call == nil || event.Call.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func TestCleanStartupRejectsPBXIdentityDrift(t *testing.T) {
	fake := newFakeAsterisk(t)
	fake.mu.Lock()
	fake.infoHook = func(index int) {
		if index == 2 {
			fake.mu.Lock()
			fake.startupTime = "2026-08-14T01:02:04.000+00:00"
			fake.mu.Unlock()
		}
	}
	fake.mu.Unlock()
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("adapter=%v err=%v", adapter, err)
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupRejectsPBXVersionDrift(t *testing.T) {
	fake := newFakeAsterisk(t)
	fake.mu.Lock()
	fake.infoHook = func(index int) {
		if index == 2 {
			fake.mu.Lock()
			fake.version = "22.10.2"
			fake.mu.Unlock()
		}
	}
	fake.mu.Unlock()
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("adapter=%v err=%v", adapter, err)
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupRejectsPreexistingProviderChannel(t *testing.T) {
	for _, state := range []string{"Ring", "Up"} {
		t.Run(state, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			fake.mu.Lock()
			fake.channels["orphan-provider-channel"] = state
			fake.mu.Unlock()
			adapter, err := New(context.Background(), cleanStartupConfig(fake))
			if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("adapter=%v err=%v", adapter, err)
			}
			assertNoIncomingMutation(t, fake)
		})
	}
}

func TestCleanStartupRejectsProviderCallRacingEitherBarrierWindow(t *testing.T) {
	for _, test := range []struct {
		name    string
		install func(*fakeAsterisk)
	}{
		{
			name: "before first barrier",
			install: func(fake *fakeAsterisk) {
				fake.barrierHook = func(index int, _ string) {
					if index == 1 {
						fake.queueStartupIncoming("barrier-race-channel")
					}
				}
			},
		},
		{
			name: "after enumeration snapshot",
			install: func(fake *fakeAsterisk) {
				fake.channelListHook = func() {
					fake.queueStartupIncoming("enumeration-race-channel")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			fake.mu.Lock()
			test.install(fake)
			fake.mu.Unlock()
			adapter, err := New(context.Background(), cleanStartupConfig(fake))
			if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("adapter=%v err=%v", adapter, err)
			}
			assertNoIncomingMutation(t, fake)
		})
	}
}

func TestCleanStartupAllowsStrictlyUnrelatedChannel(t *testing.T) {
	fake := newFakeAsterisk(t)
	unrelated := ariChannel{ID: "unrelated-channel", Name: "PJSIP/other-endpoint-00000002", State: "Up"}
	unrelated.Dialplan.Context = "other-context"
	unrelated.Dialplan.Exten = "200"
	unrelated.Dialplan.Priority = 1
	unrelated.Dialplan.AppName = "Stasis"
	unrelated.Dialplan.AppData = "other-app,other-route"
	fake.mu.Lock()
	fake.startupChannels = []ariChannel{unrelated}
	fake.mu.Unlock()
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupRejectsPartialProviderIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ariChannel)
	}{
		{name: "context", mutate: func(channel *ariChannel) {
			channel.Dialplan.Context = "unexpected-context"
		}},
		{name: "missing policy marker", mutate: func(channel *ariChannel) {
			channel.ChannelVars = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			ambiguous := fake.incomingChannel("ambiguous-provider-channel", "Ring")
			test.mutate(&ambiguous)
			fake.mu.Lock()
			fake.startupChannels = []ariChannel{ambiguous}
			fake.mu.Unlock()
			adapter, err := New(context.Background(), cleanStartupConfig(fake))
			if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("adapter=%v err=%v", adapter, err)
			}
			assertNoIncomingMutation(t, fake)
		})
	}
}

func TestCleanStartupFailuresRemainRecoveryLocked(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeAsterisk, *Config)
	}{
		{
			name: "malformed channel list",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.channelListRaw = []byte(`[{"id":"first","id":"duplicate","name":"PJSIP/other-00000001","state":"Up"}]`)
			},
		},
		{
			name: "channel list HTTP failure",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.channelListStatus = http.StatusInternalServerError
			},
		},
		{
			name: "malformed event",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.barrierHook = func(index int, _ string) {
					if index == 1 {
						_ = fake.sendRawEvent(`{"type":"StasisStart",`)
					}
				}
			},
		},
		{
			name: "barrier timeout",
			mutate: func(fake *fakeAsterisk, cfg *Config) {
				fake.barrierNoEvent = true
				cfg.HTTPTimeout = 25 * time.Millisecond
			},
		},
		{
			name: "invalid marker timestamp",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.barrierHook = func(index int, marker string) {
					if index == 1 {
						_ = fake.sendEvent(ariEvent{
							Type: "ChannelUserevent", EventName: marker, Timestamp: "invalid-time",
						})
					}
				}
			},
		},
		{
			name: "wrong PBX event identity",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.barrierHook = func(index int, marker string) {
					if index == 1 {
						_ = fake.sendEvent(ariEvent{
							Type: "ChannelUserevent", EventName: marker,
							AsteriskID: "different-pbx-entity",
						})
					}
				}
			},
		},
		{
			name: "PBX clock moves backwards across anchors",
			mutate: func(fake *fakeAsterisk, _ *Config) {
				fake.barrierHook = func(index int, marker string) {
					if index == 2 {
						_ = fake.sendEvent(ariEvent{
							Type: "ChannelUserevent", EventName: marker,
							Timestamp: "2026-08-14T01:03:02.000+00:00",
						})
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			cfg := cleanStartupConfig(fake)
			fake.mu.Lock()
			test.mutate(fake, &cfg)
			fake.mu.Unlock()
			adapter, err := New(context.Background(), cfg)
			if adapter != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("adapter=%v err=%v", adapter, err)
			}
			assertNoIncomingMutation(t, fake)
		})
	}
}

func TestCleanStartupRejectsPostStartupEventFromDifferentPBX(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	channel := fake.incomingChannel("wrong-pbx-event", "Ring")
	if err := fake.sendEvent(ariEvent{
		Type: "StasisStart", AsteriskID: "different-pbx-entity",
		Args: []string{"incoming"}, Channel: &channel,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := adapter.Observe(ctx); !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("Observe error=%v", err)
	}
	assertNoIncomingMutation(t, fake)
}

func TestApplicationOwnershipLossTerminatesAdapterForRecovery(t *testing.T) {
	for _, eventType := range []string{"ApplicationReplaced", "ApplicationUnregistered"} {
		t.Run(eventType, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			adapter, err := New(context.Background(), cleanStartupConfig(fake))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = adapter.Close() }()
			if _, err := adapter.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := fake.sendEvent(ariEvent{Type: eventType, Application: fake.app}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := adapter.Observe(ctx); !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("Observe error=%v", err)
			}
			assertNoIncomingMutation(t, fake)
		})
	}
}

func TestApplicationReplacementNeverDeletesExistingProviderChannel(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adapter.Close() }()
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.incoming("provider-channel-before-replacement")
	if event, err := adapter.Observe(context.Background()); err != nil || event.Call == nil {
		t.Fatalf("incoming event=%+v err=%v", event, err)
	}
	if err := fake.sendEvent(ariEvent{Type: "ApplicationReplaced", Application: fake.app}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := adapter.Observe(ctx); !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("Observe error=%v", err)
	}
	fake.mu.Lock()
	_, providerStillExists := fake.channels["provider-channel-before-replacement"]
	fake.mu.Unlock()
	if !providerStillExists {
		t.Fatal("application replacement deleted the provider channel")
	}
	assertNoIncomingMutation(t, fake)
}

func TestCleanStartupCloseCancelsObserveWithoutProviderMutation(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), cleanStartupConfig(fake))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := adapter.Observe(context.Background())
		result <- err
	}()
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, sipgateway.ErrClosed) {
			t.Fatalf("Observe error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel Observe")
	}
	assertNoIncomingMutation(t, fake)
}

func (f *fakeAsterisk) queueStartupIncoming(handle string) {
	f.mu.Lock()
	f.channels[handle] = "Ring"
	f.mu.Unlock()
	channel := f.incomingChannel(handle, "Ring")
	_ = f.sendEvent(ariEvent{
		Type: "StasisStart", Application: f.app, Args: []string{"incoming"}, Channel: &channel,
	})
}

func assertNoIncomingMutation(t *testing.T, fake *fakeAsterisk) {
	t.Helper()
	if fake.answerCalls.Load() != 0 || fake.endCalls.Load() != 0 {
		t.Fatalf("provider mutations answer=%d end=%d", fake.answerCalls.Load(), fake.endCalls.Load())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, request := range fake.requestLog {
		if strings.HasSuffix(request, "/answer") || strings.HasPrefix(request, "DELETE channels/") {
			t.Fatalf("unexpected provider mutation request=%q", request)
		}
	}
}
