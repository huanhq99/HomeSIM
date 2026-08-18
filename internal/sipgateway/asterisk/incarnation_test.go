package asterisk

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

func TestLiveInspectRejectsReplacementPBX404BeforeOldSocketEOF(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	directBefore := fake.directRESTCalls.Load()

	// Model an application-level proxy trying to keep the old downstream
	// WebSocket open while answering its next RESTRequest from a replacement PBX.
	// The response itself travels on the old socket, so EOF has not happened yet.
	fake.mu.Lock()
	fake.restRequestHook = func(_ int, request ariRESTRequest) (*ariRESTResponse, bool) {
		if request.Method != http.MethodGet || request.URI != "channels/"+incoming.Ref.ProviderHandle {
			return nil, false
		}
		return fakeRESTResponse(fake, request, http.StatusNotFound, "not found\n", "02:11:22:33:44:66"), true
	}
	fake.mu.Unlock()

	result, err := adapter.Inspect(context.Background(), incoming.Ref)
	if result.State != "" || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("replacement 404 result=%+v err=%v", result, err)
	}
	adapter.mu.Lock()
	current := adapter.calls[incoming.Ref.ProviderHandle]
	adapter.mu.Unlock()
	if current.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("replacement 404 became ended: %+v", current)
	}
	if fake.directRESTCalls.Load() != directBefore || fake.answerCalls.Load() != 0 || fake.endCalls.Load() != 0 {
		t.Fatalf("HTTP fallback or mutation: direct=%d want=%d answer=%d end=%d",
			fake.directRESTCalls.Load(), directBefore, fake.answerCalls.Load(), fake.endCalls.Load())
	}
	select {
	case <-adapter.done:
	case <-time.After(time.Second):
		t.Fatal("replacement identity did not terminally stop Adapter")
	}
}

func TestLiveInspectTreats404AsEndedOnlyOnVerifiedEventSocket(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	directBefore := fake.directRESTCalls.Load()
	fake.mu.Lock()
	delete(fake.channels, incoming.Ref.ProviderHandle)
	fake.mu.Unlock()

	ended, err := adapter.Inspect(context.Background(), incoming.Ref)
	if err != nil || ended.State != sipgateway.ProviderCallEnded {
		t.Fatalf("bound 404 result=%+v err=%v", ended, err)
	}
	if fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("bound Inspect used HTTP fallback: got=%d want=%d", fake.directRESTCalls.Load(), directBefore)
	}
}

func TestLiveMutationRejectsReusedChannelIdentityBeforeUnknownOrPOST(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	directBefore := fake.directRESTCalls.Load()
	fake.mu.Lock()
	fake.inspectMutate = func(channel *ariChannel) {
		channel.CreationTime = "2026-08-14T01:04:04.000+00:00"
		channel.ProtocolID = "replacement-dialog@pbx.invalid"
	}
	fake.mu.Unlock()

	result, err := adapter.AnswerIncoming(context.Background(), sipgateway.AnswerIncomingRequest{
		Call: incoming.Ref, CommandID: "reuse-answer-command", MediaLeaseID: media.ID(),
	})
	if result.Outcome != "" || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("reused-channel answer=%+v err=%v", result, err)
	}
	adapter.mu.Lock()
	commandCount := len(adapter.commands)
	adapter.mu.Unlock()
	if commandCount != 0 || fake.answerCalls.Load() != 0 {
		t.Fatalf("reused channel reached mutation: commands=%d answers=%d", commandCount, fake.answerCalls.Load())
	}
	if fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("reused channel caused HTTP fallback: got=%d want=%d", fake.directRESTCalls.Load(), directBefore)
	}
	select {
	case <-adapter.done:
	case <-time.After(time.Second):
		t.Fatal("reused channel identity did not terminally stop Adapter")
	}
}

