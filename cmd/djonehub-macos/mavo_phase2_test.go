package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type hermeticMavoPhase2Permit struct{}

func (hermeticMavoPhase2Permit) mavoPhase2MutationPermitMarker() {}

type fakeMavoPhase2Target struct {
	mu                    sync.Mutex
	ops                   []string
	failAt                map[string]error
	panicAt               map[string]bool
	attemptedFalseAt      map[string]bool
	preflightValue        mavoPhase2Preflight
	inspectionValue       mavoPhase2TargetInspection
	probeValue            mavoPhase2AudioProbe
	cleanupValue          mavoPhase2CleanupEvidence
	onStep                func(string)
	cleanupSawLiveContext bool
}

func newFakeMavoPhase2Target() *fakeMavoPhase2Target {
	identity := strings.Repeat("a", 64)
	return &fakeMavoPhase2Target{
		failAt:           make(map[string]error),
		panicAt:          make(map[string]bool),
		attemptedFalseAt: make(map[string]bool),
		preflightValue: mavoPhase2Preflight{
			DeviceIdentitySHA256: identity,
			USBLocationID:        0x01100000,
			USBLifecycle:         40,
			Firmware:             mavoPhase1Firmware,
			FactoryProfileExact:  true,
			NetworkReady:         true,
			VoiceIdle:            true,
			SMSMutationIdle:      true,
			KnownModulesAbsent:   true,
			RemoteRuntimeAbsent:  true,
			AudioRouteDisabled:   true,
			ExclusiveLeaseReady:  true,
		},
		inspectionValue: mavoPhase2TargetInspection{
			DeviceIdentitySHA256: identity,
			USBLocationID:        0x01100000,
			USBLifecycle:         41,
			TargetProfileExact:   true,
			ADBDescriptorValid:   true,
			UACDescriptorValid:   true,
			ADBRoot:              true,
			KernelRelease:        mavoPhase2KernelRelease,
			Architecture:         mavoPhase2Architecture,
			TTYGS0Present:        true,
			VocServerPresent:     true,
			AudioLibraryPresent:  true,
			ARMELLoaderPresent:   true,
			KnownModulesAbsent:   true,
		},
		probeValue: mavoPhase2AudioProbe{
			CardName:           mavoPhase2CardName,
			DevicePaths:        slices.Clone(mavoPhase2RequiredAudioDevices),
			APRModuleLoaded:    true,
			VoiceModuleLoaded:  true,
			UACDescriptorValid: true,
			USBLocationMatched: true,
			HelperRunning:      false,
			AudioRouteEnabled:  false,
			CallCommandIssued:  false,
			SMSMutationIssued:  false,
		},
		cleanupValue: mavoPhase2CleanupEvidence{
			DeviceIdentityMatched: true,
			FactoryProfileExact:   true,
			NetworkRecovered:      true,
			ModulesAbsent:         []string{"qdc507_aprv3", "qdc507_voice"},
			ArtifactsAbsent: []string{
				"mavo-pcm-bridge.armv7", "qdc507_aprv3.ko", "qdc507_voice.ko",
			},
			RemoteRuntimeAbsent: true,
			HelperAbsent:        true,
			AudioRouteDisabled:  true,
		},
	}
}

func (f *fakeMavoPhase2Target) step(name string) error {
	f.mu.Lock()
	f.ops = append(f.ops, name)
	panicNow := f.panicAt[name]
	err := f.failAt[name]
	hook := f.onStep
	f.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	if panicNow {
		panic("secret=phase2-panic-123456789012345 at " + name)
	}
	return err
}

func (f *fakeMavoPhase2Target) attempted(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.attemptedFalseAt[name]
}

func (f *fakeMavoPhase2Target) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

func (f *fakeMavoPhase2Target) preflight(context.Context) (mavoPhase2Preflight, error) {
	return f.preflightValue, f.step("preflight")
}

func (f *fakeMavoPhase2Target) activateTemporaryProfile(context.Context, mavoPhase2Preflight) (bool, error) {
	return f.attempted("activate"), f.step("activate")
}

