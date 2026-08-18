package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// moduleSetupStatus is intentionally a small state machine. It separates a
// harmless inspection from the one explicit action which may write the
// module's USB composition and cause a re-enumeration.
type moduleSetupStatus struct {
	State                string `json:"state"`
	Summary              string `json:"summary"`
	Detail               string `json:"detail,omitempty"`
	CanInitialize        bool   `json:"can_initialize"`
	RequiresConfirmation bool   `json:"requires_confirmation"`
	BackupPath           string `json:"backup_path,omitempty"`
	UpdatedAt            string `json:"updated_at"`
}

type usbComposition struct {
	VendorID  int   `json:"vendor_id"`
	ProductID int   `json:"product_id"`
	Flags     []int `json:"flags"`
}

type moduleSetupSnapshot struct {
	USB                usbComposition `json:"usb"`
	USBNetMode         int            `json:"usbnet_mode"`
	IMSConfiguration   int            `json:"ims_configuration"`
	IMSVoLTECapability int            `json:"ims_volte_capability"`
	USBLocationID      uint32         `json:"usb_location_id"`
	// The raw IMEI is never persisted or logged.  This digest is used only to
	// prevent a transaction or rollback from crossing over to another modem.
	DeviceIdentityHash string `json:"device_identity_sha256"`
}

var moduleIMEILinePattern = regexp.MustCompile(`^(?:\+CGSN:\s*)?([0-9]{15})$`)

func (c usbComposition) command() string {
	parts := []string{fmt.Sprintf("0x%04X", c.VendorID), fmt.Sprintf("0x%04X", c.ProductID)}
	for _, flag := range c.Flags {
		parts = append(parts, strconv.Itoa(flag))
	}
	return `AT+QCFG="USBCFG",` + strings.Join(parts, ",")
}

func (c usbComposition) hasUAC() bool {
	return len(c.Flags) >= 1 && c.Flags[len(c.Flags)-1] == 1
}

func (c usbComposition) hasADB() bool {
	return len(c.Flags) >= 2 && c.Flags[len(c.Flags)-2] == 1
}

func (c usbComposition) isUACTarget() bool {
	return c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID &&
		len(c.Flags) == 7 && strings.Join(intSliceStrings(c.Flags), ",") == "1,1,1,1,1,0,1"
}

// The all-functions composition also exposes ADB. Both the Quectel and DJI
// identities have been observed with this complete UAC layout. Treating an
// already-audio-capable module as a migration target is unnecessary and can
// turn a successful no-op write into a false setup failure.
func (c usbComposition) isADBAndUACTarget() bool {
	return c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID &&
		len(c.Flags) == 7 && strings.Join(intSliceStrings(c.Flags), ",") == "1,1,1,1,1,1,1"
}

func (c usbComposition) isFullUACTarget() bool {
	if len(c.Flags) != 7 || strings.Join(intSliceStrings(c.Flags), ",") != "1,1,1,1,1,1,1" {
		return false
	}
	return (c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID) ||
		(c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID)
}

func (c usbComposition) isCallAudioCapable() bool {
	return c.isUACTarget() || c.isFullUACTarget()
}

func (c usbComposition) isFactoryDJI() bool {
	return c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID &&
		len(c.Flags) == 7 && strings.Join(intSliceStrings(c.Flags), ",") == "1,1,1,1,1,0,0"
}

func intSliceStrings(values []int) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.Itoa(value))
	}
	return result
}

func parseUSBComposition(response string) (usbComposition, error) {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), `+qcfg: "usbcfg",`) {
			continue
		}
		comma := strings.Index(line, ",")
		if comma < 0 {
			break
		}
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) < 3 {
			break
		}
		parse := func(raw string) (int, error) {
			value, err := strconv.ParseInt(strings.TrimSpace(raw), 0, 32)
			return int(value), err
		}
		vendorID, err := parse(fields[0])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB vendor: %w", err)
		}
		productID, err := parse(fields[1])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB product: %w", err)
		}
		flags := make([]int, 0, len(fields)-2)
		for _, field := range fields[2:] {
			value, err := parse(field)
			if err != nil {
				return usbComposition{}, fmt.Errorf("parse USB flag: %w", err)
			}
			flags = append(flags, value)
		}
		return usbComposition{VendorID: vendorID, ProductID: productID, Flags: flags}, nil
	}
	return usbComposition{}, errors.New("模块没有返回可识别的 USBCFG")
}