func TestPrepareMediaRejectsReplacementIdentityBeforeCreatingResources(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	directBefore := fake.directRESTCalls.Load()
	fake.mu.Lock()
	fake.restResponseHook = func(_ int, response *ariRESTResponse) {
		if response.URI == "channels/"+incoming.Ref.ProviderHandle {
			response.AsteriskID = "02:11:22:33:44:66"
		}
	}
	fake.mu.Unlock()

	media, err := adapter.PrepareMedia(context.Background(), sipgateway.PrepareMediaRequest{
		Call: incoming.Ref, Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	})
	if media != nil || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("replacement PrepareMedia media=%v err=%v", media, err)
	}
	fake.mu.Lock()
	mediaChannelID, bridgeCount := fake.mediaChannelID, len(fake.bridges)
	fake.mu.Unlock()
	if mediaChannelID != "" || bridgeCount != 0 || fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("replacement PrepareMedia created/fell back: media=%q bridges=%d direct=%d want=%d",
			mediaChannelID, bridgeCount, fake.directRESTCalls.Load(), directBefore)
	}
}

func TestEndActiveRejectsReusedChannelIdentityBeforeUnknownOrDELETE(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	answered, err := adapter.AnswerIncoming(context.Background(), sipgateway.AnswerIncomingRequest{
		Call: incoming.Ref, CommandID: "end-reuse-answer-command", MediaLeaseID: media.ID(),
	})
	if err != nil || answered.Outcome != sipgateway.CommandApplied || answered.Current == nil {
		t.Fatalf("answer=%+v err=%v", answered, err)
	}
	directBefore := fake.directRESTCalls.Load()
	fake.mu.Lock()
	fake.inspectMutate = func(channel *ariChannel) {
		channel.CreationTime = "2026-08-14T01:04:04.000+00:00"
		channel.ProtocolID = "replacement-dialog@pbx.invalid"
	}
	fake.mu.Unlock()

	result, err := adapter.EndActive(context.Background(), sipgateway.EndActiveRequest{
		Call: answered.Current.Ref, CommandID: "reuse-end-command",
	})
	if result.Outcome != "" || !errors.Is(err, sipgateway.ErrRecoveryRequired) {
		t.Fatalf("reused-channel end=%+v err=%v", result, err)
	}
	if fake.endCalls.Load() != 0 || fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("reused channel reached DELETE/fallback: end=%d direct=%d want=%d",
			fake.endCalls.Load(), fake.directRESTCalls.Load(), directBefore)
	}
	adapter.mu.Lock()
	record, recorded := adapter.commands["reuse-end-command"]
	adapter.mu.Unlock()
	if recorded || record.result.Outcome != "" {
		t.Fatalf("preflight failure stored mutation command: recorded=%t result=%+v", recorded, record.result)
	}
}

func TestLiveRESTResponseCorrelationViolationsAreTerminal(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ariRESTResponse)
	}{
		{name: "wrong asterisk identity", mutate: func(response *ariRESTResponse) {
			response.AsteriskID = "02:11:22:33:44:66"
		}},
		{name: "wrong uri", mutate: func(response *ariRESTResponse) {
			response.URI = "channels/unrelated-channel"
		}},
		{name: "unknown request id", mutate: func(response *ariRESTResponse) {
			response.RequestID = "unmatched-request-id"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			adapter, incoming := newObservedIncoming(t, fake)
			directBefore := fake.directRESTCalls.Load()
			fake.mu.Lock()
			fake.restResponseHook = func(_ int, response *ariRESTResponse) {
				if response.URI == "channels/"+incoming.Ref.ProviderHandle {
					test.mutate(response)
				}
			}
			fake.mu.Unlock()

			if _, err := adapter.Inspect(context.Background(), incoming.Ref); !errors.Is(err, sipgateway.ErrRecoveryRequired) {
				t.Fatalf("correlation error=%v", err)
			}
			if fake.directRESTCalls.Load() != directBefore || fake.answerCalls.Load() != 0 || fake.endCalls.Load() != 0 {
				t.Fatalf("correlation failure fell back or mutated: direct=%d want=%d answer=%d end=%d",
					fake.directRESTCalls.Load(), directBefore, fake.answerCalls.Load(), fake.endCalls.Load())
			}
			select {
			case <-adapter.done:
			case <-time.After(time.Second):
				t.Fatal("correlation failure did not terminally stop Adapter")
			}
		})
	}
}

