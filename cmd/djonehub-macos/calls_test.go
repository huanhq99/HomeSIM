package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseCLCCIncomingCall(t *testing.T) {
	got := parseCLCC("AT+CLCC\r\n+CLCC: 1,1,4,0,0,\"13800138000\",129\r\nOK")
	if len(got) != 1 {
		t.Fatalf("parseCLCC() len=%d, want 1", len(got))
	}
	if got[0].Index != 1 || got[0].Direction != "incoming" ||
		got[0].State != "incoming" || got[0].Number != "13800138000" {
		t.Fatalf("parseCLCC()=%+v", got[0])
	}
}

func TestParseCLCCNoCall(t *testing.T) {
	if got := parseCLCC("AT+CLCC\r\nOK"); len(got) != 0 {
		t.Fatalf("parseCLCC()=%+v, want empty", got)
	}
}

func TestParseCLCCIgnoresDataSession(t *testing.T) {
	response := "AT+CLCC\r\n+CLCC: 2,1,0,1,0,\"\",128\r\nOK"
	if got := parseCLCCForDisplay(response); len(got) != 0 {
		t.Fatalf("parseCLCCForDisplay()=%+v, want data session ignored", got)
	}
}

func TestValidateCallATResponseRejectsModemError(t *testing.T) {
	if err := validateCallATResponse("ATD10086;\r\nERROR"); err == nil {
		t.Fatal("validateCallATResponse accepted ERROR reply")
	}
	if err := validateCallATResponse("ATD10086;\r\nOK"); err != nil {
		t.Fatalf("validateCallATResponse rejected OK reply: %v", err)
	}
	if err := validateCallATResponse("ATD10086;\r\n"); err == nil {
		t.Fatal("validateCallATResponse accepted incomplete reply")
	}
}

func TestCallLifecycleMarksMissed(t *testing.T) {
	a := &app{callPollInterval: 3 * time.Second, callNotifier: func(callRecord) {}}
	started := time.Date(2026, 7, 26, 10, 0, 0, 0, time.Local)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, started)
	a.applyCallPoll(nil, started.Add(8*time.Second))

	if a.activeCall != nil {
		t.Fatal("active call was not cleared")
	}
	if len(a.callHistory) != 1 || !a.callHistory[0].Missed {
		t.Fatalf("history=%+v, want one missed call", a.callHistory)
	}
}

func TestAnsweredCallIsNotMissed(t *testing.T) {
	a := &app{callPollInterval: 3 * time.Second, callNotifier: func(callRecord) {}}
	started := time.Date(2026, 7, 26, 10, 0, 0, 0, time.Local)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, started)
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "active", Number: "10086",
	}}, started.Add(3*time.Second))
	a.applyCallPoll(nil, started.Add(8*time.Second))

	if len(a.callHistory) != 1 || a.callHistory[0].Missed {
		t.Fatalf("history=%+v, want answered call", a.callHistory)
	}
}

func TestCallTopologyFingerprintIsOrderIndependent(t *testing.T) {
	calls := []parsedCall{
		{Index: 2, Direction: "incoming", State: "waiting"},
		{Index: 1, Direction: "outgoing", State: "active"},
	}
	reversed := []parsedCall{calls[1], calls[0]}
	if got, want := callTopologyFingerprint(calls), callTopologyFingerprint(reversed); got != want {
		t.Fatalf("fingerprint depends on CLCC order: %q != %q", got, want)
	}
}

