package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	mavoPhase1ExperimentName = "mavo_adb_uac_roundtrip"
	// Deliberately human-readable. The local API requires this exact phrase in
	// addition to confirm=true so a generic module-setup click cannot start the
	// experiment.
	mavoPhase1Confirmation  = "一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号"
	mavoPhase1EvidenceV1    = "mavo-phase1-evidence-v1"
	mavoPhase1Firmware      = "QDC507GLEFM21"
	legacyUACExperimentName = "legacy_uac_roundtrip"
	legacyUACConfirmation   = "一次 legacy UAC profile 往返，自动恢复，不推模块、不拨号"
	legacyUACEvidenceV1     = "legacy-uac-evidence-v1"
)

var mavoPhase1TargetComposition = usbComposition{
	VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID,
	Flags: []int{1, 1, 1, 1, 1, 1, 1},
}

type mavoPhase1TargetInspection struct {
	ADBDescriptorValid  bool   `json:"adb_descriptor_valid"`
	UACDescriptorValid  bool   `json:"uac_descriptor_valid"`
	CoreAudioValid      bool   `json:"coreaudio_valid,omitempty"`
	QPCMVProbePassed    bool   `json:"qpcmv_probe_passed,omitempty"`
	ADBRoot             bool   `json:"adb_root"`
	KernelRelease       string `json:"kernel_release,omitempty"`
	Architecture        string `json:"architecture,omitempty"`
	TTYGS0Present       bool   `json:"ttygs0_present"`
	VocServerPresent    bool   `json:"voc_server_present"`
	ALSAUCMTestPresent  bool   `json:"alsaucm_test_present"`
	AudioLibraryPresent bool   `json:"audio_library_present"`
	ARMELLoaderPresent  bool   `json:"armel_loader_present"`
	QDCModulesAbsent    bool   `json:"qdc_modules_absent"`
}

type mavoPhase1Evidence struct {
	Schema           string                      `json:"schema"`
	UpdatedAt        string                      `json:"updated_at"`
	Phase            string                      `json:"phase"`
	Original         moduleSetupSnapshot         `json:"original"`
	Target           usbComposition              `json:"target"`
	TargetInspection *mavoPhase1TargetInspection `json:"target_inspection,omitempty"`
	RollbackVerified bool                        `json:"rollback_verified"`
	Detail           string                      `json:"detail,omitempty"`
}

type mavoPhase1Result struct {
	MutationAttempted bool
	TargetVerified    bool
	RollbackVerified  bool
	EvidencePath      string
	Err               error
}

// mavoPhase1Hardware keeps orchestration independently fault-testable. The
// production implementation below is the only layer allowed to speak AT/ADB.
// None of these methods loads files onto the module, loads a kernel module, or
// issues a call command.
type mavoPhase1Hardware interface {
	preflight() (moduleSetupSnapshot, error)
	armEvidence(moduleSetupSnapshot) (string, error)
	recordEvidence(path, phase string, inspection *mavoPhase1TargetInspection, rollbackVerified bool, detail string) error
	setTarget(moduleSetupSnapshot) (attempted bool, err error)
	rebootAndVerifyTarget(moduleSetupSnapshot) error
	inspectTarget(moduleSetupSnapshot) (mavoPhase1TargetInspection, error)
	restoreOriginal(moduleSetupSnapshot) error
}