func (f *fakeMavoPhase2Target) inspectTemporaryTarget(context.Context, mavoPhase2Preflight) (mavoPhase2TargetInspection, error) {
	return f.inspectionValue, f.step("inspect_target")
}

func (f *fakeMavoPhase2Target) pushArtifact(_ context.Context, artifact mavoPhase2Artifact) (bool, error) {
	name := "push:" + artifact.Spec.Name
	return f.attempted(name), f.step(name)
}

func (f *fakeMavoPhase2Target) artifactMatches(_ context.Context, artifact mavoPhase2ArtifactSpec) error {
	return f.step("verify_artifact:" + artifact.Name)
}

func (f *fakeMavoPhase2Target) loadModule(_ context.Context, artifact mavoPhase2ArtifactSpec) (bool, error) {
	name := "load:" + artifact.ModuleName
	return f.attempted(name), f.step(name)
}

func (f *fakeMavoPhase2Target) moduleLoaded(_ context.Context, name string) (bool, error) {
	err := f.step("verify_module:" + name)
	return err == nil, err
}

func (f *fakeMavoPhase2Target) probeAudio(context.Context, mavoPhase2TargetInspection) (mavoPhase2AudioProbe, error) {
	return f.probeValue, f.step("probe_audio")
}

func (f *fakeMavoPhase2Target) noteCleanupContext(ctx context.Context) {
	f.mu.Lock()
	f.cleanupSawLiveContext = f.cleanupSawLiveContext || ctx.Err() == nil
	f.mu.Unlock()
}

func (f *fakeMavoPhase2Target) unloadModule(ctx context.Context, name string) error {
	f.noteCleanupContext(ctx)
	return f.step("unload:" + name)
}

func (f *fakeMavoPhase2Target) removeArtifact(ctx context.Context, name string) error {
	f.noteCleanupContext(ctx)
	return f.step("remove:" + name)
}

func (f *fakeMavoPhase2Target) restoreFactoryProfile(ctx context.Context, _ mavoPhase2Preflight) error {
	f.noteCleanupContext(ctx)
	return f.step("restore_factory")
}

func (f *fakeMavoPhase2Target) verifyCleanup(
	ctx context.Context,
	_ mavoPhase2Preflight,
	_ []mavoPhase2ArtifactSpec,
) (mavoPhase2CleanupEvidence, error) {
	f.noteCleanupContext(ctx)
	return f.cleanupValue, f.step("verify_cleanup")
}

type fakeMavoPhase2EvidenceSink struct {
	mu      sync.Mutex
	ops     []string
	records []mavoPhase2Evidence
	failAt  string
	panicAt string
	path    string
}

func (s *fakeMavoPhase2EvidenceSink) arm(evidence mavoPhase2Evidence) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, "arm")
	if s.panicAt == "arm" {
		panic("secret=arm-123456789012345")
	}
	if s.failAt == "arm" {
		return "", errors.New("token=arm-secret-123456789012345")
	}
	if s.path == "" {
		s.path = "/private/mavo-phase2-evidence.json"
	}
	s.records = append(s.records, cloneMavoPhase2Evidence(evidence))
	return s.path, nil
}

func (s *fakeMavoPhase2EvidenceSink) record(_ string, evidence mavoPhase2Evidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := "record:" + evidence.Phase
	s.ops = append(s.ops, name)
	if s.panicAt == name {
		panic("authorization=record-secret-123456789012345")
	}
	if s.failAt == name {
		return errors.New("password=record-secret-123456789012345")
	}
	s.records = append(s.records, cloneMavoPhase2Evidence(evidence))
	return nil
}

func (s *fakeMavoPhase2EvidenceSink) last() mavoPhase2Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return mavoPhase2Evidence{}
	}
	return cloneMavoPhase2Evidence(s.records[len(s.records)-1])
}