func TestSingleActiveCallMatchesExactIndexAndDirection(t *testing.T) {
	expected := callMediaTicket{Generation: 7, CallID: "call-1", Index: 1, Direction: "incoming"}
	tests := []struct {
		name  string
		calls []parsedCall
		want  bool
	}{
		{name: "same call", calls: []parsedCall{{Index: 1, Direction: "incoming", State: "active"}}, want: true},
		{name: "wrong index", calls: []parsedCall{{Index: 2, Direction: "incoming", State: "active"}}},
		{name: "wrong direction", calls: []parsedCall{{Index: 1, Direction: "outgoing", State: "active"}}},
		{name: "not active", calls: []parsedCall{{Index: 1, Direction: "incoming", State: "held"}}},
		{name: "two calls", calls: []parsedCall{
			{Index: 1, Direction: "incoming", State: "active"},
			{Index: 2, Direction: "outgoing", State: "held"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := singleActiveCallMatches(test.calls, expected); got != test.want {
				t.Fatalf("singleActiveCallMatches()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestCallTopologyGenerationEligibilityAndMultiCallTeardown(t *testing.T) {
	var stopped []uint64
	a := &app{
		callNotifier: func(callRecord) {},
		callMediaRouteStop: func(generation uint64) error {
			stopped = append(stopped, generation)
			return nil
		},
	}
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local)
	incoming := []parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10086"}}
	active := []parsedCall{{Index: 1, Direction: "incoming", State: "active", Number: "10086"}}

	a.applyCallPoll(incoming, now)
	if a.callGeneration != 1 || a.callMediaEligible {
		t.Fatalf("incoming generation=%d eligible=%v, want 1/false", a.callGeneration, a.callMediaEligible)
	}
	a.applyCallPoll(active, now.Add(time.Second))
	if a.callGeneration != 2 || !a.callMediaEligible {
		t.Fatalf("first active generation=%d eligible=%v, want 2/true", a.callGeneration, a.callMediaEligible)
	}
	ticket, ok := a.currentCallMediaTicket()
	if !ok || ticket.Generation != 2 || ticket.Index != 1 || ticket.Direction != "incoming" {
		t.Fatalf("first active ticket=%+v ok=%v", ticket, ok)
	}
	if got := stopped; len(got) != 1 || got[0] != 1 {
		t.Fatalf("transition teardown=%v, want [1]", got)
	}

	// An identical CLCC snapshot is not a topology change.
	a.applyCallPoll(active, now.Add(2*time.Second))
	if a.callGeneration != 2 || len(stopped) != 1 {
		t.Fatalf("stable active generation=%d teardown=%v, want 2/[1]", a.callGeneration, stopped)
	}

	// Adding any second voice call invalidates exact-one eligibility and tears
	// down the route belonging to the prior generation.
	multiple := []parsedCall{
		{Index: 1, Direction: "incoming", State: "active", Number: "10086"},
		{Index: 2, Direction: "outgoing", State: "held", Number: "10010"},
	}
	a.applyCallPoll(multiple, now.Add(3*time.Second))
	if a.callGeneration != 3 || a.callMediaEligible {
		t.Fatalf("multi-call generation=%d eligible=%v, want 3/false", a.callGeneration, a.callMediaEligible)
	}
	if _, ok := a.currentCallMediaTicket(); ok {
		t.Fatal("multi-call topology produced a media ticket")
	}
	if got := stopped; len(got) != 2 || got[1] != 2 {
		t.Fatalf("multi-call teardown=%v, want [1 2]", got)
	}

	// A state change inside the ineligible topology is still a new generation
	// and must run teardown for that immediately preceding generation.
	multiple[1].State = "waiting"
	a.applyCallPoll(multiple, now.Add(4*time.Second))
	if a.callGeneration != 4 || a.callMediaEligible {
		t.Fatalf("changed multi-call generation=%d eligible=%v, want 4/false", a.callGeneration, a.callMediaEligible)
	}
	if got := stopped; len(got) != 3 || got[2] != 3 {
		t.Fatalf("changed multi-call teardown=%v, want [1 2 3]", got)
	}
}

func TestOldGenerationStopRechecksAfterOperationBarrier(t *testing.T) {
	a := &app{}
	a.moduleVoiceOpMu.Lock() // model a newer start currently owning the operation lock
	a.moduleVoiceMu.Lock()
	a.moduleVoiceCallGeneration = 1
	a.moduleVoiceMu.Unlock()

	entered := make(chan struct{})
	done := make(chan error, 1)
	var physicalStops atomic.Int32
	go func() {
		close(entered)
		done <- a.stopModuleVoiceRouteForCallWith(1, func() error {
			physicalStops.Add(1)
			return nil
		})
	}()
	<-entered

	// The newer start publishes its generation before releasing opMu. The old
	// stop must observe generation 2 only after it acquires the same barrier.
	a.moduleVoiceMu.Lock()
	a.moduleVoiceCallGeneration = 2
	a.moduleVoiceMu.Unlock()
	a.moduleVoiceOpMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("old stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old stop did not acquire operation barrier")
	}
	if got := physicalStops.Load(); got != 0 {
		t.Fatalf("old stop invoked physical teardown %d time(s), want 0", got)
	}
}

func TestMatchingGenerationStopRunsOnce(t *testing.T) {
	a := &app{moduleVoiceCallGeneration: 9}
	var physicalStops atomic.Int32
	if err := a.stopModuleVoiceRouteForCallWith(9, func() error {
		physicalStops.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("matching stop returned error: %v", err)
	}
	if got := physicalStops.Load(); got != 1 {
		t.Fatalf("matching stop invoked physical teardown %d time(s), want 1", got)
	}
}

func TestVoiceStartRequiresExplicitConfirmationAndExactActiveCall(t *testing.T) {
	a := &app{}
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "missing confirm", body: `{}`, want: http.StatusBadRequest},
		{name: "no exact active call", body: `{"confirm":true}`, want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/voice/start", strings.NewReader(test.body))
			a.voiceStartAPI(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), test.want)
			}
		})
	}
}