func executeMavoPhase1(hardware mavoPhase1Hardware) (result mavoPhase1Result) {
	if hardware == nil {
		result.Err = errors.New("phase-1 hardware adapter is nil")
		return result
	}

	original, err := hardware.preflight()
	if err != nil {
		result.Err = fmt.Errorf("read-only preflight failed: %w", err)
		return result
	}
	evidencePath, err := hardware.armEvidence(original)
	if err != nil {
		result.Err = fmt.Errorf("durable pre-mutation evidence failed: %w", err)
		return result
	}
	result.EvidencePath = evidencePath

	restored := false
	restore := func(reason error) {
		if restored || !result.MutationAttempted {
			return
		}
		restored = true
		rollbackErr := safeMavoPhase1Restore(hardware, original)
		if rollbackErr != nil {
			result.RollbackVerified = false
			result.Err = fmt.Errorf("%v; automatic rollback failed: %w", reason, rollbackErr)
			_ = hardware.recordEvidence(evidencePath, "recovery_required", nil, false, phase1SafeError(result.Err))
			return
		}
		result.RollbackVerified = true
		result.Err = reason
		_ = hardware.recordEvidence(evidencePath, "rolled_back", nil, true, phase1SafeError(reason))
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := fmt.Errorf("phase-1 panic: %v", recovered)
			if result.MutationAttempted && !restored {
				restore(panicErr)
			} else {
				result.Err = panicErr
			}
		}
	}()

	attempted, err := hardware.setTarget(original)
	result.MutationAttempted = attempted
	if err != nil || !attempted {
		if err == nil {
			err = errors.New("target composition write was not attempted")
		}
		restore(fmt.Errorf("target composition was not verified: %w", err))
		if !attempted {
			result.Err = err
			_ = hardware.recordEvidence(evidencePath, "failed_before_write", nil, false, phase1SafeError(err))
		}
		return result
	}
	if err := hardware.recordEvidence(evidencePath, "target_configured", nil, false, ""); err != nil {
		restore(fmt.Errorf("record target configuration: %w", err))
		return result
	}

	if err := hardware.rebootAndVerifyTarget(original); err != nil {
		restore(fmt.Errorf("target enumeration failed: %w", err))
		return result
	}
	if err := hardware.recordEvidence(evidencePath, "target_enumerated", nil, false, ""); err != nil {
		restore(fmt.Errorf("record target enumeration: %w", err))
		return result
	}

	inspection, err := hardware.inspectTarget(original)
	if err != nil {
		restore(fmt.Errorf("target read-only inspection failed: %w", err))
		return result
	}
	result.TargetVerified = true
	if err := hardware.recordEvidence(evidencePath, "target_verified", &inspection, false, ""); err != nil {
		restore(fmt.Errorf("record target inspection: %w", err))
		return result
	}

	restored = true
	if err := safeMavoPhase1Restore(hardware, original); err != nil {
		result.RollbackVerified = false
		result.Err = fmt.Errorf("target gates passed but automatic rollback failed: %w", err)
		_ = hardware.recordEvidence(evidencePath, "recovery_required", &inspection, false, phase1SafeError(result.Err))
		return result
	}
	result.RollbackVerified = true
	if err := hardware.recordEvidence(evidencePath, "completed", &inspection, true, ""); err != nil {
		result.Err = fmt.Errorf("hardware restored but final evidence write failed: %w", err)
		return result
	}
	return result
}

func safeMavoPhase1Restore(hardware mavoPhase1Hardware, original moduleSetupSnapshot) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("rollback panic: %v", recovered)
		}
	}()
	return hardware.restoreOriginal(original)
}

var phase1SensitiveDigits = regexp.MustCompile(`[0-9]{12,}`)

func phase1SafeError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " "))
	text = phase1SensitiveDigits.ReplaceAllString(text, "[redacted]")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

type liveMavoPhase1Hardware struct {
	app                   *app
	locationID            uint32
	targetComposition     usbComposition
	requireADB            bool
	evidenceSchema        string
	targetObserved        usbComposition
	targetRebootAttempted bool
	evidencePath          string
	evidence              mavoPhase1Evidence
}

func (h *liveMavoPhase1Hardware) target() usbComposition {
	if len(h.targetComposition.Flags) == 7 {
		return h.targetComposition
	}
	return mavoPhase1TargetComposition
}

func (h *liveMavoPhase1Hardware) targetMatches(composition usbComposition) bool {
	if h.requireADB {
		return isMavoPhase1FullComposition(composition)
	}
	return composition.isUACTarget()
}

func isMavoPhase1FullComposition(composition usbComposition) bool {
	if len(composition.Flags) != 7 || strings.Join(intSliceStrings(composition.Flags), ",") != "1,1,1,1,1,1,1" {
		return false
	}
	return (composition.VendorID == quectelUSBVendorID && composition.ProductID == quectelUSBProductID) ||
		(composition.VendorID == djiUSBVendorID && composition.ProductID == djiUSBProductID)
}