func cloneMavoPhase2Evidence(value mavoPhase2Evidence) mavoPhase2Evidence {
	data, _ := json.Marshal(value)
	var cloned mavoPhase2Evidence
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func syntheticMavoPhase2Runtime() *mavoPhase2Runtime {
	artifacts := make([]mavoPhase2Artifact, 0, len(mavoPhase2PinnedArtifacts))
	for index, spec := range mavoPhase2PinnedArtifacts {
		data := []byte{byte(index + 1)}
		artifacts = append(artifacts, mavoPhase2Artifact{Spec: spec, Data: data, Size: int64(len(data))})
	}
	return &mavoPhase2Runtime{
		version:         mavoPhase2RuntimeVersion,
		cacheVerifiedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC),
		artifacts:       artifacts,
	}
}

func newTestMavoPhase2Coordinator(target *fakeMavoPhase2Target, sink *fakeMavoPhase2EvidenceSink) *mavoPhase2Coordinator {
	return &mavoPhase2Coordinator{
		target: target, evidence: sink,
		now: func() time.Time { return time.Date(2026, 8, 15, 2, 3, 4, 5, time.UTC) },
	}
}

func runSyntheticMavoPhase2(target *fakeMavoPhase2Target, sink *fakeMavoPhase2EvidenceSink) mavoPhase2Result {
	return newTestMavoPhase2Coordinator(target, sink).probe(
		context.Background(), syntheticMavoPhase2Runtime(), mavoPhase2Confirmation, hermeticMavoPhase2Permit{},
	)
}

func TestMavoPhase2CheckIsReadOnly(t *testing.T) {
	target := newFakeMavoPhase2Target()
	coordinator := newTestMavoPhase2Coordinator(target, &fakeMavoPhase2EvidenceSink{})
	preflight, err := coordinator.check(context.Background(), syntheticMavoPhase2Runtime())
	if err != nil || !preflight.FactoryProfileExact {
		t.Fatalf("check failed: preflight=%+v err=%v", preflight, err)
	}
	if got := target.operations(); !slices.Equal(got, []string{"preflight"}) {
		t.Fatalf("check performed more than read-only preflight: %v", got)
	}
}

func TestMavoPhase2ApplyRequiresIndependentConfirmationAndNonProductionPermit(t *testing.T) {
	for name, confirmation := range map[string]string{
		"empty":       "",
		"phase one":   mavoPhase1Confirmation,
		"generic yes": "yes",
	} {
		t.Run(name, func(t *testing.T) {
			target := newFakeMavoPhase2Target()
			result := newTestMavoPhase2Coordinator(target, &fakeMavoPhase2EvidenceSink{}).probe(
				context.Background(), syntheticMavoPhase2Runtime(), confirmation, hermeticMavoPhase2Permit{},
			)
			if result.Err == nil || result.Phase != "locked" || len(target.operations()) != 0 {
				t.Fatalf("inexact confirmation crossed gate: result=%+v ops=%v", result, target.operations())
			}
		})
	}
	target := newFakeMavoPhase2Target()
	result := newTestMavoPhase2Coordinator(target, &fakeMavoPhase2EvidenceSink{}).probe(
		context.Background(), syntheticMavoPhase2Runtime(), mavoPhase2Confirmation, nil,
	)
	if result.Err == nil || result.Phase != "locked" || len(target.operations()) != 0 {
		t.Fatalf("nil production permit crossed gate: result=%+v ops=%v", result, target.operations())
	}
}

