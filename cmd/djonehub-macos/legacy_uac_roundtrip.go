package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const legacyUACRecoveryExperimentName = "legacy_uac_restore"

var legacyUACTargetComposition = usbComposition{
	VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID,
	Flags: []int{1, 1, 1, 1, 1, 0, 1},
}

func (a *app) startLegacyUACAPI(w http.ResponseWriter, confirmation string) {
	if confirmation != legacyUACConfirmation {
		writeError(w, http.StatusBadRequest, "需要精确确认一次 legacy UAC 往返实验")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	a.moduleSetupMu.Lock()
	running := a.moduleSetup.State == "initializing" || a.moduleSetup.State == "restarting" ||
		a.moduleSetup.State == "verifying" || strings.HasPrefix(a.moduleSetup.State, "mavo_phase1_running") ||
		strings.HasPrefix(a.moduleSetup.State, "legacy_uac_running")
	if running {
		a.moduleSetupMu.Unlock()
		a.moduleMutationMu.Unlock()
		writeError(w, http.StatusConflict, "模块实验正在进行")
		return
	}
	a.moduleSetup = moduleSetupStatus{
		State: "legacy_uac_running", Summary: "正在执行 legacy UAC 可逆前置实验",
	}
	status := a.moduleSetup
	a.moduleSetupMu.Unlock()
	go a.runLegacyUACLocked()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) runLegacyUACLocked() {
	defer a.moduleMutationMu.Unlock()
	locationID, err := a.claimCurrentUSBATLocation()
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "legacy_uac_failed", Summary: "legacy UAC 只读安全门未通过", Detail: phase1SafeError(err)})
		return
	}
	defer a.releaseUSBATLocation(locationID)
	hardware := &liveMavoPhase1Hardware{
		app: a, locationID: locationID, targetComposition: legacyUACTargetComposition,
		requireADB: false, evidenceSchema: legacyUACEvidenceV1,
	}
	result := executeMavoPhase1(hardware)
	status := moduleSetupStatus{BackupPath: result.EvidencePath}
	switch {
	case result.TargetVerified && result.RollbackVerified && result.Err == nil:
		status.State = "legacy_uac_complete"
		status.Summary = "legacy UAC 通过，已验证 UAC/CoreAudio/QPCMV 并恢复工厂配置"
		status.Detail = "这不是运营商 VoLTE 或真实通话证明"
	case result.MutationAttempted && result.RollbackVerified:
		status.State = "legacy_uac_rolled_back"
		status.Summary = "legacy UAC 未通过，已自动恢复工厂配置"
		status.Detail = phase1SafeError(result.Err)
	case result.MutationAttempted:
		status.State = "legacy_uac_recovery_required"
		status.Summary = "legacy UAC 自动恢复未获验证，请勿拔出或重启模块"
		status.Detail = phase1SafeError(result.Err)
	default:
		status.State = "legacy_uac_failed"
		status.Summary = "legacy UAC 在任何持久写入前停止"
		status.Detail = phase1SafeError(result.Err)
	}
	a.setModuleSetup(status)
}