func (h *liveMavoPhase1Hardware) activeTargetComposition() usbComposition {
	if h.targetMatches(h.targetObserved) {
		return h.targetObserved
	}
	return h.target()
}

func (h *liveMavoPhase1Hardware) preflight() (moduleSetupSnapshot, error) {
	if h.app == nil || h.locationID == 0 {
		return moduleSetupSnapshot{}, errors.New("phase-1 USB location is unavailable")
	}
	if !h.app.moduleSetupCallIsIdle() {
		return moduleSetupSnapshot{}, errors.New("application call state is not idle")
	}
	if h.app.directQPCMVBlocksPersistentMutation() {
		return moduleSetupSnapshot{}, errors.New("an existing audio route blocks phase-1")
	}
	original, err := h.app.readModuleSetupSnapshotAtLocation(h.locationID)
	if err != nil {
		return moduleSetupSnapshot{}, err
	}
	if !original.USB.isFactoryDJI() {
		return moduleSetupSnapshot{}, fmt.Errorf("phase-1 requires exact factory USB composition, found %s", original.USB.command())
	}
	if original.IMSVoLTECapability != 1 {
		return moduleSetupSnapshot{}, errors.New("module does not report VoLTE capability")
	}
	if err := h.app.verifyPhysicalUSBIdentity(original.USB, original.USBLocationID); err != nil {
		return moduleSetupSnapshot{}, err
	}
	if err := h.validateReadOnlyGates(original, true); err != nil {
		return moduleSetupSnapshot{}, err
	}
	return original, nil
}

func (h *liveMavoPhase1Hardware) validateReadOnlyGates(original moduleSetupSnapshot, requireNetwork bool) error {
	return h.app.withExclusiveUSBATCommandsAtLocation(h.locationID, func(command usbATCommandFunc) error {
		query := func(at, label string) (string, error) {
			response, err := command(at, 5*time.Second)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", label, err)
			}
			if atResponseIsError(response) || !atQuerySucceeded(response) {
				return "", fmt.Errorf("read %s: query was not accepted", label)
			}
			return response, nil
		}
		identityResponse, err := query("AT+CGSN", "module identity")
		if err != nil {
			return err
		}
		imei, err := parseModuleIMEI(identityResponse)
		if err != nil || moduleIdentityDigest(imei) != original.DeviceIdentityHash {
			return errors.New("module identity changed during phase-1")
		}
		firmwareResponse, err := query("AT+CGMR", "firmware")
		if err != nil {
			return err
		}
		if !atResponseHasExactValue(firmwareResponse, mavoPhase1Firmware) {
			return errors.New("module firmware is not the validated QDC507GLEFM21 build")
		}
		callResponse, err := query("AT+CLCC", "call state")
		if err != nil {
			return err
		}
		if err := validateCLCCVoiceIdleForModuleSetup(callResponse); err != nil {
			return errors.New("strict CLCC gate did not prove voice-idle state")
		}
		volteResponse, err := query(`AT+QCFG="volte_disable"`, "VoLTE disable flag")
		if err != nil || !volteIsEnabled(volteResponse) {
			return errors.New("VoLTE is not confirmed enabled")
		}
		if !requireNetwork {
			return nil
		}
		pinResponse, err := query("AT+CPIN?", "SIM state")
		if err != nil || !atResponseHasExactPrefixedValue(pinResponse, "+CPIN:", "READY") {
			return errors.New("SIM is not ready")
		}
		registrationResponse, err := query("AT+CEREG?", "EPS registration")
		if err != nil {
			return err
		}
		registration, err := parsePhase1Registration(registrationResponse)
		if err != nil || (registration != 1 && registration != 5) {
			return errors.New("EPS registration is not home or roaming registered")
		}
		attachResponse, err := query("AT+CGATT?", "packet attachment")
		if err != nil || !atResponseHasExactPrefixedValue(attachResponse, "+CGATT:", "1") {
			return errors.New("packet service is not attached")
		}
		return nil
	})
}

func atResponseHasExactValue(response, expected string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		if strings.TrimSpace(line) == expected {
			return true
		}
	}
	return false
}