func TestMavoPhase2SuccessAlwaysCleansInReverseOrder(t *testing.T) {
	target := newFakeMavoPhase2Target()
	sink := &fakeMavoPhase2EvidenceSink{}
	result := runSyntheticMavoPhase2(target, sink)
	if result.Err != nil || result.Phase != "completed" || !result.MutationAttempted ||
		!result.TargetVerified || !result.AudioProbed || !result.CleanupVerified || result.RecoveryRequired {
		t.Fatalf("unexpected Phase-2 success result: %+v", result)
	}
	want := []string{
		"preflight", "activate", "inspect_target",
		"push:mavo-pcm-bridge.armv7", "verify_artifact:mavo-pcm-bridge.armv7",
		"push:qdc507_aprv3.ko", "verify_artifact:qdc507_aprv3.ko",
		"push:qdc507_voice.ko", "verify_artifact:qdc507_voice.ko",
		"load:qdc507_aprv3", "verify_module:qdc507_aprv3",
		"load:qdc507_voice", "verify_module:qdc507_voice",
		"probe_audio",
		"unload:qdc507_voice", "unload:qdc507_aprv3",
		"remove:qdc507_voice.ko", "remove:qdc507_aprv3.ko", "remove:mavo-pcm-bridge.armv7",
		"restore_factory", "verify_cleanup",
	}
	if got := target.operations(); !slices.Equal(got, want) {
		t.Fatalf("operation order:\n got %v\nwant %v", got, want)
	}
	last := sink.last()
	if last.Phase != "completed" || !last.EvidenceComplete || !last.CleanupVerified || last.RecoveryRequired ||
		last.AudioProbe == nil || last.Cleanup == nil || len(last.Artifacts) != 3 {
		t.Fatalf("final evidence is incomplete: %+v", last)
	}
}

func TestMavoPhase2LostMutationRepliesUseReadbackAndStillClean(t *testing.T) {
	for _, step := range []string{
		"activate", "push:mavo-pcm-bridge.armv7", "push:qdc507_aprv3.ko", "push:qdc507_voice.ko",
		"load:qdc507_aprv3", "load:qdc507_voice",
	} {
		t.Run(step, func(t *testing.T) {
			target := newFakeMavoPhase2Target()
			target.failAt[step] = errors.New("transport lost token=do-not-log-123456789012345")
			sink := &fakeMavoPhase2EvidenceSink{}
			result := runSyntheticMavoPhase2(target, sink)
			if result.Err != nil || result.Phase != "completed" || !result.CleanupVerified {
				t.Fatalf("readback did not recover ambiguous %s: %+v", step, result)
			}
			encoded, _ := json.Marshal(sink.last())
			if strings.Contains(string(encoded), "do-not-log") || strings.Contains(string(encoded), "123456789012345") {
				t.Fatalf("evidence leaked injected secret: %s", encoded)
			}
		})
	}
}

func TestMavoPhase2FailureInjectionStillPerformsCompleteCleanup(t *testing.T) {
	for _, step := range []string{
		"inspect_target",
		"verify_artifact:mavo-pcm-bridge.armv7",
		"verify_artifact:qdc507_aprv3.ko",
		"verify_artifact:qdc507_voice.ko",
		"verify_module:qdc507_aprv3",
		"verify_module:qdc507_voice",
		"probe_audio",
	} {
		t.Run(step, func(t *testing.T) {
			target := newFakeMavoPhase2Target()
			target.failAt[step] = errors.New("injected failure at " + step)
			result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
			if result.Err == nil || result.Phase != "failed_safe" || !result.MutationAttempted ||
				!result.CleanupVerified || result.RecoveryRequired {
				t.Fatalf("failure did not fail safe: %+v", result)
			}
			ops := target.operations()
			for _, required := range []string{
				"unload:qdc507_voice", "unload:qdc507_aprv3", "remove:qdc507_voice.ko",
				"remove:qdc507_aprv3.ko", "remove:mavo-pcm-bridge.armv7", "restore_factory", "verify_cleanup",
			} {
				if !slices.Contains(ops, required) {
					t.Fatalf("failure at %s skipped %s: %v", step, required, ops)
				}
			}
		})
	}
}

func TestMavoPhase2UncertainNotAttemptedMutationFailsAndCleans(t *testing.T) {
	target := newFakeMavoPhase2Target()
	target.attemptedFalseAt["push:qdc507_aprv3.ko"] = true
	result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
	if result.Err == nil || result.Phase != "failed_safe" || !result.CleanupVerified {
		t.Fatalf("not-attempted mutation was accepted: %+v", result)
	}
	if !slices.Contains(target.operations(), "verify_cleanup") {
		t.Fatalf("cleanup verification was skipped: %v", target.operations())
	}
}

