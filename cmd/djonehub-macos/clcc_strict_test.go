package main

import "testing"

func TestParseCLCCForMutationAcceptsCompleteVoiceSnapshots(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     []parsedCall
	}{
		{
			name:     "empty without echo",
			response: "OK",
		},
		{
			name:     "active with echo and number",
			response: "AT+CLCC\r\n+CLCC: 1,1,0,0,0,\"10086\",129\r\nOK\r\n",
			want: []parsedCall{{
				Index: 1, Direction: "incoming", State: "active", Number: "10086",
			}},
		},
		{
			name:     "waiting without number",
			response: "+clcc: 2, 1, 5, 0, 0\nOK",
			want: []parsedCall{{
				Index: 2, Direction: "incoming", State: "waiting",
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCLCCForMutation(test.response)
			if err != nil {
				t.Fatalf("parseCLCCForMutation() error = %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("parseCLCCForMutation() = %+v, want %+v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("parseCLCCForMutation()[%d] = %+v, want %+v", i, got[i], test.want[i])
				}
			}
		})
	}
}

func TestParseCLCCForMutationFiltersStrictQDC507DataContexts(t *testing.T) {
	response := "AT+CLCC\r\n" +
		"+CLCC: 1,1,0,1,0,\"\",128\r\n" +
		"+CLCC: 2,1,0,1,0,\"\",128\r\n" +
		"+CLCC: 3,1,4,0,0,\"10086\",129\r\nOK\r\n"
	calls, err := parseCLCCForMutation(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Index != 3 || calls[0].State != "incoming" || calls[0].Number != "10086" {
		t.Fatalf("voice calls = %+v", calls)
	}

	dataOnly := "+CLCC: 1,1,0,1,0,\"\",128\r\n+CLCC: 2,1,0,1,0,\"\",128\r\nOK"
	calls, err = parseCLCCForMutation(dataOnly)
	if err != nil || len(calls) != 0 {
		t.Fatalf("data-only CLCC = %+v, %v; want voice-idle", calls, err)
	}
}

func TestValidateCLCCVoiceIdleForModuleSetupAllowsOnlyStrictNonVoiceRecords(t *testing.T) {
	accepted := []string{
		"AT+CLCC\r\n+CLCC: 1,1,0,1,0\r\n+CLCC: 2,1,0,1,0\r\nOK\r\n",
		"+CLCC: 4,0,0,2,0\r\nOK",
	}
	for _, response := range accepted {
		if err := validateCLCCVoiceIdleForModuleSetup(response); err != nil {
			t.Fatalf("validateCLCCVoiceIdleForModuleSetup() error = %v", err)
		}
	}

	rejected := []string{
		"+CLCC: 1,1,0,1,0\r\n+CLCC: 3,1,5,0,0,\"10086\",129\r\nOK",
		"+CLCC: 1,1,0,1,0\r\n+CLCC: garbage\r\nOK",
		"+CLCC: 1,1,0,1,0\r\nRING\r\nOK",
		"+CLCC: 1,1,0,1,0\r\n+CLCC: 1,0,0,2,0\r\nOK",
		"+CLCC: 1,1,0,3,0\r\nOK",
		"+CLCC: 1,1,0,1,0",
	}
	for _, response := range rejected {
		if err := validateCLCCVoiceIdleForModuleSetup(response); err == nil {
			t.Fatal("validateCLCCVoiceIdleForModuleSetup() accepted unsafe response")
		}
	}
}

func TestParseCLCCForMutationRejectsAnythingAmbiguous(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "missing terminal OK", response: "+CLCC: 1,1,0,0,0"},
		{name: "error instead of OK", response: "+CLCC: 1,1,0,0,0\r\nERROR"},
		{name: "valid prefix with trailing garbage", response: "+CLCC: 1,1,0,0,0 garbage\r\nOK"},
		{name: "partial row", response: "+CLCC: 1,1,0\r\nOK"},
		{name: "unknown direction", response: "+CLCC: 1,2,0,0,0\r\nOK"},
		{name: "unknown state", response: "+CLCC: 1,1,6,0,0\r\nOK"},
		{name: "unknown mode", response: "+CLCC: 1,1,0,9,0\r\nOK"},
		{name: "multiparty call", response: "+CLCC: 1,1,0,0,1\r\nOK"},
		{name: "invalid number type", response: "+CLCC: 1,1,0,0,0,\"\",999\r\nOK"},
		{name: "duplicate index", response: "+CLCC: 1,1,0,0,0\r\n+CLCC: 1,0,1,0,0\r\nOK"},
		{name: "extra malformed CLCC row", response: "+CLCC: 1,1,0,0,0\r\n+CLCC: garbage\r\nOK"},
		{name: "unrelated unsolicited line", response: "+CLCC: 1,1,0,0,0\r\nRING\r\nOK"},
		{name: "content after OK", response: "+CLCC: 1,1,0,0,0\r\nOK\r\n+CLCC: 2,1,0,0,0"},
		{name: "misplaced echo", response: "+CLCC: 1,1,0,0,0\r\nAT+CLCC\r\nOK"},
		{name: "zero index", response: "+CLCC: 0,1,0,0,0\r\nOK"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := parseCLCCForMutation(test.response); err == nil {
				t.Fatalf("parseCLCCForMutation() = %+v, want error", got)
			}
			// The legacy no-error entry point is used by idle-only mutation
			// gates. It must never turn invalid input into an empty snapshot.
			if got := parseCLCC(test.response); len(got) == 0 {
				t.Fatal("parseCLCC() turned an invalid response into safe idle")
			}
		})
	}
}

func TestParseCLCCForDisplayRemainsTolerant(t *testing.T) {
	response := "RING\r\n+CLCC: 3,1,4,0,0,\"\",129 trailing text\r\nOK"
	got := parseCLCCForDisplay(response)
	if len(got) != 1 || got[0].Index != 3 || got[0].Direction != "incoming" || got[0].State != "incoming" {
		t.Fatalf("parseCLCCForDisplay() = %+v, want tolerant incoming call", got)
	}
}
