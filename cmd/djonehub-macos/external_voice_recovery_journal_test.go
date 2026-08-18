package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

func TestExternalVoiceMutationJournalBindsAndResolvesExactIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-voice", "recovery.json")
	store, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	journal, err := newExternalVoiceMutationJournal(
		store,
		"synthetic-gateway",
		bytes.NewReader(bytes.Repeat([]byte{0x41}, externalVoiceRecoveryIDBytes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	bodyHash := sha256.Sum256([]byte("synthetic exact request bytes"))
	evidence, err := journal.Evidence(
		sipgateway.CommandAnswerIncoming,
		" Synthetic-Owner@Example.Invalid ",
		bodyHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := journal.Evidence(
		sipgateway.CommandAnswerIncoming,
		"synthetic-owner@example.invalid",
		bodyHash,
	)
	if err != nil || canonical != evidence {
		t.Fatalf("canonical evidence mismatch: %v", err)
	}
	otherKind, err := journal.Evidence(
		sipgateway.CommandEndActive,
		"synthetic-owner@example.invalid",
		bodyHash,
	)
	if err != nil || otherKind.RequestDigest == evidence.RequestDigest {
		t.Fatalf("command kind was not bound: %v", err)
	}
	var token sipgateway.RecoveryToken
	providerMarker := []byte("synthetic-private-provider-token-marker")
	if err := token.UnmarshalBinary(providerMarker); err != nil {
		t.Fatal(err)
	}
	pending := sipgateway.PendingMutation{
		Kind:               sipgateway.CommandAnswerIncoming,
		CommandID:          "synthetic-command-0001",
		Call:               sipgateway.PublicCallRef{PublicCallID: strings.Repeat("a", 43), Generation: 7},
		ExpectedRevision:   11,
		PublicMediaLeaseID: "pml_synthetic_0001",
		ProviderToken:      token,
		Evidence:           evidence,
	}
	recoveryID, err := journal.Arm(pending)
	if err != nil || !externalVoiceRecoveryIDPattern.MatchString(recoveryID) {
		t.Fatalf("Arm recoveryID=%q err=%v", recoveryID, err)
	}
	snapshot, err := store.Snapshot()
	if err != nil || snapshot.State != externalVoiceRecoveryArmed ||
		snapshot.PublicCallID != pending.Call.PublicCallID ||
		!bytes.Equal(snapshot.ProviderToken, providerMarker) {
		t.Fatalf("armed snapshot=%+v err=%v", snapshot, err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		string(providerMarker),
		"synthetic-owner@example.invalid",
		pending.CommandID,
	} {
		if strings.Contains(string(disk), marker) {
			t.Fatalf("recovery disk record leaked marker %q", marker)
		}
	}
	if err := journal.Resolve(recoveryID, sipgateway.MutationResolution{
		Outcome: sipgateway.CommandApplied,
		State:   sipgateway.ProviderCallActive,
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Snapshot()
	if err != nil || resolved.State != externalVoiceRecoveryResolved ||
		resolved.Resolution != externalVoiceRecoveryApplied || len(resolved.ProviderToken) != 0 {
		t.Fatalf("resolved snapshot=%+v err=%v", resolved, err)
	}
}

func TestExternalVoiceMutationJournalFailsClosedAndZerosKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-voice", "recovery.json")
	store, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	journal, err := newExternalVoiceMutationJournal(store, "synthetic-gateway", strings.NewReader("short"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := journal.Evidence(
		sipgateway.CommandAnswerIncoming,
		"synthetic-owner@example.invalid",
		sha256.Sum256([]byte("synthetic request")),
	)
	if err != nil {
		t.Fatal(err)
	}
	var token sipgateway.RecoveryToken
	if err := token.UnmarshalBinary([]byte("synthetic-token")); err != nil {
		t.Fatal(err)
	}
	_, err = journal.Arm(sipgateway.PendingMutation{
		Kind:               sipgateway.CommandAnswerIncoming,
		CommandID:          "synthetic-command-0002",
		Call:               sipgateway.PublicCallRef{PublicCallID: strings.Repeat("b", 43), Generation: 1},
		ExpectedRevision:   1,
		PublicMediaLeaseID: "pml_synthetic_0002",
		ProviderToken:      token,
		Evidence:           evidence,
	})
	if !errors.Is(err, errExternalVoiceUnavailable) {
		t.Fatalf("short ID reader error=%v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if journal.ownerKey != ([sha256.Size]byte{}) || journal.requestKey != ([sha256.Size]byte{}) ||
		journal.commandKey != ([sha256.Size]byte{}) {
		t.Fatal("journal Close did not clear derived keys")
	}
	if _, err := journal.Evidence(
		sipgateway.CommandAnswerIncoming,
		"synthetic-owner@example.invalid",
		sha256.Sum256([]byte("synthetic request")),
	); !errors.Is(err, errExternalVoiceClosed) {
		t.Fatalf("Evidence after Close error=%v", err)
	}
}

func TestExternalVoiceRecoveryResolutionBindsKindOutcomeAndState(t *testing.T) {
	tests := []struct {
		name       string
		kind       externalVoiceRecoveryKind
		resolution sipgateway.MutationResolution
		want       externalVoiceRecoveryResolution
		valid      bool
	}{
		{
			name: "answer applied active",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandApplied,
				State:   sipgateway.ProviderCallActive,
			},
			want:  externalVoiceRecoveryApplied,
			valid: true,
		},
		{
			name: "answer rejected incoming",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandRejected,
				State:   sipgateway.ProviderCallIncoming,
			},
			want:  externalVoiceRecoveryRejected,
			valid: true,
		},
		{
			name: "answer reconciled active",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandUnknown,
				State:   sipgateway.ProviderCallActive,
			},
			want:  externalVoiceRecoveryApplied,
			valid: true,
		},
		{
			name: "answer provider ended",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandUnknown,
				State:   sipgateway.ProviderCallEnded,
			},
			want:  externalVoiceRecoveryProviderEnded,
			valid: true,
		},
		{
			name: "end applied ended",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandApplied,
				State:   sipgateway.ProviderCallEnded,
			},
			want:  externalVoiceRecoveryApplied,
			valid: true,
		},
		{
			name: "end rejected active",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandRejected,
				State:   sipgateway.ProviderCallActive,
			},
			want:  externalVoiceRecoveryRejected,
			valid: true,
		},
		{
			name: "end provider ended",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandUnknown,
				State:   sipgateway.ProviderCallEnded,
			},
			want:  externalVoiceRecoveryProviderEnded,
			valid: true,
		},
		{
			name: "answer applied ended is cross-kind",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandApplied,
				State:   sipgateway.ProviderCallEnded,
			},
		},
		{
			name: "end applied active is cross-kind",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandApplied,
				State:   sipgateway.ProviderCallActive,
			},
		},
		{
			name: "answer rejected active is cross-kind",
			kind: externalVoiceRecoveryAnswerIncoming,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandRejected,
				State:   sipgateway.ProviderCallActive,
			},
		},
		{
			name: "end rejected incoming is cross-kind",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandRejected,
				State:   sipgateway.ProviderCallIncoming,
			},
		},
		{
			name: "end unknown active remains armed",
			kind: externalVoiceRecoveryEndActive,
			resolution: sipgateway.MutationResolution{
				Outcome: sipgateway.CommandUnknown,
				State:   sipgateway.ProviderCallActive,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := externalVoiceRecoveryResolutionFromMutation(test.kind, test.resolution)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("resolution=%q err=%v want=%q", got, err, test.want)
				}
				return
			}
			if !errors.Is(err, sipgateway.ErrInvalidMutationJournalRecord) {
				t.Fatalf("resolution=%q error=%v", got, err)
			}
		})
	}
}
