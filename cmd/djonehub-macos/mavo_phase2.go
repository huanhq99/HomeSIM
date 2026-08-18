package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Phase 2 is deliberately not wired to main, an HTTP handler, or a live ADB
// adapter. It is a fault-testable orchestration and integration contract for a
// separately authorized experiment. In particular, this file contains no AT,
// ADB shell, module-loader, dial, answer, hangup, or SMS command.
const (
	mavoPhase2ExperimentName = "mavo_temporary_runtime_probe"
	mavoPhase2Confirmation   = "一次 MaVo Phase-2 临时运行时探测，结束后卸载清理并恢复，不拨号不发短信"
	mavoPhase2EvidenceV1     = "mavo-phase2-evidence-v1"
	mavoPhase2RuntimeVersion = "qdc507-3.18.44-voice-20260712.5"
	mavoPhase2KernelRelease  = "3.18.44"
	mavoPhase2Architecture   = "armv7l"
	mavoPhase2CardName       = "mdm9607-tomtom-i2s-snd-card"
	mavoPhase2RemoteRoot     = "/run/maccellular-call"
	mavoPhase2MaxArtifact    = 16 << 20
	mavoPhase2CleanupTimeout = 45 * time.Second
)

type mavoPhase2ArtifactSpec struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	RemoteMode uint32 `json:"remote_mode"`
	ModuleName string `json:"module_name,omitempty"`
}

// mavoPhase2PinnedArtifacts is the complete device-side input set. Phase 2
// intentionally does not consume a manifest, report, license file, archive,
// network download, or repository-local fallback.
var mavoPhase2PinnedArtifacts = []mavoPhase2ArtifactSpec{
	{
		Name:       "mavo-pcm-bridge.armv7",
		SHA256:     "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc",
		RemoteMode: 0o755,
	},
	{
		Name:       "qdc507_aprv3.ko",
		SHA256:     "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a",
		RemoteMode: 0o644,
		ModuleName: "qdc507_aprv3",
	},
	{
		Name:       "qdc507_voice.ko",
		SHA256:     "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c",
		RemoteMode: 0o644,
		ModuleName: "qdc507_voice",
	},
}

var mavoPhase2RequiredAudioDevices = []string{
	"/dev/snd/controlC0",
	"/dev/snd/pcmC0D4p",
	"/dev/snd/pcmC0D4c",
	"/dev/snd/pcmC0D5p",
	"/dev/snd/pcmC0D6c",
}

type mavoPhase2Artifact struct {
	Spec mavoPhase2ArtifactSpec
	Data []byte
	Size int64
}

type mavoPhase2Runtime struct {
	version         string
	cacheVerifiedAt time.Time
	artifacts       []mavoPhase2Artifact
}

func (*mavoPhase2Runtime) String() string   { return "mavoPhase2Runtime{redacted}" }
func (*mavoPhase2Runtime) GoString() string { return "mavoPhase2Runtime{redacted}" }

func defaultMavoPhase2CacheDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve local Phase-2 cache directory")
	}
	return filepath.Join(home, "Library", "Application Support", "MacCellular Runtime", "ModuleVoice"), nil
}

func loadMavoPhase2Runtime(directory string) (*mavoPhase2Runtime, error) {
	return loadMavoPhase2RuntimeWithSpecs(directory, mavoPhase2PinnedArtifacts)
}