func TestMavoPhase2PanicAndCanceledRequestUseIndependentCleanupContext(t *testing.T) {
	requestContext, cancel := context.WithCancel(context.Background())
	target := newFakeMavoPhase2Target()
	target.panicAt["probe_audio"] = true
	target.onStep = func(step string) {
		if step == "probe_audio" {
			cancel()
		}
	}
	result := newTestMavoPhase2Coordinator(target, &fakeMavoPhase2EvidenceSink{}).probe(
		requestContext, syntheticMavoPhase2Runtime(), mavoPhase2Confirmation, hermeticMavoPhase2Permit{},
	)
	if result.Err == nil || result.Phase != "failed_safe" || !result.CleanupVerified {
		t.Fatalf("panic/cancel did not fail safe: %+v", result)
	}
	if !target.cleanupSawLiveContext {
		t.Fatal("cleanup inherited the canceled request context")
	}
	if strings.Contains(result.Err.Error(), "phase2-panic") || strings.Contains(result.Err.Error(), "123456789012345") {
		t.Fatalf("panic detail was not redacted: %v", result.Err)
	}
}

func TestMavoPhase2CleanupWarningsAreResolvedOnlyByExactTerminalReadback(t *testing.T) {
	target := newFakeMavoPhase2Target()
	target.failAt["unload:qdc507_voice"] = errors.New("response lost")
	target.failAt["remove:qdc507_aprv3.ko"] = errors.New("response lost")
	target.failAt["restore_factory"] = errors.New("response lost")
	result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
	if result.Err != nil || result.Phase != "completed" || !result.CleanupVerified {
		t.Fatalf("exact final readback did not resolve cleanup replies: %+v", result)
	}

	target = newFakeMavoPhase2Target()
	target.failAt["unload:qdc507_voice"] = errors.New("response lost")
	target.cleanupValue.NetworkRecovered = false
	result = runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
	if result.Err == nil || result.Phase != "recovery_required" || result.CleanupVerified || !result.RecoveryRequired {
		t.Fatalf("incomplete terminal readback was not recovery-required: %+v", result)
	}
}

func TestMavoPhase2CleanupContinuesAfterCleanupPanics(t *testing.T) {
	target := newFakeMavoPhase2Target()
	target.panicAt["unload:qdc507_voice"] = true
	target.panicAt["remove:qdc507_aprv3.ko"] = true
	result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
	if result.Err != nil || !result.CleanupVerified {
		t.Fatalf("terminal readback did not resolve cleanup panics: %+v", result)
	}
	for _, required := range []string{"unload:qdc507_aprv3", "remove:mavo-pcm-bridge.armv7", "restore_factory", "verify_cleanup"} {
		if !slices.Contains(target.operations(), required) {
			t.Fatalf("cleanup panic skipped %s: %v", required, target.operations())
		}
	}
}

func TestMavoPhase2FinalCleanupVerificationFailureIsRecoveryRequired(t *testing.T) {
	target := newFakeMavoPhase2Target()
	target.failAt["verify_cleanup"] = errors.New("cannot read final state")
	result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{})
	if result.Err == nil || result.Phase != "recovery_required" || !result.RecoveryRequired || result.CleanupVerified {
		t.Fatalf("final verification failure did not require recovery: %+v", result)
	}
}

func TestMavoPhase2EvidenceFailureAfterMutationStillCleans(t *testing.T) {
	target := newFakeMavoPhase2Target()
	sink := &fakeMavoPhase2EvidenceSink{failAt: "record:target_verified"}
	result := runSyntheticMavoPhase2(target, sink)
	if result.Err == nil || result.Phase != "failed_safe" || !result.CleanupVerified {
		t.Fatalf("evidence failure did not trigger safe cleanup: %+v", result)
	}
	if !slices.Contains(target.operations(), "verify_cleanup") {
		t.Fatalf("evidence failure skipped cleanup: %v", target.operations())
	}
	if strings.Contains(result.Err.Error(), "record-secret") || strings.Contains(result.Err.Error(), "123456789012345") {
		t.Fatalf("evidence error leaked secret: %v", result.Err)
	}
}