func atResponseHasExactPrefixedValue(response, prefix, expected string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(prefix)) &&
			strings.TrimSpace(line[len(prefix):]) == expected {
			return true
		}
	}
	return false
}

func parsePhase1Registration(response string) (int, error) {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "+CEREG:") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line[len("+CEREG:"):]), ",")
		if len(fields) == 0 {
			break
		}
		candidate := fields[0]
		if len(fields) >= 2 {
			candidate = fields[1]
		}
		value, err := strconv.Atoi(strings.TrimSpace(candidate))
		if err != nil {
			return 0, err
		}
		return value, nil
	}
	return 0, errors.New("missing CEREG status")
}

func parseMavoPhase1TargetOutput(output string) (mavoPhase1TargetInspection, error) {
	expected := map[string]bool{
		"uid": true, "kernel": true, "arch": true, "ttygs0": true,
		"voc_server": true, "alsaucm_test": true, "audio_library": true,
		"armel_loader": true, "qdc_modules_absent": true,
	}
	values := make(map[string]string, len(expected))
	for _, raw := range strings.Split(strings.ReplaceAll(output, "\r", ""), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "__MAVO_STATUS_") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !expected[key] || value == "" {
			return mavoPhase1TargetInspection{}, errors.New("ADB target probe returned an unexpected field")
		}
		if _, duplicate := values[key]; duplicate {
			return mavoPhase1TargetInspection{}, errors.New("ADB target probe returned a duplicate field")
		}
		values[key] = value
	}
	if len(values) != len(expected) {
		return mavoPhase1TargetInspection{}, errors.New("ADB target probe did not return every required field")
	}
	flag := func(key string) (bool, error) {
		switch values[key] {
		case "1":
			return true, nil
		case "0":
			return false, nil
		default:
			return false, fmt.Errorf("ADB target probe returned invalid %s flag", key)
		}
	}
	ttygs0, err := flag("ttygs0")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	vocServer, err := flag("voc_server")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	alsaucmTest, err := flag("alsaucm_test")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	audioLibrary, err := flag("audio_library")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	armelLoader, err := flag("armel_loader")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	qdcModulesAbsent, err := flag("qdc_modules_absent")
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	inspection := mavoPhase1TargetInspection{
		ADBRoot: values["uid"] == "0", KernelRelease: values["kernel"], Architecture: values["arch"],
		TTYGS0Present: ttygs0, VocServerPresent: vocServer, ALSAUCMTestPresent: alsaucmTest,
		AudioLibraryPresent: audioLibrary, ARMELLoaderPresent: armelLoader, QDCModulesAbsent: qdcModulesAbsent,
	}
	if !inspection.ADBRoot || inspection.KernelRelease != trustedVoiceKernelRelease ||
		inspection.Architecture != "armv7l" || !inspection.TTYGS0Present || !inspection.VocServerPresent ||
		!inspection.ALSAUCMTestPresent || !inspection.AudioLibraryPresent || !inspection.ARMELLoaderPresent ||
		!inspection.QDCModulesAbsent {
		return inspection, errors.New("ADB target does not satisfy the fixed MaVo runtime prerequisites")
	}
	return inspection, nil
}

func (h *liveMavoPhase1Hardware) armEvidence(original moduleSetupSnapshot) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Application Support", "MacCellular", "module-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	prefix := "mavo-phase1"
	if h.evidenceSchema == legacyUACEvidenceV1 {
		prefix = "legacy-uac"
	}
	path := filepath.Join(dir, prefix+"-"+time.Now().Format("20060102-150405")+".json")
	h.evidence = mavoPhase1Evidence{
		Schema: mavoPhase1EvidenceV1, UpdatedAt: time.Now().Format(time.RFC3339), Phase: "armed",
		Original: original, Target: h.target(),
	}
	if h.evidenceSchema != "" {
		h.evidence.Schema = h.evidenceSchema
	}
	if err := createPrivateJSON(path, h.evidence); err != nil {
		return "", err
	}
	h.evidencePath = path
	return path, nil
}