func (a *app) inspectModuleSetup() moduleSetupStatus {
	if a.demo {
		return moduleSetupStatus{State: "ready", Summary: "演示模块已完成初始化", UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	response, err := a.runATCommand(`AT+QCFG="USBCFG"`, 4*time.Second)
	if err != nil {
		return moduleSetupStatus{State: "disconnected", Summary: "等待 4G 模块连接", Detail: err.Error(), UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	composition, err := parseUSBComposition(response)
	if err != nil {
		// During USB profile switching the AT endpoint can reappear before
		// USBCFG is readable again. Keep this as a reconnecting state instead
		// of classifying a transient ERROR as an unsupported module.
		if atResponseIsError(response) {
			return moduleSetupStatus{State: "reconnecting", Summary: "正在重新连接并读取 USB 配置", Detail: "模块正处于 USB 模式切换阶段，请稍候自动重试", UpdatedAt: time.Now().Format(time.RFC3339)}
		}
		return moduleSetupStatus{State: "unsupported", Summary: "无法识别模块 USB 配置", Detail: err.Error(), UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	if composition.isCallAudioCapable() {
		imsResponse, imsErr := a.runATCommand(`AT+QCFG="ims"`, 3*time.Second)
		imsConfig, volteCapability, parseErr := parseIMSConfiguration(imsResponse)
		if imsErr == nil && parseErr == nil && imsConfig == 1 && volteCapability == 1 {
			return moduleSetupStatus{State: "ready", Summary: "模块已完成 UAC 与 VoLTE 配置", Detail: composition.command(), UpdatedAt: time.Now().Format(time.RFC3339)}
		}
		return moduleSetupStatus{State: "needs_initialization", Summary: "模块音频已就绪，需要启用 VoLTE", Detail: composition.command(), CanInitialize: true, RequiresConfirmation: true, UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	if composition.isFactoryDJI() || composition.isFullUACTarget() {
		return moduleSetupStatus{State: "needs_initialization", Summary: "发现新模块，可初始化通话能力", Detail: composition.command(), CanInitialize: true, RequiresConfirmation: true, UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	return moduleSetupStatus{State: "unsupported", Summary: "模块 USB 配置不是可安全初始化的原始状态", Detail: composition.command(), UpdatedAt: time.Now().Format(time.RFC3339)}
}

func (a *app) moduleSetupStatusAPI(w http.ResponseWriter, _ *http.Request) {
	a.moduleSetupMu.RLock()
	current := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	if current.State == "initializing" || current.State == "restarting" || current.State == "verifying" ||
		current.State == "rolled_back" || strings.HasPrefix(current.State, "mavo_phase1_") ||
		strings.HasPrefix(current.State, "legacy_uac_") {
		writeJSON(w, http.StatusOK, current)
		return
	}
	status := a.inspectModuleSetup()
	writeJSON(w, http.StatusOK, status)
}

func (a *app) setModuleSetup(status moduleSetupStatus) {
	status.UpdatedAt = time.Now().Format(time.RFC3339)
	a.moduleSetupMu.Lock()
	a.moduleSetup = status
	a.moduleSetupMu.Unlock()
	// Keep the transaction observable without logging its detail field, which
	// may contain raw transport errors. State and the fixed Chinese summary are
	// sufficient to distinguish ready, rollback and fail-closed outcomes.
	log.Printf("module setup state=%s summary=%s", status.State, status.Summary)
}

func (a *app) moduleSetupStartAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm            bool   `json:"confirm"`
		Experiment         string `json:"experiment"`
		Phase1Confirmation string `json:"phase1_confirmation"`
		LegacyConfirmation string `json:"legacy_uac_confirmation"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Experiment != "" {
		switch body.Experiment {
		case mavoPhase1ExperimentName:
			if !body.Confirm {
				writeError(w, http.StatusBadRequest, "MaVo phase 1 需要明确确认")
				return
			}
			a.startMavoPhase1API(w, body.Phase1Confirmation)
		case legacyUACExperimentName:
			if !body.Confirm {
				writeError(w, http.StatusBadRequest, "legacy UAC 实验需要明确确认")
				return
			}
			a.startLegacyUACAPI(w, body.LegacyConfirmation)
		case legacyUACRecoveryExperimentName:
			if !body.Confirm {
				writeError(w, http.StatusBadRequest, "legacy UAC 恢复需要明确确认")
				return
			}
			a.startLegacyUACRecoveryAPI(w, body.LegacyConfirmation)
		default:
			writeError(w, http.StatusBadRequest, "未知的模块实验")
		}
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要确认后才会初始化模块")
		return
	}
	inspection := a.inspectModuleSetup()
	if !inspection.CanInitialize {
		writeError(w, http.StatusConflict, inspection.Summary)
		return
	}
	// Claim the global modem mutation transaction before publishing the async
	// setup state. USB-mode changes, eSIM writes, recovery and reboots must not
	// interleave with this transaction.
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	// Claim the transaction atomically. Two simultaneous confirmation requests
	// must never launch two persistent writers after both complete inspection.
	a.moduleSetupMu.Lock()
	running := a.moduleSetup.State == "initializing" || a.moduleSetup.State == "restarting" || a.moduleSetup.State == "verifying"
	if running {
		a.moduleSetupMu.Unlock()
		a.moduleMutationMu.Unlock()
		writeError(w, http.StatusConflict, "模块初始化正在进行")
		return
	}
	a.moduleSetup = moduleSetupStatus{
		State: "initializing", Summary: "正在备份并初始化模块", Detail: inspection.Detail,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	a.moduleSetupMu.Unlock()
	go a.runModuleSetupLocked()
	a.moduleSetupMu.RLock()
	status := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) runModuleSetupLocked() {
	defer a.moduleMutationMu.Unlock()
	locationID, err := a.claimCurrentUSBATLocation()
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法锁定当前物理模块", Detail: err.Error()})
		return
	}
	defer a.releaseUSBATLocation(locationID)
	original, err := a.readModuleSetupSnapshotAtLocation(locationID)
	if err != nil || !(original.USB.isFactoryDJI() || original.USB.isCallAudioCapable()) {
		detail := "模块 USB 配置已变化，停止初始化"
		if err != nil {
			detail = err.Error()
		}
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "初始化前校验未通过", Detail: detail})
		return
	}
	if !a.moduleSetupCallIsIdle() {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "检测到通话，未执行模块初始化"})
		return
	}
	if a.directQPCMVBlocksPersistentMutation() {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "通话音频路由尚未安全关闭，未执行模块初始化"})
		return
	}
	if err := a.verifyModuleSetupIdentity(original); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "模块身份校验失败", Detail: err.Error()})
		return
	}
	callResponse, err := a.runATCommand("AT+CLCC", 5*time.Second)
	callValidationErr := validateCLCCVoiceIdleForModuleSetup(callResponse)
	if err != nil || callValidationErr != nil {
		detail := errString(err)
		if callValidationErr != nil {
			detail = callValidationErr.Error()
		}
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法确认模块当前没有语音通话", Detail: detail})
		return
	}
	if !original.USB.hasUAC() {
		capability, capabilityErr := a.runATCommand("AT+QPCMV=?", 5*time.Second)
		if capabilityErr != nil || atResponseIsError(capability) ||
			!strings.Contains(strings.ToUpper(capability), "+QPCMV:") || !strings.Contains(capability, "0-2") {
			a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "模块未确认支持 USB 通话音频", Detail: firstNonEmpty(errString(capabilityErr), capability)})
			return
		}
	}
	volteResponse, err := a.runATCommand(`AT+QCFG="volte_disable"`, 5*time.Second)
	if err != nil || !volteIsEnabled(volteResponse) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法确认 VoLTE 已启用", Detail: firstNonEmpty(errString(err), volteResponse)})
		return
	}
	backupPath, err := saveModuleSetupBackup(original)
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法保存模块回滚备份", Detail: err.Error()})
		return
	}
	// This is the documented ADB-off/UAC-on composition: the final flag enables USB
	// Audio and the penultimate ADB flag deliberately remains disabled. Direct
	// QPCMV routing needs no privileged module-side runtime.
	target := usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
	expectedUSB := target
	if original.USB.isCallAudioCapable() {
		// v1.2.9 upstream explicitly preserves both known UAC layouts. The
		// existing composition is the expected post-reboot identity; do not
		// rewrite it merely to make the local direct-QPCMV target canonical.
		expectedUSB = original.USB
	}
	if !original.USB.isCallAudioCapable() {
		write, attempted, writeErr := a.runVerifiedModuleSetupCommand(original, target.command(), 8*time.Second)
		if writeErr != nil {
			reason := "USB 音频配置写入响应不明确"
			if !attempted {
				reason = "USB 音频配置写入前安全校验失败"
			}
			a.rollbackModuleSetup(original, backupPath, reason+": "+writeErr.Error())
			return
		}
		if atResponseIsError(write) {
			a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "模块拒绝 USB 音频配置", Detail: write, BackupPath: backupPath})
			return
		}
		readBack, readErr := a.runATCommand(`AT+QCFG="USBCFG"`, 5*time.Second)
		actual, parseErr := parseUSBComposition(readBack)
		if readErr != nil || parseErr != nil || !actual.isCallAudioCapable() {
			a.rollbackModuleSetup(original, backupPath, "USB 配置写入后的精确回读未通过: "+firstNonEmpty(errString(readErr), errString(parseErr), readBack))
			return
		}
	}
	// Keep the original data-mode composition. Voice setup only requires UAC.
	// Forcing ECM during the same transaction expands the blast radius
	// and is unnecessary for call/SMS validation; network setup stays behind
	// its own explicit control.
	if original.IMSConfiguration != 1 {
		imsWrite, attempted, writeErr := a.runVerifiedModuleSetupCommand(original, `AT+QCFG="ims",1`, 5*time.Second)
		if writeErr != nil || atResponseIsError(imsWrite) {
			detail := firstNonEmpty(errString(writeErr), imsWrite)
			if !attempted {
				detail = "写入前安全校验失败: " + detail
			}
			a.rollbackModuleSetup(original, backupPath, "IMS 配置写入失败: "+detail)
			return
		}
		imsReadBack, readErr := a.runATCommand(`AT+QCFG="ims"`, 5*time.Second)
		imsConfig, _, imsParseErr := parseIMSConfiguration(imsReadBack)
		if readErr != nil || imsParseErr != nil || imsConfig != 1 {
			a.rollbackModuleSetup(original, backupPath, "IMS 配置回读未通过: "+firstNonEmpty(errString(readErr), errString(imsParseErr), imsReadBack))
			return
		}
	}
	finalSnapshot, err := a.readModuleSetupSnapshotAtLocation(original.USBLocationID)
	if err != nil || finalSnapshot.USB.command() != expectedUSB.command() || finalSnapshot.USBNetMode != original.USBNetMode ||
		finalSnapshot.IMSConfiguration != 1 || finalSnapshot.IMSVoLTECapability != 1 ||
		!sameModuleSetupIdentity(original, finalSnapshot) {
		a.rollbackModuleSetup(original, backupPath, "重启前的最终配置核对未通过: "+errString(err))
		return
	}
	a.setModuleSetup(moduleSetupStatus{State: "restarting", Summary: "模块正在重启并重新识别", BackupPath: backupPath})
	// A successful CFUN reboot often detaches USB before an OK reply arrives.
	// The command must never be repeated; re-enumeration plus exact read-back is
	// the only success criterion below.
	if !a.moduleSetupCallIsIdle() {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "检测到通话，未执行模块重启", BackupPath: backupPath})
		return
	}
	if err := a.verifyModuleSetupIdentity(original); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "重启前模块身份已变更，未执行重启", Detail: err.Error(), BackupPath: backupPath})
		return
	}
	rebootResponse, rebootAttempted, rebootExecution, rebootErr := a.runVerifiedModuleSetupCommandExact(original, "AT+CFUN=1,1", 4*time.Second)
	if !moduleRebootAccepted(rebootResponse, rebootAttempted, rebootErr) {
		a.rollbackModuleSetup(original, backupPath, "重启前安全校验或 CFUN 写入失败: "+firstNonEmpty(errString(rebootErr), rebootResponse))
		return
	}
	a.markUSBATExecutionDetached(rebootExecution, "first-use module setup reboot")
	// This exact QDC507 took a little over three minutes to return after the
	// legacy UAC reboot. Use the measured bound here as well as in the reversible
	// experiment; otherwise production adoption would roll back a valid profile
	// before macOS can publish its USB and CoreAudio descriptors.
	for deadline := time.Now().Add(240 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if err := a.ensureUSBATAtLocation(original.USBLocationID); err != nil {
			continue
		}
		if err := a.verifyPhysicalUSBIdentity(expectedUSB, original.USBLocationID); err != nil {
			continue
		}
		verified, verifyErr := a.readModuleSetupSnapshotAtLocation(original.USBLocationID)
		if verifyErr != nil || verified.USB.command() != expectedUSB.command() || verified.USBNetMode != original.USBNetMode ||
			verified.IMSConfiguration != 1 || verified.IMSVoLTECapability != 1 ||
			!sameModuleSetupIdentity(original, verified) {
			continue
		}
		volteResponse, volteErr := a.runATCommand(`AT+QCFG="volte_disable"`, 4*time.Second)
		if volteErr != nil || !volteIsEnabled(volteResponse) {
			continue
		}
		a.setModuleSetup(moduleSetupStatus{State: "verifying", Summary: "正在验证 UAC 与 QPCMV 控制面", BackupPath: backupPath})
		if err := validateDirectUACUSB(original.USBLocationID); err != nil {
			a.rollbackModuleSetup(original, backupPath, "UAC USB 描述符未形成唯一的全双工音频布局: "+err.Error())
			return
		}
		if err := validateDirectUACCoreAudio(original.USBLocationID); err != nil {
			a.rollbackModuleSetup(original, backupPath, "CoreAudio 未发布同一物理模块的 8 kHz 全双工 UAC 设备: "+err.Error())
			return
		}
		if err := a.preflightDirectQPCMVExpected(original); err != nil {
			a.rollbackModuleSetup(original, backupPath, "QPCMV/UAC 控制面预检未通过: "+err.Error())
			return
		}
		a.setModuleSetup(moduleSetupStatus{
			State: "ready", Summary: "模块初始化完成；实际声音需在通话中验证", BackupPath: backupPath,
		})
		return
	}
	a.rollbackModuleSetup(original, backupPath, "UAC 模块未在 240 秒内稳定重新枚举")
}

func (a *app) moduleSetupCallIsIdle() bool {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	return a.activeCall == nil && !a.callMediaEligible
}

func atQuerySucceeded(response string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "OK") {
			return true
		}
	}
	return false
}

func moduleRebootAccepted(response string, attempted bool, err error) bool {
	if !attempted {
		return false
	}
	if err == nil {
		return atQuerySucceeded(response) && !atResponseIsError(response)
	}
	upper := strings.ToUpper(err.Error())
	return strings.Contains(upper, "NO_DEVICE") || strings.Contains(upper, "NOT_FOUND")
}

func usbPhysicalIdentityMatchesComposition(identity usbATPhysicalIdentity, expected usbComposition, locationID uint32) bool {
	return locationID != 0 && identity.Location == locationID &&
		identity.VendorID == expected.VendorID && identity.ProductID == expected.ProductID
}

func (a *app) verifyPhysicalUSBIdentity(expected usbComposition, locationID uint32) error {
	if err := a.ensureUSBATAtLocation(locationID); err != nil {
		return err
	}
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	if a.usbAT == nil {
		return errors.New("USB AT device is not open")
	}
	identity := a.usbAT.PhysicalIdentity()
	if !usbPhysicalIdentityMatchesComposition(identity, expected, locationID) {
		return fmt.Errorf("实际 USB 身份 %04x:%04x location=0x%08x 与配置 %04x:%04x 不一致",
			identity.VendorID, identity.ProductID, identity.Location, expected.VendorID, expected.ProductID)
	}
	return nil
}

func (a *app) readModuleSetupSnapshotAtLocation(locationID uint32) (moduleSetupSnapshot, error) {
	if locationID == 0 {
		return moduleSetupSnapshot{}, errors.New("模块快照缺少 USB location")
	}
	var snapshot moduleSetupSnapshot
	var firstIdentityHash string
	err := a.withExclusiveUSBATCommandsAtLocation(locationID, func(command usbATCommandFunc) error {
		query := func(at, label string) (string, error) {
			response, err := command(at, 5*time.Second)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", label, err)
			}
			if atResponseIsError(response) {
				return "", fmt.Errorf("read %s: modem rejected query", label)
			}
			return response, nil
		}

		identityResponse, err := query("AT+CGSN", "module identity before snapshot")
		if err != nil {
			return err
		}
		imei, err := parseModuleIMEI(identityResponse)
		if err != nil {
			return err
		}
		firstIdentityHash = moduleIdentityDigest(imei)

		usbResponse, err := query(`AT+QCFG="USBCFG"`, "USBCFG")
		if err != nil {
			return err
		}
		snapshot.USB, err = parseUSBComposition(usbResponse)
		if err != nil {
			return err
		}

		usbNetResponse, err := query(`AT+QCFG="usbnet"`, "usbnet")
		if err != nil {
			return err
		}
		snapshot.USBNetMode, err = parseUSBNetModeValue(usbNetResponse)
		if err != nil || (snapshot.USBNetMode != 0 && snapshot.USBNetMode != 1) {
			return errors.New("usbnet 必须是可回滚的 0 或 1")
		}

		imsResponse, err := query(`AT+QCFG="ims"`, "IMS")
		if err != nil {
			return err
		}
		snapshot.IMSConfiguration, snapshot.IMSVoLTECapability, err = parseIMSConfiguration(imsResponse)
		if err != nil {
			return err
		}

		identityResponse, err = query("AT+CGSN", "module identity after snapshot")
		if err != nil {
			return err
		}
		imei, err = parseModuleIMEI(identityResponse)
		if err != nil {
			return err
		}
		if finalHash := moduleIdentityDigest(imei); finalHash != firstIdentityHash {
			return errors.New("读取配置期间模块身份发生变化")
		}
		return nil
	})
	if err != nil {
		return moduleSetupSnapshot{}, err
	}
	snapshot.USBLocationID = locationID
	snapshot.DeviceIdentityHash = firstIdentityHash
	return snapshot, nil
}

func parseModuleIMEI(response string) (string, error) {
	var imei string
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		match := moduleIMEILinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 2 {
			continue
		}
		if imei != "" && imei != match[1] {
			return "", errors.New("模块返回了多个不同的设备身份")
		}
		imei = match[1]
	}
	if imei == "" {
		return "", errors.New("模块没有返回严格的 15 位 IMEI")
	}
	return imei, nil
}

func moduleIdentityDigest(imei string) string {
	digest := sha256.Sum256([]byte(imei))
	return fmt.Sprintf("%x", digest[:])
}

func (a *app) readModuleIdentity() (uint32, string, error) {
	device := a.currentUSBATSnapshot()
	if device == nil {
		return 0, "", errors.New("USB AT 连接不存在")
	}
	locationID := device.LocationID()
	if locationID == 0 {
		return 0, "", errors.New("无法读取模块 USB location")
	}
	response, err := device.Command("AT+CGSN", 5*time.Second)
	if err != nil {
		a.resetUSBATDeviceIfGone(device, err)
		return 0, "", fmt.Errorf("读取模块身份失败: %w", err)
	}
	imei, err := parseModuleIMEI(response)
	if err != nil {
		return 0, "", err
	}
	if !a.isCurrentUSBAT(device, locationID) {
		return 0, "", errors.New("读取身份期间 USB 模块已变更")
	}
	return locationID, moduleIdentityDigest(imei), nil
}

func sameModuleSetupIdentity(expected, actual moduleSetupSnapshot) bool {
	return expected.USBLocationID != 0 && expected.USBLocationID == actual.USBLocationID &&
		expected.DeviceIdentityHash != "" && expected.DeviceIdentityHash == actual.DeviceIdentityHash
}

func (a *app) verifyModuleSetupIdentity(expected moduleSetupSnapshot) error {
	if expected.USBLocationID == 0 || expected.DeviceIdentityHash == "" {
		return errors.New("原始模块身份快照不完整")
	}
	if err := a.ensureUSBATAtLocation(expected.USBLocationID); err != nil {
		return err
	}
	locationID, identityHash, err := a.readModuleIdentity()
	if err != nil {
		return err
	}
	if locationID != expected.USBLocationID || identityHash != expected.DeviceIdentityHash {
		return errors.New("当前设备与初始化快照不同，拒绝写入或回滚")
	}
	return nil
}

func (a *app) runVerifiedModuleSetupCommand(expected moduleSetupSnapshot, atCommand string, timeout time.Duration) (response string, attempted bool, err error) {
	response, attempted, _, err = a.runVerifiedModuleSetupCommandExact(expected, atCommand, timeout)
	return response, attempted, err
}

func (a *app) runVerifiedModuleSetupCommandExact(expected moduleSetupSnapshot, atCommand string, timeout time.Duration) (response string, attempted bool, execution usbATExecution, err error) {
	if !a.moduleSetupCallIsIdle() {
		return "", false, usbATExecution{}, errors.New("检测到语音通话，未执行模块配置写入")
	}
	if expected.USBLocationID == 0 || expected.DeviceIdentityHash == "" {
		return "", false, usbATExecution{}, errors.New("原始模块身份快照不完整")
	}
	execution, err = a.withExclusiveUSBATCommandsAtLocationExact(expected.USBLocationID, func(command usbATCommandFunc) error {
		identityResponse, commandErr := command("AT+CGSN", 5*time.Second)
		if commandErr != nil {
			return fmt.Errorf("写入前读取模块身份: %w", commandErr)
		}
		imei, parseErr := parseModuleIMEI(identityResponse)
		if parseErr != nil {
			return parseErr
		}
		if moduleIdentityDigest(imei) != expected.DeviceIdentityHash {
			return errors.New("当前设备与初始化快照不同，拒绝写入或回滚")
		}

		callResponse, commandErr := command("AT+CLCC", 5*time.Second)
		if commandErr != nil {
			return fmt.Errorf("写入前查询实时通话: %w", commandErr)
		}
		if err := validateCLCCVoiceIdleForModuleSetup(callResponse); err != nil {
			return errors.New("无法确认模块当前没有语音通话")
		}
		if !a.moduleSetupCallIsIdle() {
			return errors.New("通话状态在写入前发生变化")
		}

		attempted = true
		response, commandErr = command(atCommand, timeout)
		return commandErr
	})
	return response, attempted, execution, err
}

// runFreshNoCallModuleCommand is used by explicit one-off module mutations
// outside the setup transaction. It binds one current USB handle, performs a
// fresh CLCC query, and only then sends the mutation while the handle is still
// exclusively locked.
func (a *app) runFreshNoCallModuleCommand(atCommand string, timeout time.Duration) (response string, attempted bool, err error) {
	response, attempted, _, err = a.runFreshNoCallModuleCommandExact(atCommand, timeout)
	return response, attempted, err
}

func (a *app) runFreshNoCallModuleCommandExact(atCommand string, timeout time.Duration) (response string, attempted bool, execution usbATExecution, err error) {
	if !a.moduleSetupCallIsIdle() {
		return "", false, usbATExecution{}, errors.New("检测到语音通话，未执行模块操作")
	}
	if a.demo || a.modem != nil {
		callResponse, callErr := a.runATCommand("AT+CLCC", 5*time.Second)
		if callErr != nil || !atQuerySucceeded(callResponse) || len(parseCLCC(callResponse)) != 0 {
			return "", false, usbATExecution{}, errors.New("无法确认模块当前没有语音通话")
		}
		attempted = true
		response, err = a.runATCommand(atCommand, timeout)
		return response, attempted, usbATExecution{}, err
	}

	locationID, err := a.claimCurrentUSBATLocation()
	if err != nil {
		return "", false, usbATExecution{}, err
	}
	defer a.releaseUSBATLocation(locationID)
	execution, err = a.withExclusiveUSBATCommandsAtLocationExact(locationID, func(command usbATCommandFunc) error {
		callResponse, commandErr := command("AT+CLCC", 5*time.Second)
		if commandErr != nil {
			return fmt.Errorf("模块操作前查询实时通话: %w", commandErr)
		}
		if !atQuerySucceeded(callResponse) || len(parseCLCC(callResponse)) != 0 || !a.moduleSetupCallIsIdle() {
			return errors.New("无法确认模块当前没有语音通话")
		}
		attempted = true
		response, commandErr = command(atCommand, timeout)
		return commandErr
	})
	return response, attempted, execution, err
}

func parseUSBNetModeValue(response string) (int, error) {
	value := parseUSBNetMode(response)
	if value == "" {
		return 0, errors.New("模块没有返回可识别的 usbnet 状态")
	}
	mode, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse usbnet mode: %w", err)
	}
	return mode, nil
}

func volteIsEnabled(response string) bool {
	compact := strings.ReplaceAll(strings.ToLower(response), " ", "")
	return strings.Contains(compact, `"volte_disable",0`) || strings.Contains(compact, `"volte/disable",0`)
}

func parseIMSConfiguration(response string) (configuration int, volteCapability int, err error) {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), `+qcfg: "ims",`) {
			continue
		}
		comma := strings.Index(line, ",")
		if comma < 0 {
			break
		}
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) < 2 {
			break
		}
		configuration, err = strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("parse IMS configuration: %w", err)
		}
		volteCapability, err = strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("parse VoLTE capability: %w", err)
		}
		return configuration, volteCapability, nil
	}
	return 0, 0, errors.New("模块没有返回可识别的 IMS 状态")
}

func (a *app) rollbackModuleSetup(original moduleSetupSnapshot, backupPath, reason string) {
	if !a.moduleSetupCallIsIdle() {
		a.setModuleSetup(moduleSetupStatus{
			State: "failed", Summary: "检测到通话，已拒绝在通话中自动回滚",
			Detail: reason + "；挂断后根据备份手动处理", BackupPath: backupPath,
		})
		return
	}
	a.setModuleSetup(moduleSetupStatus{State: "verifying", Summary: "初始化未完成，正在恢复原始模块配置", Detail: reason, BackupPath: backupPath})
	commands := []string{
		fmt.Sprintf(`AT+QCFG="ims",%d`, original.IMSConfiguration),
		fmt.Sprintf(`AT+QCFG="usbnet",%d`, original.USBNetMode),
		original.USB.command(),
	}
	for _, command := range commands {
		response, attempted, err := a.runVerifiedModuleSetupCommand(original, command, 8*time.Second)
		if err != nil || atResponseIsError(response) {
			summary := "初始化未完成，自动回滚失败；不要重启或拔出模块"
			if !attempted || (err != nil && strings.Contains(err.Error(), "当前设备")) {
				summary = "当前模块身份或通话状态不允许写入，已拒绝自动回滚"
			}
			a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: summary, Detail: command + ": " + firstNonEmpty(errString(err), response), BackupPath: backupPath})
			return
		}
	}
	readBack, err := a.readModuleSetupSnapshotAtLocation(original.USBLocationID)
	if err != nil || readBack.USB.command() != original.USB.command() ||
		readBack.USBNetMode != original.USBNetMode || readBack.IMSConfiguration != original.IMSConfiguration ||
		!sameModuleSetupIdentity(original, readBack) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "初始化未完成，回滚回读失败；不要重启或拔出模块", Detail: errString(err), BackupPath: backupPath})
		return
	}
	if err := a.verifyModuleSetupIdentity(original); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "回滚后模块身份不符，未执行重启", Detail: err.Error(), BackupPath: backupPath})
		return
	}
	rebootResponse, rebootAttempted, rebootExecution, rebootErr := a.runVerifiedModuleSetupCommandExact(original, "AT+CFUN=1,1", 4*time.Second)
	if !moduleRebootAccepted(rebootResponse, rebootAttempted, rebootErr) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "原始配置已写回，但安全重启未执行", Detail: firstNonEmpty(errString(rebootErr), rebootResponse), BackupPath: backupPath})
		return
	}
	a.markUSBATExecutionDetached(rebootExecution, "module setup automatic rollback reboot")
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if err := a.ensureUSBATAtLocation(original.USBLocationID); err != nil {
			continue
		}
		if err := a.verifyPhysicalUSBIdentity(original.USB, original.USBLocationID); err != nil {
			continue
		}
		verified, verifyErr := a.readModuleSetupSnapshotAtLocation(original.USBLocationID)
		if verifyErr == nil && verified.USB.command() == original.USB.command() &&
			verified.USBNetMode == original.USBNetMode && verified.IMSConfiguration == original.IMSConfiguration &&
			sameModuleSetupIdentity(original, verified) {
			a.setModuleSetup(moduleSetupStatus{State: "rolled_back", Summary: "初始化未验证，已恢复并重启到原始模块配置", Detail: reason, BackupPath: backupPath})
			return
		}
	}
	a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "原始配置已写回，但重启后的回读未确认", Detail: reason, BackupPath: backupPath})
}

func saveModuleSetupBackup(snapshot moduleSetupSnapshot) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Application Support", "MacCellular", "module-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	payload := struct {
		SavedAt  string              `json:"saved_at"`
		Snapshot moduleSetupSnapshot `json:"snapshot"`
	}{SavedAt: time.Now().Format(time.RFC3339), Snapshot: snapshot}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "module-before-v1.3.0-"+time.Now().Format("20060102-150405")+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	return path, file.Close()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "未知错误"
}