func TestMavoPhase2EvidenceArmPanicStopsBeforeMutation(t *testing.T) {
	target := newFakeMavoPhase2Target()
	result := runSyntheticMavoPhase2(target, &fakeMavoPhase2EvidenceSink{panicAt: "arm"})
	if result.Err == nil || result.Phase != "failed_before_write" || result.MutationAttempted || result.CleanupVerified {
		t.Fatalf("evidence arm panic crossed mutation boundary: %+v", result)
	}
	if got := target.operations(); !slices.Equal(got, []string{"preflight"}) {
		t.Fatalf("evidence arm panic performed target operations: %v", got)
	}
	if strings.Contains(result.Err.Error(), "arm-123456789012345") {
		t.Fatalf("evidence arm panic leaked detail: %v", result.Err)
	}
}

func TestMavoPhase2CacheVerifierReadsOnlyExactPolicy(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"helper.bin": []byte("synthetic helper\n"),
		"apr.ko":     []byte("synthetic apr\n"),
		"voice.ko":   []byte("synthetic voice\n"),
	}
	specs := make([]mavoPhase2ArtifactSpec, 0, len(contents))
	for _, name := range []string{"helper.bin", "apr.ko", "voice.ko"} {
		data := contents[name]
		digest := sha256.Sum256(data)
		module := ""
		mode := uint32(0o755)
		if strings.HasSuffix(name, ".ko") {
			module = strings.TrimSuffix(name, ".ko")
			mode = 0o644
		}
		specs = append(specs, mavoPhase2ArtifactSpec{
			Name: name, SHA256: hex.EncodeToString(digest[:]), RemoteMode: mode, ModuleName: module,
		})
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated, malformed manifest must not be consumed by the three-file
	// loader. It is deliberately outside the policy.
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte("not-json\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadMavoPhase2RuntimeWithSpecs(directory, specs)
	if err != nil || len(loaded.artifacts) != 3 {
		t.Fatalf("valid synthetic cache rejected: runtime=%+v err=%v", loaded, err)
	}

	if err := os.WriteFile(filepath.Join(directory, "voice.ko"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMavoPhase2RuntimeWithSpecs(directory, specs); err == nil {
		t.Fatal("tampered cached runtime was accepted")
	}

	if err := os.Remove(filepath.Join(directory, "voice.ko")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "apr.ko"), filepath.Join(directory, "voice.ko")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMavoPhase2RuntimeWithSpecs(directory, specs); err == nil {
		t.Fatal("symlinked cached runtime was accepted")
	}
}

func TestMavoPhase2PinnedArtifactPolicyMatchesFrozenHashes(t *testing.T) {
	want := []mavoPhase2ArtifactSpec{
		{Name: "mavo-pcm-bridge.armv7", SHA256: "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc", RemoteMode: 0o755},
		{Name: "qdc507_aprv3.ko", SHA256: "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a", RemoteMode: 0o644, ModuleName: "qdc507_aprv3"},
		{Name: "qdc507_voice.ko", SHA256: "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c", RemoteMode: 0o644, ModuleName: "qdc507_voice"},
	}
	if !slices.Equal(mavoPhase2PinnedArtifacts, want) {
		t.Fatalf("pinned three-file policy changed: %+v", mavoPhase2PinnedArtifacts)
	}
}

func TestMavoPhase2FileEvidenceSinkIsPrivateAtomicAndPathBound(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "evidence")
	sink, err := newMavoPhase2FileEvidenceSink(directory)
	if err != nil {
		t.Fatal(err)
	}
	evidence := newMavoPhase2Evidence(
		strings.Repeat("a", 32), time.Date(2026, 8, 15, 2, 3, 4, 0, time.UTC),
		syntheticMavoPhase2Runtime(), newFakeMavoPhase2Target().preflightValue,
	)
	path, err := sink.arm(evidence)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence file mode=%v err=%v", info.Mode().Perm(), err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("evidence directory mode=%v err=%v", directoryInfo.Mode().Perm(), err)
	}
	evidence.Phase = "completed"
	evidence.EvidenceComplete = true
	if err := sink.record(path, evidence); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"phase": "completed"`) {
		t.Fatalf("evidence update missing: %v %s", err, data)
	}
	if err := sink.record(filepath.Join(directory, "other.json"), evidence); err == nil {
		t.Fatal("evidence sink accepted a changed path")
	}
	evidence.RunID = "../escape"
	if _, err := sink.arm(evidence); err == nil {
		t.Fatal("evidence sink accepted a traversal run ID")
	}
}

func TestMavoPhase2ErrorsAreRedacted(t *testing.T) {
	raw := errors.New("token=abc123 authorization:BearerXYZ email operator@example.com imei 123456789012345 /Users/operator/private\n" + strings.Repeat("a", 64))
	got := mavoPhase2SafeError(raw)
	for _, forbidden := range []string{"abc123", "BearerXYZ", "operator@example.com", "123456789012345", "/Users/operator", strings.Repeat("a", 64), "\n"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("safe error contains %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "[redacted]") || len(got) > 512 {
		t.Fatalf("safe error was not bounded/redacted: %q", got)
	}
}

func TestMavoPhase2AudioProbeRejectsCallSMSOrStartedRoute(t *testing.T) {
	valid := newFakeMavoPhase2Target().probeValue
	for name, mutate := range map[string]func(*mavoPhase2AudioProbe){
		"helper running": func(value *mavoPhase2AudioProbe) { value.HelperRunning = true },
		"route enabled":  func(value *mavoPhase2AudioProbe) { value.AudioRouteEnabled = true },
		"call issued":    func(value *mavoPhase2AudioProbe) { value.CallCommandIssued = true },
		"sms issued":     func(value *mavoPhase2AudioProbe) { value.SMSMutationIssued = true },
		"wrong location": func(value *mavoPhase2AudioProbe) { value.USBLocationMatched = false },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.DevicePaths = slices.Clone(valid.DevicePaths)
			mutate(&candidate)
			if err := validateMavoPhase2AudioProbe(candidate); err == nil {
				t.Fatal("unsafe audio probe was accepted")
			}
		})
	}
}

func TestMavoPhase2DirectMediaIntegrationBindingIsExact(t *testing.T) {
	valid := mavoPhase2MediaBinding{
		RuntimeLeaseID: strings.Repeat("a", 32), DeviceIdentityHash: strings.Repeat("b", 64),
		USBLocationID: 1, USBLifecycle: 2, CallTicketHash: strings.Repeat("c", 64), CallGeneration: 3,
	}
	if err := validateMavoPhase2MediaBinding(valid); err != nil {
		t.Fatalf("valid exact media binding rejected: %v", err)
	}
	invalid := valid
	invalid.CallGeneration = 0
	if err := validateMavoPhase2MediaBinding(invalid); err == nil {
		t.Fatal("generation-free media binding accepted")
	}
	invalid = valid
	invalid.USBLifecycle = 0
	if err := validateMavoPhase2MediaBinding(invalid); err == nil {
		t.Fatal("USB-lifecycle-free media binding accepted")
	}
}

func TestMavoPhase2HasNoProductionPermitImplementationOrWiring(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	directory := filepath.Dir(current)
	entries, err := filepath.Glob(filepath.Join(directory, "mavo_phase2*.go"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("find Phase-2 source: %v", err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"AT+", "ATD", "ATA", "ATH", "/api/module/setup", "startMavoPhase2API"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains forbidden live operation or API marker %q", filepath.Base(path), forbidden)
			}
		}
		if strings.Contains(text, "func (mavoPhase2MutationPermit") ||
			strings.Contains(text, "func (*mavoPhase2MutationPermit") ||
			strings.Contains(text, "mavoPhase2MutationPermitMarker() {}") {
			t.Fatalf("%s implements a production mutation permit", filepath.Base(path))
		}
	}
	allGo, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range allGo {
		if strings.HasSuffix(path, "_test.go") || strings.Contains(filepath.Base(path), "mavo_phase2") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "mavoPhase2Coordinator") || strings.Contains(string(data), "mavo_temporary_runtime_probe") {
			t.Fatalf("Phase-2 was wired by existing production file %s", filepath.Base(path))
		}
	}
}