func (h *liveMavoPhase1Hardware) recordEvidence(path, phase string, inspection *mavoPhase1TargetInspection, rollbackVerified bool, detail string) error {
	if path == "" || path != h.evidencePath {
		return errors.New("phase-1 evidence path changed")
	}
	h.evidence.UpdatedAt = time.Now().Format(time.RFC3339)
	h.evidence.Phase = phase
	h.evidence.TargetInspection = inspection
	h.evidence.RollbackVerified = rollbackVerified
	h.evidence.Detail = phase1SafeError(errors.New(detail))
	if detail == "" {
		h.evidence.Detail = ""
	}
	return replacePrivateJSON(path, h.evidence)
}

func createPrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func replacePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".mavo-phase1-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (h *liveMavoPhase1Hardware) setTarget(original moduleSetupSnapshot) (bool, error) {
	target := h.target()
	if h.requireADB {
		challengeResponse, err := h.app.runATCommand("AT+QADBKEY?", 3*time.Second)
		if err != nil || !atQuerySucceeded(challengeResponse) {
			return false, errors.New("module did not return the ADB unlock challenge")
		}
		challenge, err := parseQADBChallenge(challengeResponse)
		if err != nil {
			return false, err
		}
		key, err := deriveQADBKey(challenge)
		if err != nil {
			return false, err
		}
		unlockResponse, attempted, err := h.app.runVerifiedModuleSetupCommand(original, `AT+QADBKEY="`+key+`"`, 5*time.Second)
		if err != nil || !attempted || !atQuerySucceeded(unlockResponse) || atResponseIsError(unlockResponse) {
			return attempted, errors.New("module rejected the ADB unlock response")
		}
	}
	response, attempted, err := h.app.runVerifiedModuleSetupCommand(original, target.command(), 8*time.Second)
	if err != nil {
		return attempted, err
	}
	if atResponseIsError(response) || !atQuerySucceeded(response) {
		return attempted, errors.New("module did not clearly accept target composition")
	}
	readBack, err := h.app.readModuleSetupSnapshotAtLocation(h.locationID)
	if err != nil {
		return attempted, err
	}
	if !h.targetMatches(readBack.USB) || readBack.USBNetMode != original.USBNetMode ||
		readBack.IMSConfiguration != original.IMSConfiguration ||
		readBack.IMSVoLTECapability != original.IMSVoLTECapability || !sameModuleSetupIdentity(original, readBack) {
		return attempted, fmt.Errorf(
			"target write-back mismatch: actual=%s usb=%t usbnet=%t ims=%t volte=%t location=%t identity=%t",
			readBack.USB.command(),
			h.targetMatches(readBack.USB),
			readBack.USBNetMode == original.USBNetMode,
			readBack.IMSConfiguration == original.IMSConfiguration,
			readBack.IMSVoLTECapability == original.IMSVoLTECapability,
			readBack.USBLocationID == original.USBLocationID,
			readBack.DeviceIdentityHash == original.DeviceIdentityHash,
		)
	}
	h.targetObserved = readBack.USB
	h.evidence.Target = readBack.USB
	return attempted, nil
}