func TestRESTResponseURICorrelationUsesEquivalentEscapingOnly(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{name: "exact", left: "asterisk/info?only=system%2Cstatus", right: "asterisk/info?only=system%2Cstatus", want: true},
		{name: "comma escape", left: "asterisk/info?only=system,status", right: "asterisk/info?only=system%2Cstatus", want: true},
		{name: "query order", left: "bridges/id?type=mixing&x=1", right: "bridges/id?x=1&type=mixing", want: true},
		{name: "different value", left: "asterisk/info?only=system", right: "asterisk/info?only=system%2Cstatus"},
		{name: "response omits query", left: "asterisk/info", right: "asterisk/info?only=system%2Cstatus", want: true},
		{name: "response adds query", left: "asterisk/info?only=system", right: "asterisk/info"},
		{name: "different path", left: "channels/id", right: "bridges/id"},
		{name: "encoded separator", left: "channels%2Fid", right: "channels/id"},
		{name: "fragment", left: "channels/id#unsafe", right: "channels/id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := equivalentRESTResponseURI(test.left, test.right); got != test.want {
				t.Fatalf("equivalentRESTResponseURI()=%t want %t", got, test.want)
			}
		})
	}
}

func TestEventSocketLossNeverFallsBackForCleanupAndConcurrentClose(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	_ = prepareMedia(t, adapter, incoming)
	directBefore := fake.directRESTCalls.Load()
	fake.mu.Lock()
	eventWS := fake.eventWS
	mediaChannelID := fake.mediaChannelID
	fake.mu.Unlock()
	if eventWS == nil || mediaChannelID == "" {
		t.Fatal("fake sockets/resources were not prepared")
	}
	if err := eventWS.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adapter.done:
	case <-time.After(time.Second):
		t.Fatal("event WebSocket loss did not stop Adapter")
	}

	const closers = 16
	var wait sync.WaitGroup
	wait.Add(closers)
	errorsSeen := make(chan error, closers)
	for index := 0; index < closers; index++ {
		go func() {
			defer wait.Done()
			errorsSeen <- adapter.Close()
		}()
	}
	waitDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Close deadlocked after event socket loss")
	}
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("Close error=%v", err)
		}
	}
	if fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("failed cleanup opened HTTP fallback: got=%d want=%d", fake.directRESTCalls.Load(), directBefore)
	}
	fake.mu.Lock()
	_, mediaStillPresent := fake.channels[mediaChannelID]
	bridgeCount := len(fake.bridges)
	fake.mu.Unlock()
	if !mediaStillPresent || bridgeCount == 0 {
		t.Fatalf("test did not preserve failed-socket orphan evidence: media=%t bridges=%d", mediaStillPresent, bridgeCount)
	}
}

func fakeRESTResponse(
	fake *fakeAsterisk,
	request ariRESTRequest,
	status int,
	body string,
	asteriskID string,
) *ariRESTResponse {
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	return &ariRESTResponse{
		Type: "RESTResponse", TransactionID: request.TransactionID,
		RequestID: request.RequestID, StatusCode: status, ReasonPhrase: http.StatusText(status),
		URI: request.URI, ContentType: contentType, MessageBody: body,
		Timestamp: "2026-08-14T01:03:03.000+00:00", AsteriskID: asteriskID,
		Application: fake.app,
	}
}

func TestHealthyCloseCleansOwnedResourcesOnlyOverBoundWebSocket(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	_ = prepareMedia(t, adapter, incoming)
	directBefore := fake.directRESTCalls.Load()
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	if fake.directRESTCalls.Load() != directBefore {
		t.Fatalf("healthy cleanup used HTTP: got=%d want=%d", fake.directRESTCalls.Load(), directBefore)
	}
	fake.mu.Lock()
	remainingMedia := false
	for channelID := range fake.channels {
		remainingMedia = remainingMedia || strings.HasPrefix(channelID, "dj1m_")
	}
	bridgeCount := len(fake.bridges)
	_, incomingPresent := fake.channels[incoming.Ref.ProviderHandle]
	fake.mu.Unlock()
	if remainingMedia || bridgeCount != 0 || !incomingPresent {
		t.Fatalf("healthy cleanup media=%t bridges=%d incoming=%t", remainingMedia, bridgeCount, incomingPresent)
	}
}