// loadMavoPhase2RuntimeWithSpecs exists so cache validation can be tested with
// synthetic bytes. Production callers use loadMavoPhase2Runtime, whose policy
// is the fixed three-entry list above.
func loadMavoPhase2RuntimeWithSpecs(directory string, specs []mavoPhase2ArtifactSpec) (*mavoPhase2Runtime, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "" || directory == "." || !filepath.IsAbs(directory) {
		return nil, errors.New("Phase-2 cache must be an absolute local directory")
	}
	if err := validateMavoPhase2ArtifactSpecs(specs); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, mavoPhase2SanitizedError("inspect Phase-2 cache", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("Phase-2 cache must be a private real directory")
	}

	artifacts := make([]mavoPhase2Artifact, 0, len(specs))
	for _, spec := range specs {
		path := filepath.Join(directory, spec.Name)
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return nil, mavoPhase2SanitizedError("inspect pinned Phase-2 artifact "+spec.Name, err)
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 || fileInfo.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("pinned Phase-2 artifact %s must be a private regular file", spec.Name)
		}
		if fileInfo.Size() <= 0 || fileInfo.Size() > mavoPhase2MaxArtifact {
			return nil, fmt.Errorf("pinned Phase-2 artifact %s has an invalid size", spec.Name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, mavoPhase2SanitizedError("read pinned Phase-2 artifact "+spec.Name, err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != spec.SHA256 {
			return nil, fmt.Errorf("pinned Phase-2 artifact %s failed SHA-256 verification", spec.Name)
		}
		artifacts = append(artifacts, mavoPhase2Artifact{
			Spec: spec,
			Data: slices.Clone(data),
			Size: int64(len(data)),
		})
	}
	return &mavoPhase2Runtime{
		version:         mavoPhase2RuntimeVersion,
		cacheVerifiedAt: time.Now().UTC(),
		artifacts:       artifacts,
	}, nil
}

func validateMavoPhase2ArtifactSpecs(specs []mavoPhase2ArtifactSpec) error {
	if len(specs) == 0 || len(specs) > 8 {
		return errors.New("Phase-2 artifact policy has an invalid size")
	}
	namePattern := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	modulePattern := regexp.MustCompile(`^[A-Za-z0-9_]*$`)
	hashPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if !namePattern.MatchString(spec.Name) || !hashPattern.MatchString(spec.SHA256) ||
			!modulePattern.MatchString(spec.ModuleName) || (spec.RemoteMode != 0o644 && spec.RemoteMode != 0o755) || seen[spec.Name] {
			return errors.New("Phase-2 artifact policy is not exact")
		}
		seen[spec.Name] = true
	}
	return nil
}

func validatePinnedMavoPhase2Runtime(runtime *mavoPhase2Runtime) error {
	if runtime == nil || runtime.version != mavoPhase2RuntimeVersion || runtime.cacheVerifiedAt.IsZero() ||
		len(runtime.artifacts) != len(mavoPhase2PinnedArtifacts) {
		return errors.New("Phase-2 runtime was not produced by the pinned cache verifier")
	}
	for index, expected := range mavoPhase2PinnedArtifacts {
		artifact := runtime.artifacts[index]
		if artifact.Spec != expected || artifact.Size <= 0 || len(artifact.Data) != int(artifact.Size) {
			return errors.New("Phase-2 runtime artifact order or metadata changed")
		}
	}
	return nil
}

type mavoPhase2Preflight struct {
	DeviceIdentitySHA256 string `json:"device_identity_sha256"`
	USBLocationID        uint32 `json:"usb_location_id"`
	USBLifecycle         uint64 `json:"usb_lifecycle"`
	Firmware             string `json:"firmware"`
	FactoryProfileExact  bool   `json:"factory_profile_exact"`
	NetworkReady         bool   `json:"network_ready"`
	VoiceIdle            bool   `json:"voice_idle"`
	SMSMutationIdle      bool   `json:"sms_mutation_idle"`
	KnownModulesAbsent   bool   `json:"known_modules_absent"`
	RemoteRuntimeAbsent  bool   `json:"remote_runtime_absent"`
	AudioRouteDisabled   bool   `json:"audio_route_disabled"`
	ExclusiveLeaseReady  bool   `json:"exclusive_lease_ready"`
}

type mavoPhase2TargetInspection struct {
	DeviceIdentitySHA256 string `json:"device_identity_sha256"`
	USBLocationID        uint32 `json:"usb_location_id"`
	USBLifecycle         uint64 `json:"usb_lifecycle"`
	TargetProfileExact   bool   `json:"target_profile_exact"`
	ADBDescriptorValid   bool   `json:"adb_descriptor_valid"`
	UACDescriptorValid   bool   `json:"uac_descriptor_valid"`
	ADBRoot              bool   `json:"adb_root"`
	KernelRelease        string `json:"kernel_release"`
	Architecture         string `json:"architecture"`
	TTYGS0Present        bool   `json:"ttygs0_present"`
	VocServerPresent     bool   `json:"voc_server_present"`
	AudioLibraryPresent  bool   `json:"audio_library_present"`
	ARMELLoaderPresent   bool   `json:"armel_loader_present"`
	KnownModulesAbsent   bool   `json:"known_modules_absent"`
}

type mavoPhase2AudioProbe struct {
	CardName           string   `json:"card_name"`
	DevicePaths        []string `json:"device_paths"`
	APRModuleLoaded    bool     `json:"apr_module_loaded"`
	VoiceModuleLoaded  bool     `json:"voice_module_loaded"`
	UACDescriptorValid bool     `json:"uac_descriptor_valid"`
	USBLocationMatched bool     `json:"usb_location_matched"`
	HelperRunning      bool     `json:"helper_running"`
	AudioRouteEnabled  bool     `json:"audio_route_enabled"`
	CallCommandIssued  bool     `json:"call_command_issued"`
	SMSMutationIssued  bool     `json:"sms_mutation_issued"`
}

type mavoPhase2CleanupEvidence struct {
	DeviceIdentityMatched bool     `json:"device_identity_matched"`
	FactoryProfileExact   bool     `json:"factory_profile_exact"`
	NetworkRecovered      bool     `json:"network_recovered"`
	ModulesAbsent         []string `json:"modules_absent"`
	ArtifactsAbsent       []string `json:"artifacts_absent"`
	RemoteRuntimeAbsent   bool     `json:"remote_runtime_absent"`
	HelperAbsent          bool     `json:"helper_absent"`
	AudioRouteDisabled    bool     `json:"audio_route_disabled"`
}

type mavoPhase2EvidenceEvent struct {
	At      string `json:"at"`
	Phase   string `json:"phase"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

type mavoPhase2Evidence struct {
	Schema            string                       `json:"schema"`
	RunID             string                       `json:"run_id"`
	Experiment        string                       `json:"experiment"`
	UpdatedAt         string                       `json:"updated_at"`
	Phase             string                       `json:"phase"`
	RuntimeVersion    string                       `json:"runtime_version"`
	Artifacts         []mavoPhase2ArtifactEvidence `json:"artifacts"`
	Preflight         mavoPhase2Preflight          `json:"preflight"`
	TargetInspection  *mavoPhase2TargetInspection  `json:"target_inspection,omitempty"`
	AudioProbe        *mavoPhase2AudioProbe        `json:"audio_probe,omitempty"`
	Cleanup           *mavoPhase2CleanupEvidence   `json:"cleanup,omitempty"`
	MutationAttempted bool                         `json:"mutation_attempted"`
	CleanupVerified   bool                         `json:"cleanup_verified"`
	RecoveryRequired  bool                         `json:"recovery_required"`
	EvidenceComplete  bool                         `json:"evidence_complete"`
	Events            []mavoPhase2EvidenceEvent    `json:"events"`
}

type mavoPhase2ArtifactEvidence struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	RemoteMode uint32 `json:"remote_mode"`
	ModuleName string `json:"module_name,omitempty"`
}

type mavoPhase2Result struct {
	Phase             string
	EvidencePath      string
	MutationAttempted bool
	TargetVerified    bool
	AudioProbed       bool
	CleanupVerified   bool
	RecoveryRequired  bool
	Err               error
}

// mavoPhase2ReadOnlyInspector is the only interface needed by default check.
// Its implementation must not alter local services, the modem, or the SIM.
type mavoPhase2ReadOnlyInspector interface {
	preflight(context.Context) (mavoPhase2Preflight, error)
}

// mavoPhase2Target is the future live-adapter boundary. The current tree has
// no implementation. Every mutator must report attempted=true before bytes can
// leave the host, so a lost reply still triggers full cleanup.
type mavoPhase2Target interface {
	mavoPhase2ReadOnlyInspector
	activateTemporaryProfile(context.Context, mavoPhase2Preflight) (attempted bool, err error)
	inspectTemporaryTarget(context.Context, mavoPhase2Preflight) (mavoPhase2TargetInspection, error)
	pushArtifact(context.Context, mavoPhase2Artifact) (attempted bool, err error)
	artifactMatches(context.Context, mavoPhase2ArtifactSpec) error
	loadModule(context.Context, mavoPhase2ArtifactSpec) (attempted bool, err error)
	moduleLoaded(context.Context, string) (bool, error)
	probeAudio(context.Context, mavoPhase2TargetInspection) (mavoPhase2AudioProbe, error)
	unloadModule(context.Context, string) error
	removeArtifact(context.Context, string) error
	restoreFactoryProfile(context.Context, mavoPhase2Preflight) error
	verifyCleanup(context.Context, mavoPhase2Preflight, []mavoPhase2ArtifactSpec) (mavoPhase2CleanupEvidence, error)
}

type mavoPhase2EvidenceSink interface {
	arm(mavoPhase2Evidence) (string, error)
	record(string, mavoPhase2Evidence) error
}

// There is intentionally no non-test implementation of this interface. A
// future live integration therefore requires a visible source change in
// addition to the exact human confirmation. Until then, production mutation
// is not callable even if someone constructs a target adapter.
type mavoPhase2MutationPermit interface {
	mavoPhase2MutationPermitMarker()
}

type mavoPhase2Coordinator struct {
	target   mavoPhase2Target
	evidence mavoPhase2EvidenceSink
	now      func() time.Time
}

func (c *mavoPhase2Coordinator) check(ctx context.Context, runtime *mavoPhase2Runtime) (mavoPhase2Preflight, error) {
	if c == nil || c.target == nil {
		return mavoPhase2Preflight{}, errors.New("Phase-2 read-only inspector is unavailable")
	}
	if err := validatePinnedMavoPhase2Runtime(runtime); err != nil {
		return mavoPhase2Preflight{}, err
	}
	preflight, err := c.target.preflight(ctx)
	if err != nil {
		return mavoPhase2Preflight{}, mavoPhase2SanitizedError("Phase-2 read-only preflight failed", err)
	}
	if err := validateMavoPhase2Preflight(preflight); err != nil {
		return mavoPhase2Preflight{}, err
	}
	return preflight, nil
}

func (c *mavoPhase2Coordinator) probe(
	ctx context.Context,
	runtime *mavoPhase2Runtime,
	confirmation string,
	permit mavoPhase2MutationPermit,
) (result mavoPhase2Result) {
	result.Phase = "locked"
	defer func() {
		if recovered := recover(); recovered != nil {
			if result.Phase == "locked" {
				result.Phase = "failed_before_write"
			}
			result.Err = mavoPhase2JoinSafe(
				result.Err,
				mavoPhase2SanitizedError("Phase-2 coordinator panic", fmt.Errorf("%v", recovered)),
			)
		}
	}()
	if confirmation != mavoPhase2Confirmation {
		result.Err = errors.New("Phase-2 apply requires its exact independent confirmation phrase")
		return result
	}
	if permit == nil {
		result.Err = errors.New("Phase-2 production mutation is not authorized or wired")
		return result
	}
	if c == nil || c.target == nil || c.evidence == nil {
		result.Err = errors.New("Phase-2 coordinator is incomplete")
		return result
	}
	preflight, err := c.check(ctx, runtime)
	if err != nil {
		result.Phase = "failed_before_write"
		result.Err = err
		return result
	}

	now := c.now
	if now == nil {
		now = time.Now
	}
	runID, err := newMavoPhase2RunID()
	if err != nil {
		result.Phase = "failed_before_write"
		result.Err = err
		return result
	}
	evidence := newMavoPhase2Evidence(runID, now(), runtime, preflight)
	evidencePath, err := c.evidence.arm(evidence)
	if err != nil {
		result.Phase = "failed_before_write"
		result.Err = mavoPhase2SanitizedError("arm durable Phase-2 evidence", err)
		return result
	}
	result.EvidencePath = evidencePath

	mark := func(phase, outcome string, detail error) error {
		evidence.Phase = phase
		evidence.UpdatedAt = now().UTC().Format(time.RFC3339Nano)
		evidence.Events = append(evidence.Events, mavoPhase2EvidenceEvent{
			At: evidence.UpdatedAt, Phase: phase, Outcome: outcome, Detail: mavoPhase2SafeError(detail),
		})
		if err := c.evidence.record(evidencePath, evidence); err != nil {
			return mavoPhase2SanitizedError("record durable Phase-2 evidence", err)
		}
		return nil
	}

	var operationErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				operationErr = mavoPhase2SanitizedError("Phase-2 orchestration panic", fmt.Errorf("%v", recovered))
			}
		}()
		operationErr = c.runProbeOperations(ctx, runtime, preflight, &result, &evidence, mark)
	}()
	if err := mark("cleaning", "started", nil); err != nil && operationErr == nil {
		operationErr = err
	}

	cleanup, cleanupErr, cleanupWarnings := c.cleanup(preflight, runtime.artifacts)
	if cleanup != nil {
		evidence.Cleanup = cleanup
	}
	if cleanupErr == nil && cleanup != nil {
		result.CleanupVerified = true
		evidence.CleanupVerified = true
	}
	if cleanupWarnings != nil {
		evidence.Events = append(evidence.Events, mavoPhase2EvidenceEvent{
			At: now().UTC().Format(time.RFC3339Nano), Phase: "cleanup_verified",
			Outcome: "readback_recovered", Detail: mavoPhase2SafeError(cleanupWarnings),
		})
	}

	switch {
	case cleanupErr != nil:
		result.Phase = "recovery_required"
		result.RecoveryRequired = true
		evidence.Phase = result.Phase
		evidence.RecoveryRequired = true
		result.Err = mavoPhase2JoinSafe(operationErr, cleanupErr)
	case operationErr != nil:
		result.Phase = "failed_safe"
		evidence.Phase = result.Phase
		result.Err = operationErr
	default:
		result.Phase = "completed"
		evidence.Phase = result.Phase
	}
	evidence.MutationAttempted = result.MutationAttempted
	evidence.EvidenceComplete = true
	evidence.UpdatedAt = now().UTC().Format(time.RFC3339Nano)
	evidence.Events = append(evidence.Events, mavoPhase2EvidenceEvent{
		At: evidence.UpdatedAt, Phase: result.Phase, Outcome: map[bool]string{true: "verified", false: "failed"}[result.Err == nil],
		Detail: mavoPhase2SafeError(result.Err),
	})
	if err := c.evidence.record(evidencePath, evidence); err != nil {
		evidence.EvidenceComplete = false
		result.Err = mavoPhase2JoinSafe(result.Err, mavoPhase2SanitizedError("write final Phase-2 evidence", err))
	}
	return result
}

func (c *mavoPhase2Coordinator) runProbeOperations(
	ctx context.Context,
	runtime *mavoPhase2Runtime,
	preflight mavoPhase2Preflight,
	result *mavoPhase2Result,
	evidence *mavoPhase2Evidence,
	mark func(string, string, error) error,
) error {
	// Set conservatively before the first mutator is entered. If an adapter
	// panics after emitting bytes but before returning, cleanup still runs.
	result.MutationAttempted = true
	evidence.MutationAttempted = true
	attempted, activateErr := c.target.activateTemporaryProfile(ctx, preflight)
	if !attempted {
		if activateErr == nil {
			activateErr = errors.New("temporary profile activation was not attempted")
		}
		return mavoPhase2SanitizedError("activate temporary ADB+UAC profile", activateErr)
	}
	inspection, inspectErr := c.target.inspectTemporaryTarget(ctx, preflight)
	if inspectErr != nil {
		return mavoPhase2JoinSafe(
			mavoPhase2SanitizedError("activate temporary ADB+UAC profile", activateErr),
			mavoPhase2SanitizedError("verify temporary ADB+UAC profile", inspectErr),
		)
	}
	if err := validateMavoPhase2TargetInspection(preflight, inspection); err != nil {
		return err
	}
	result.TargetVerified = true
	evidence.TargetInspection = &inspection
	if err := mark("target_verified", recoveredOutcome(activateErr), activateErr); err != nil {
		return err
	}

	for _, artifact := range runtime.artifacts {
		attempted, pushErr := c.target.pushArtifact(ctx, artifact)
		if !attempted {
			if pushErr == nil {
				pushErr = errors.New("artifact transfer was not attempted")
			}
			return mavoPhase2SanitizedError("transfer pinned artifact "+artifact.Spec.Name, pushErr)
		}
		verifyErr := c.target.artifactMatches(ctx, artifact.Spec)
		if verifyErr != nil {
			return mavoPhase2JoinSafe(
				mavoPhase2SanitizedError("transfer pinned artifact "+artifact.Spec.Name, pushErr),
				mavoPhase2SanitizedError("verify transferred artifact "+artifact.Spec.Name, verifyErr),
			)
		}
		if err := mark("artifact_verified", recoveredOutcome(pushErr), pushErr); err != nil {
			return err
		}
	}

	for _, artifact := range runtime.artifacts {
		if artifact.Spec.ModuleName == "" {
			continue
		}
		attempted, loadErr := c.target.loadModule(ctx, artifact.Spec)
		if !attempted {
			if loadErr == nil {
				loadErr = errors.New("module load was not attempted")
			}
			return mavoPhase2SanitizedError("load pinned module "+artifact.Spec.ModuleName, loadErr)
		}
		loaded, inspectErr := c.target.moduleLoaded(ctx, artifact.Spec.ModuleName)
		if inspectErr != nil || !loaded {
			if inspectErr == nil {
				inspectErr = errors.New("module readback reported absent")
			}
			return mavoPhase2JoinSafe(
				mavoPhase2SanitizedError("load pinned module "+artifact.Spec.ModuleName, loadErr),
				mavoPhase2SanitizedError("verify pinned module "+artifact.Spec.ModuleName, inspectErr),
			)
		}
		if err := mark("module_verified", recoveredOutcome(loadErr), loadErr); err != nil {
			return err
		}
	}

	probe, err := c.target.probeAudio(ctx, inspection)
	if err != nil {
		return mavoPhase2SanitizedError("read-only prepared-audio probe", err)
	}
	if err := validateMavoPhase2AudioProbe(probe); err != nil {
		return err
	}
	result.AudioProbed = true
	evidence.AudioProbe = &probe
	return mark("audio_probe_verified", "verified", nil)
}

func (c *mavoPhase2Coordinator) cleanup(
	preflight mavoPhase2Preflight,
	artifacts []mavoPhase2Artifact,
) (*mavoPhase2CleanupEvidence, error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mavoPhase2CleanupTimeout)
	defer cancel()
	var warnings []error
	moduleSpecs := make([]mavoPhase2ArtifactSpec, 0, 2)
	for _, artifact := range artifacts {
		if artifact.Spec.ModuleName != "" {
			moduleSpecs = append(moduleSpecs, artifact.Spec)
		}
	}
	for index := len(moduleSpecs) - 1; index >= 0; index-- {
		name := moduleSpecs[index].ModuleName
		if err := mavoPhase2SafeCleanupCall("unload module "+name, func() error {
			return c.target.unloadModule(ctx, name)
		}); err != nil {
			warnings = append(warnings, err)
		}
	}
	for index := len(artifacts) - 1; index >= 0; index-- {
		name := artifacts[index].Spec.Name
		if err := mavoPhase2SafeCleanupCall("remove artifact "+name, func() error {
			return c.target.removeArtifact(ctx, name)
		}); err != nil {
			warnings = append(warnings, err)
		}
	}
	if err := mavoPhase2SafeCleanupCall("restore factory profile", func() error {
		return c.target.restoreFactoryProfile(ctx, preflight)
	}); err != nil {
		warnings = append(warnings, err)
	}

	specs := make([]mavoPhase2ArtifactSpec, 0, len(artifacts))
	for _, artifact := range artifacts {
		specs = append(specs, artifact.Spec)
	}
	var cleanup mavoPhase2CleanupEvidence
	verifyErr := mavoPhase2SafeCleanupCall("verify final cleanup", func() error {
		var err error
		cleanup, err = c.target.verifyCleanup(ctx, preflight, specs)
		return err
	})
	if verifyErr == nil {
		verifyErr = validateMavoPhase2Cleanup(cleanup, specs)
	}
	if verifyErr != nil {
		return &cleanup, mavoPhase2JoinSafe(errors.Join(warnings...), verifyErr), errors.Join(warnings...)
	}
	// Individual command failures are warnings when the independent terminal
	// readback proves every exact resource absent and the factory state restored.
	return &cleanup, nil, errors.Join(warnings...)
}

func mavoPhase2SafeCleanupCall(label string, call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = mavoPhase2SanitizedError(label+" panic", fmt.Errorf("%v", recovered))
		}
	}()
	if call == nil {
		return errors.New(label + ": cleanup operation is unavailable")
	}
	if err := call(); err != nil {
		return mavoPhase2SanitizedError(label, err)
	}
	return nil
}

func recoveredOutcome(err error) string {
	if err != nil {
		return "readback_recovered"
	}
	return "verified"
}

func validateMavoPhase2Preflight(value mavoPhase2Preflight) error {
	if !validMavoPhase2Digest(value.DeviceIdentitySHA256) || value.USBLocationID == 0 || value.USBLifecycle == 0 ||
		value.Firmware != mavoPhase1Firmware || !value.FactoryProfileExact || !value.NetworkReady || !value.VoiceIdle ||
		!value.SMSMutationIdle || !value.KnownModulesAbsent || !value.RemoteRuntimeAbsent ||
		!value.AudioRouteDisabled || !value.ExclusiveLeaseReady {
		return errors.New("Phase-2 factory preflight did not satisfy every fixed gate")
	}
	return nil
}

func validateMavoPhase2TargetInspection(original mavoPhase2Preflight, value mavoPhase2TargetInspection) error {
	if value.DeviceIdentitySHA256 != original.DeviceIdentitySHA256 || value.USBLocationID != original.USBLocationID ||
		value.USBLifecycle <= original.USBLifecycle || !value.TargetProfileExact || !value.ADBDescriptorValid ||
		!value.UACDescriptorValid || !value.ADBRoot || value.KernelRelease != mavoPhase2KernelRelease ||
		value.Architecture != mavoPhase2Architecture || !value.TTYGS0Present || !value.VocServerPresent ||
		!value.AudioLibraryPresent || !value.ARMELLoaderPresent || !value.KnownModulesAbsent {
		return errors.New("Phase-2 temporary target did not satisfy every fixed gate")
	}
	return nil
}

func validateMavoPhase2AudioProbe(value mavoPhase2AudioProbe) error {
	if value.CardName != mavoPhase2CardName || !sameStringSet(value.DevicePaths, mavoPhase2RequiredAudioDevices) ||
		!value.APRModuleLoaded || !value.VoiceModuleLoaded || !value.UACDescriptorValid ||
		!value.USBLocationMatched || value.HelperRunning || value.AudioRouteEnabled ||
		value.CallCommandIssued || value.SMSMutationIssued {
		return errors.New("Phase-2 audio probe did not prove the inert prepared-runtime state")
	}
	return nil
}

func validateMavoPhase2Cleanup(value mavoPhase2CleanupEvidence, specs []mavoPhase2ArtifactSpec) error {
	modules := make([]string, 0, 2)
	artifacts := make([]string, 0, len(specs))
	for _, spec := range specs {
		artifacts = append(artifacts, spec.Name)
		if spec.ModuleName != "" {
			modules = append(modules, spec.ModuleName)
		}
	}
	if !value.DeviceIdentityMatched || !value.FactoryProfileExact || !value.NetworkRecovered ||
		!sameStringSet(value.ModulesAbsent, modules) || !sameStringSet(value.ArtifactsAbsent, artifacts) ||
		!value.RemoteRuntimeAbsent || !value.HelperAbsent || !value.AudioRouteDisabled {
		return errors.New("Phase-2 cleanup could not prove exact terminal restoration")
	}
	return nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		seen[value]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func validMavoPhase2Digest(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value)
}

func newMavoPhase2RunID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("generate Phase-2 evidence identifier")
	}
	return hex.EncodeToString(value[:]), nil
}

func newMavoPhase2Evidence(runID string, at time.Time, runtime *mavoPhase2Runtime, preflight mavoPhase2Preflight) mavoPhase2Evidence {
	artifacts := make([]mavoPhase2ArtifactEvidence, 0, len(runtime.artifacts))
	for _, artifact := range runtime.artifacts {
		artifacts = append(artifacts, mavoPhase2ArtifactEvidence{
			Name: artifact.Spec.Name, SHA256: artifact.Spec.SHA256, Size: artifact.Size,
			RemoteMode: artifact.Spec.RemoteMode, ModuleName: artifact.Spec.ModuleName,
		})
	}
	updatedAt := at.UTC().Format(time.RFC3339Nano)
	return mavoPhase2Evidence{
		Schema: mavoPhase2EvidenceV1, RunID: runID, Experiment: mavoPhase2ExperimentName,
		UpdatedAt: updatedAt, Phase: "armed", RuntimeVersion: runtime.version,
		Artifacts: artifacts, Preflight: preflight,
		Events: []mavoPhase2EvidenceEvent{{At: updatedAt, Phase: "armed", Outcome: "verified"}},
	}
}

var (
	mavoPhase2LongDigits = regexp.MustCompile(`[0-9]{7,}`)
	mavoPhase2LongHex    = regexp.MustCompile(`(?i)\b[0-9a-f]{32,}\b`)
	mavoPhase2Email      = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	mavoPhase2Secret     = regexp.MustCompile(`(?i)\b(token|secret|password|authorization|bearer|key)\s*[:=]\s*[^\s;,]+`)
	mavoPhase2UserPath   = regexp.MustCompile(`/Users/[^/\s]+`)
)

func mavoPhase2SafeError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(err.Error()))
	text = strings.Join(strings.Fields(text), " ")
	text = mavoPhase2Secret.ReplaceAllString(text, "$1=[redacted]")
	text = mavoPhase2Email.ReplaceAllString(text, "[redacted-email]")
	text = mavoPhase2UserPath.ReplaceAllString(text, "/Users/[redacted]")
	text = mavoPhase2LongHex.ReplaceAllString(text, "[redacted]")
	text = mavoPhase2LongDigits.ReplaceAllString(text, "[redacted]")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func mavoPhase2SanitizedError(label string, err error) error {
	detail := mavoPhase2SafeError(err)
	if detail == "" {
		return nil
	}
	return errors.New(strings.TrimSpace(label) + ": " + detail)
}

func mavoPhase2JoinSafe(errs ...error) error {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if detail := mavoPhase2SafeError(err); detail != "" {
			parts = append(parts, detail)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}

// mavoPhase2FileEvidenceSink is durable but not instantiated by production
// code. A future authorized adapter can place it under the existing private
// module-backups directory without changing the evidence format.
type mavoPhase2FileEvidenceSink struct {
	directory string
	mu        sync.Mutex
}

func newMavoPhase2FileEvidenceSink(directory string) (*mavoPhase2FileEvidenceSink, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "" || directory == "." || !filepath.IsAbs(directory) {
		return nil, errors.New("Phase-2 evidence directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, mavoPhase2SanitizedError("create Phase-2 evidence directory", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Phase-2 evidence directory is not a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, mavoPhase2SanitizedError("protect Phase-2 evidence directory", err)
	}
	return &mavoPhase2FileEvidenceSink{directory: directory}, nil
}

func (s *mavoPhase2FileEvidenceSink) arm(evidence mavoPhase2Evidence) (string, error) {
	if s == nil || !validMavoPhase2RunID(evidence.RunID) || evidence.Schema != mavoPhase2EvidenceV1 || evidence.Phase != "armed" {
		return "", errors.New("Phase-2 evidence arm request is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.directory, "mavo-phase2-"+evidence.RunID+".json")
	if err := writeMavoPhase2JSONExclusive(path, evidence); err != nil {
		return "", err
	}
	return path, nil
}

func (s *mavoPhase2FileEvidenceSink) record(path string, evidence mavoPhase2Evidence) error {
	if s == nil || !validMavoPhase2RunID(evidence.RunID) {
		return errors.New("Phase-2 evidence update is invalid")
	}
	expected := filepath.Join(s.directory, "mavo-phase2-"+evidence.RunID+".json")
	if filepath.Clean(path) != expected {
		return errors.New("Phase-2 evidence path changed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return replaceMavoPhase2JSON(path, evidence)
}

func validMavoPhase2RunID(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(value)
}

func writeMavoPhase2JSONExclusive(path string, value any) error {
	data, err := marshalMavoPhase2Evidence(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return mavoPhase2SanitizedError("create Phase-2 evidence", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return mavoPhase2SanitizedError("write Phase-2 evidence", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return mavoPhase2SanitizedError("sync Phase-2 evidence", err)
	}
	if err := file.Close(); err != nil {
		return mavoPhase2SanitizedError("close Phase-2 evidence", err)
	}
	return syncMavoPhase2Directory(filepath.Dir(path))
}

func replaceMavoPhase2JSON(path string, value any) error {
	data, err := marshalMavoPhase2Evidence(value)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".mavo-phase2-*.tmp")
	if err != nil {
		return mavoPhase2SanitizedError("create Phase-2 evidence update", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return mavoPhase2SanitizedError("protect Phase-2 evidence update", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return mavoPhase2SanitizedError("write Phase-2 evidence update", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return mavoPhase2SanitizedError("sync Phase-2 evidence update", err)
	}
	if err := temporary.Close(); err != nil {
		return mavoPhase2SanitizedError("close Phase-2 evidence update", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return mavoPhase2SanitizedError("replace Phase-2 evidence", err)
	}
	return syncMavoPhase2Directory(directory)
}

func marshalMavoPhase2Evidence(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, mavoPhase2SanitizedError("encode Phase-2 evidence", err)
	}
	return append(data, '\n'), nil
}

func syncMavoPhase2Directory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return mavoPhase2SanitizedError("open Phase-2 evidence directory", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return mavoPhase2SanitizedError("sync Phase-2 evidence directory", err)
	}
	return nil
}

// mavoPhase2DirectMediaAdapter is the minimal future connection point to the
// existing Swift VoiceAudioService and RemoteMediaControlClient. The Phase-2
// probe never calls it. A later call-lifecycle integration must bind every
// operation to the exact prepared USB lease and call generation; it must stop
// and clean up on every Start outcome.
type mavoPhase2DirectMediaAdapter interface {
	Prepare(context.Context, mavoPhase2MediaBinding) error
	Start(context.Context, mavoPhase2MediaBinding) (mavoPhase2MediaStartEvidence, error)
	Stop(context.Context, mavoPhase2MediaBinding) error
	Cleanup(context.Context, mavoPhase2MediaBinding) (mavoPhase2MediaCleanupEvidence, error)
}

type mavoPhase2MediaBinding struct {
	RuntimeLeaseID     string
	DeviceIdentityHash string
	USBLocationID      uint32
	USBLifecycle       uint64
	CallTicketHash     string
	CallGeneration     uint64
}

type mavoPhase2MediaStartEvidence struct {
	USBLeaseMatched      bool
	HostUACCallbacks     bool
	RemoteDownlinkFrames uint64
	RemoteUplinkFrames   uint64
	StartedAt            time.Time
}

type mavoPhase2MediaCleanupEvidence struct {
	StoppedGeneration uint64
	CallbacksInFlight uint32
	UACClosed         bool
	RemoteQueuesEmpty bool
}

func validateMavoPhase2MediaBinding(value mavoPhase2MediaBinding) error {
	if !validMavoPhase2RunID(value.RuntimeLeaseID) || !validMavoPhase2Digest(value.DeviceIdentityHash) ||
		value.USBLocationID == 0 || value.USBLifecycle == 0 || !validMavoPhase2Digest(value.CallTicketHash) ||
		value.CallGeneration == 0 {
		return errors.New("Phase-2 direct-media binding is incomplete")
	}
	return nil
}