func TestNormalizeDialNumber(t *testing.T) {
	cases := map[string]string{
		"13800138000":        "13800138000",
		"+86 138-0013(8000)": "+8613800138000",
		"10086":              "10086",
		"*100#":              "*100#",
		"":                   "",
		"   ":                "",
		"12a45":              "",
		"tel:+8613800138000": "",
		"1-800-FLOWERS":      "",
	}
	for input, want := range cases {
		if got := normalizeDialNumber(input); got != want {
			t.Fatalf("normalizeDialNumber(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestDirectCallRedialCooldownStartsFromConfirmedVoiceIdle(t *testing.T) {
	ended := time.Unix(1700000000, 0)
	a := &app{callHistory: []callRecord{{EndedAt: &ended}}}
	if got := a.directCallRedialCooldownRemaining(ended.Add(2 * time.Second)); got != 3*time.Second {
		t.Fatalf("remaining=%s, want 3s", got)
	}
	if got := a.directCallRedialCooldownRemaining(ended.Add(directCallRedialCooldown)); got != 0 {
		t.Fatalf("remaining=%s, want 0", got)
	}
	a.callHistory = nil
	if got := a.directCallRedialCooldownRemaining(ended); got != 0 {
		t.Fatalf("empty history remaining=%s, want 0", got)
	}
}

func TestDialAndAnswerFailClosedDuringModuleMutation(t *testing.T) {
	a := &app{}
	a.moduleMutationMu.Lock()
	defer a.moduleMutationMu.Unlock()

	dialRecorder := httptest.NewRecorder()
	dialRequest := httptest.NewRequest(http.MethodPost, "/api/calls/dial", strings.NewReader(`{"number":"10086"}`))
	a.dialCall(dialRecorder, dialRequest)
	if dialRecorder.Code != http.StatusConflict {
		t.Fatalf("dial status=%d body=%s", dialRecorder.Code, dialRecorder.Body.String())
	}

	answerRecorder := httptest.NewRecorder()
	answerRequest := httptest.NewRequest(http.MethodPost, "/api/calls/answer", nil)
	a.answerCall(answerRecorder, answerRequest)
	if answerRecorder.Code != http.StatusConflict {
		t.Fatalf("answer status=%d body=%s", answerRecorder.Code, answerRecorder.Body.String())
	}
}

func TestQDC507DTMFUsesFastCommandBeforeCompatibilityFallback(t *testing.T) {
	var commands []string
	run := func(command string, timeout time.Duration) (string, error) {
		commands = append(commands, command)
		if command == "AT+CLDTMF=1,\"1\"" {
			return command + "\r\nOK\r\n", nil
		}
		return command + "\r\nERROR\r\n", nil
	}
	if err := sendCallDTMFWith(run, "1"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(commands, "|"), "AT+CLDTMF=1,\"1\""; got != want {
		t.Fatalf("commands=%q want=%q", got, want)
	}

	commands = nil
	run = func(command string, timeout time.Duration) (string, error) {
		commands = append(commands, command)
		if len(commands) == 1 {
			return command + "\r\nERROR\r\n", nil
		}
		return command + "\r\nOK\r\n", nil
	}
	if err := sendCallDTMFWith(run, "2"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(commands, "|"), "AT+CLDTMF=1,\"2\"|AT+VTS=\"2\""; got != want {
		t.Fatalf("fallback commands=%q want=%q", got, want)
	}
}

func TestQDC507DTMFSequenceBatchesDigitsAndQuotesHash(t *testing.T) {
	var commands []string
	run := func(command string, timeout time.Duration) (string, error) {
		commands = append(commands, command)
		if timeout < 4*time.Second {
			t.Fatalf("sequence timeout=%s is too short", timeout)
		}
		return command + "\r\nOK\r\n", nil
	}
	if err := sendCallDTMFSequenceWith(run, "13800138000#"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(commands, "|"), `AT+CLDTMF=1,"1,3,8,0,0,1,3,8,0,0,0,#"`; got != want {
		t.Fatalf("commands=%q want=%q", got, want)
	}
}