func (h *liveMavoPhase1Hardware) rebootAndVerifyTarget(original moduleSetupSnapshot) error {
	response, attempted, execution, err := h.app.runVerifiedModuleSetupCommandExact(original, "AT+CFUN=1,1", 4*time.Second)
	h.targetRebootAttempted = attempted
	if !moduleRebootAccepted(response, attempted, err) {
		return errors.New("target reboot was not clearly accepted")
	}
	h.app.markUSBATExecutionDetached(execution, "MaVo phase-1 target reboot")
	reenumerationTimeout := 90 * time.Second
	if !h.requireADB {
		// This hardware was observed taking a little over three minutes to
		// return as the legacy 2c7c:0125 composition. The earlier 90-second
		// bound caused both verification and rollback to give up just before
		// the same physical module became reachable again.
		reenumerationTimeout = 240 * time.Second
	}
	deadline := time.Now().Add(reenumerationTimeout)
	for time.Now().Before(deadline) {
		if err := h.app.ensureUSBATAtLocation(h.locationID); err == nil {
			if err := h.app.verifyPhysicalUSBIdentity(h.activeTargetComposition(), h.locationID); err == nil {
				verified, verifyErr := h.app.readModuleSetupSnapshotAtLocation(h.locationID)
				if verifyErr == nil && h.targetMatches(verified.USB) &&
					verified.USBNetMode == original.USBNetMode &&
					verified.IMSConfiguration == original.IMSConfiguration &&
					verified.IMSVoLTECapability == original.IMSVoLTECapability && sameModuleSetupIdentity(original, verified) {
					return nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	if h.requireADB {
		return errors.New("ADB+UAC target did not re-enumerate at the original USB location")
	}
	return errors.New("legacy UAC target did not re-enumerate at the original USB location")
}

func (h *liveMavoPhase1Hardware) inspectTarget(original moduleSetupSnapshot) (mavoPhase1TargetInspection, error) {
	if err := h.app.verifyPhysicalUSBIdentity(h.activeTargetComposition(), h.locationID); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	if err := h.validateReadOnlyGates(original, false); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	if h.requireADB {
		return inspectMavoPhase1Target(h.locationID)
	}
	if err := validateDirectUACUSB(h.locationID); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	if err := validateDirectUACCoreAudio(h.locationID); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	if err := h.app.preflightDirectQPCMVExpected(original); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	return mavoPhase1TargetInspection{
		UACDescriptorValid: true,
		CoreAudioValid:     true,
		QPCMVProbePassed:   true,
	}, nil
}

func (h *liveMavoPhase1Hardware) restoreOriginal(original moduleSetupSnapshot) error {
	reacquireTimeout := 90 * time.Second
	if !h.requireADB {
		reacquireTimeout = 240 * time.Second
	}
	deadline := time.Now().Add(reacquireTimeout)
	var current moduleSetupSnapshot
	var err error
	for time.Now().Before(deadline) {
		if err = h.app.ensureUSBATAtLocation(h.locationID); err == nil {
			current, err = h.app.readModuleSetupSnapshotAtLocation(h.locationID)
			if err == nil && sameModuleSetupIdentity(original, current) {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || !sameModuleSetupIdentity(original, current) {
		return errors.New("could not reacquire the exact module for rollback")
	}
	if current.USBNetMode != original.USBNetMode || current.IMSConfiguration != original.IMSConfiguration ||
		current.IMSVoLTECapability != original.IMSVoLTECapability {
		return errors.New("non-USBCFG state drifted; refusing an expanded rollback")
	}

	physical, err := h.currentPhysicalIdentity()
	if err != nil {
		return err
	}
	if current.USB.command() != original.USB.command() {
		response, attempted, writeErr := h.app.runVerifiedModuleSetupCommand(original, original.USB.command(), 8*time.Second)
		if writeErr != nil || !attempted || atResponseIsError(response) || !atQuerySucceeded(response) {
			return errors.New("original USB composition was not clearly restored")
		}
		current, err = h.app.readModuleSetupSnapshotAtLocation(h.locationID)
		if err != nil || current.USB.command() != original.USB.command() || !sameModuleSetupIdentity(original, current) {
			return errors.New("original USB composition failed exact read-back")
		}
	}

	if !h.targetRebootAttempted && usbPhysicalIdentityMatchesComposition(physical, original.USB, h.locationID) {
		return h.waitForNetworkRecovery(original, 30*time.Second)
	}
	if !usbPhysicalIdentityMatchesComposition(physical, h.activeTargetComposition(), h.locationID) &&
		!usbPhysicalIdentityMatchesComposition(physical, original.USB, h.locationID) {
		return errors.New("rollback found an unexpected physical USB identity")
	}
	response, attempted, execution, rebootErr := h.app.runVerifiedModuleSetupCommandExact(original, "AT+CFUN=1,1", 4*time.Second)
	if !moduleRebootAccepted(response, attempted, rebootErr) {
		return errors.New("rollback reboot was not clearly accepted")
	}
	h.app.markUSBATExecutionDetached(execution, "MaVo phase-1 rollback reboot")

	deadline = time.Now().Add(reacquireTimeout)
	for time.Now().Before(deadline) {
		if err := h.app.ensureUSBATAtLocation(h.locationID); err == nil &&
			h.app.verifyPhysicalUSBIdentity(original.USB, h.locationID) == nil {
			verified, verifyErr := h.app.readModuleSetupSnapshotAtLocation(h.locationID)
			if verifyErr == nil && verified.USB.command() == original.USB.command() &&
				verified.USBNetMode == original.USBNetMode &&
				verified.IMSConfiguration == original.IMSConfiguration &&
				verified.IMSVoLTECapability == original.IMSVoLTECapability && sameModuleSetupIdentity(original, verified) {
				return h.waitForNetworkRecovery(original, 90*time.Second)
			}
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("factory USB composition was not verified after rollback reboot")
}

func (h *liveMavoPhase1Hardware) currentPhysicalIdentity() (usbATPhysicalIdentity, error) {
	h.app.usbATOpenMu.Lock()
	defer h.app.usbATOpenMu.Unlock()
	if h.app.usbAT == nil || h.app.usbAT.LocationID() != h.locationID {
		return usbATPhysicalIdentity{}, errors.New("USB AT handle is not bound to the phase-1 location")
	}
	identity := h.app.usbAT.PhysicalIdentity()
	if identity.Location != h.locationID {
		return usbATPhysicalIdentity{}, errors.New("USB physical location changed")
	}
	return identity, nil
}

func (h *liveMavoPhase1Hardware) waitForNetworkRecovery(original moduleSetupSnapshot, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := h.app.verifyPhysicalUSBIdentity(original.USB, h.locationID); err != nil {
			lastErr = err
		} else if err := h.validateReadOnlyGates(original, true); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("factory profile returned but SIM/LTE recovery was not verified: %w", lastErr)
}

func (a *app) startMavoPhase1API(w http.ResponseWriter, confirmation string) {
	if confirmation != mavoPhase1Confirmation {
		writeError(w, http.StatusBadRequest, "需要精确确认一次 MaVo ADB+UAC 往返实验")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	a.moduleSetupMu.Lock()
	running := a.moduleSetup.State == "initializing" || a.moduleSetup.State == "restarting" ||
		a.moduleSetup.State == "verifying" || strings.HasPrefix(a.moduleSetup.State, "mavo_phase1_running")
	if running {
		a.moduleSetupMu.Unlock()
		a.moduleMutationMu.Unlock()
		writeError(w, http.StatusConflict, "模块实验正在进行")
		return
	}
	a.moduleSetup = moduleSetupStatus{
		State: "mavo_phase1_running", Summary: "正在执行 MaVo ADB+UAC 可逆前置实验",
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	status := a.moduleSetup
	a.moduleSetupMu.Unlock()
	go a.runMavoPhase1Locked()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) runMavoPhase1Locked() {
	defer a.moduleMutationMu.Unlock()
	locationID, err := a.claimCurrentUSBATLocation()
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "mavo_phase1_failed", Summary: "MaVo phase 1 只读安全门未通过", Detail: phase1SafeError(err)})
		return
	}
	defer a.releaseUSBATLocation(locationID)
	hardware := &liveMavoPhase1Hardware{
		app: a, locationID: locationID, targetComposition: mavoPhase1TargetComposition,
		requireADB: true, evidenceSchema: mavoPhase1EvidenceV1,
	}
	result := executeMavoPhase1(hardware)
	status := moduleSetupStatus{BackupPath: result.EvidencePath}
	switch {
	case result.TargetVerified && result.RollbackVerified && result.Err == nil:
		status.State = "mavo_phase1_complete"
		status.Summary = "MaVo phase 1 通过，已验证 ADB/UAC/kernel 并恢复工厂配置"
		status.Detail = "这不是通话音频或生产可用性证明"
	case result.MutationAttempted && result.RollbackVerified:
		status.State = "mavo_phase1_rolled_back"
		status.Summary = "MaVo phase 1 未通过，已自动恢复工厂配置"
		status.Detail = phase1SafeError(result.Err)
	case result.MutationAttempted:
		status.State = "mavo_phase1_recovery_required"
		status.Summary = "MaVo phase 1 自动恢复未获验证，请勿拔出或重启模块"
		status.Detail = phase1SafeError(result.Err)
	default:
		status.State = "mavo_phase1_failed"
		status.Summary = "MaVo phase 1 在任何持久写入前停止"
		status.Detail = phase1SafeError(result.Err)
	}
	a.setModuleSetup(status)
}