// startLegacyUACRecoveryAPI is deliberately narrower than the normal setup
// path. It exists only to finish the rollback when the legacy UAC target
// re-enumerates after the original transaction's bounded reacquire window.
// It accepts neither an arbitrary composition nor an arbitrary AT command.
func (a *app) startLegacyUACRecoveryAPI(w http.ResponseWriter, confirmation string) {
	if confirmation != legacyUACConfirmation {
		writeError(w, http.StatusBadRequest, "需要精确确认 legacy UAC 恢复")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	a.moduleSetupMu.RLock()
	evidencePath := ""
	if a.moduleSetup.State == "legacy_uac_recovery_required" {
		evidencePath = a.moduleSetup.BackupPath
	}
	a.moduleSetupMu.RUnlock()
	a.setModuleSetup(moduleSetupStatus{
		State: "legacy_uac_recovery_running", Summary: "正在恢复 legacy UAC 工厂配置",
		BackupPath: evidencePath,
	})
	go a.runLegacyUACRecoveryLocked(evidencePath)
	a.moduleSetupMu.RLock()
	status := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) runLegacyUACRecoveryLocked(evidencePath string) {
	defer a.moduleMutationMu.Unlock()
	locationID, err := a.claimCurrentUSBATLocation()
	if err != nil {
		a.setLegacyUACRecoveryRequired(err)
		return
	}
	defer a.releaseUSBATLocation(locationID)

	current, err := a.readModuleSetupSnapshotAtLocation(locationID)
	if err != nil {
		a.setLegacyUACRecoveryRequired(err)
		return
	}
	if current.USB.command() != legacyUACTargetComposition.command() {
		a.setLegacyUACRecoveryRequired(errors.New("current USB composition is not the exact legacy UAC target"))
		return
	}
	if err := a.verifyPhysicalUSBIdentity(current.USB, locationID); err != nil {
		a.setLegacyUACRecoveryRequired(err)
		return
	}

	original := current
	original.USB = usbComposition{
		VendorID: djiUSBVendorID, ProductID: djiUSBProductID,
		Flags: []int{1, 1, 1, 1, 1, 0, 0},
	}
	hardware := &liveMavoPhase1Hardware{
		app: a, locationID: locationID, targetComposition: legacyUACTargetComposition,
		targetObserved: current.USB, targetRebootAttempted: true,
	}
	if err := hardware.restoreOriginal(original); err != nil {
		a.setLegacyUACRecoveryRequired(err)
		return
	}
	detail := "恢复入口未推送模块、未拨号"
	if evidencePath != "" {
		if err := updateLegacyUACRecoveryEvidence(evidencePath, original, current); err != nil {
			detail += "；工厂配置已验证，但原实验记录更新失败：" + phase1SafeError(err)
		}
	}
	a.setModuleSetup(moduleSetupStatus{
		State: "legacy_uac_rolled_back", Summary: "legacy UAC 已恢复并验证工厂配置",
		Detail: detail, BackupPath: evidencePath,
	})
}

func (a *app) setLegacyUACRecoveryRequired(err error) {
	a.setModuleSetup(moduleSetupStatus{
		State: "legacy_uac_recovery_required", Summary: "legacy UAC 工厂恢复仍未获验证",
		Detail: phase1SafeError(err),
	})
}

func updateLegacyUACRecoveryEvidence(path string, original moduleSetupSnapshot, target moduleSetupSnapshot) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	expectedDir := filepath.Join(home, "Library", "Application Support", "MacCellular", "module-backups")
	cleanPath := filepath.Clean(path)
	base := filepath.Base(cleanPath)
	if filepath.Dir(cleanPath) != expectedDir || !strings.HasPrefix(base, "legacy-uac-") || filepath.Ext(base) != ".json" {
		return errors.New("legacy UAC recovery evidence path is outside the fixed backup directory")
	}
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > 64<<10 {
		return errors.New("legacy UAC recovery evidence is not a private bounded regular file")
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return err
	}
	var evidence mavoPhase1Evidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return errors.New("legacy UAC recovery evidence JSON is invalid")
	}
	if evidence.Schema != legacyUACEvidenceV1 || evidence.Original.USB.command() != original.USB.command() ||
		evidence.Target.command() != target.USB.command() || evidence.Original.USBNetMode != original.USBNetMode ||
		evidence.Original.IMSConfiguration != original.IMSConfiguration ||
		evidence.Original.IMSVoLTECapability != original.IMSVoLTECapability ||
		!sameModuleSetupIdentity(evidence.Original, original) {
		return errors.New("legacy UAC recovery evidence does not match the exact rollback transaction")
	}
	hardware := &liveMavoPhase1Hardware{evidencePath: cleanPath, evidence: evidence}
	return hardware.recordEvidence(cleanPath, "rolled_back", evidence.TargetInspection, true,
		"late target enumeration required the bounded recovery path")
}
