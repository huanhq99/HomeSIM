//go:build darwin && cgo

package main

import (
	"crypto/sha256"
	"encoding/hex"
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

// Legacy external-runtime support is retained only as quarantined compatibility
// code. Current entry points never load, upload, or execute these device-side
// artifacts. The direct QPCMV/UAC route is also default-off research code after
// the controlled target-composition validation returned an explicit error.
const (
	trustedVoiceRuntimeVersion = "qdc507-3.18.44-voice-20260712.5"
	trustedVoiceKernelRelease  = "3.18.44"
	trustedVoiceCardName       = "mdm9607-tomtom-i2s-snd-card"
	trustedVoiceHelperName     = "mavo-pcm-bridge.armv7"
	voiceRemoteDirectory       = "/run/maccellular-call"
	voiceRoutePIDFile          = "/run/maccellular-voice-route.pid"
	voiceRouteLogFile          = "/run/maccellular-voice-route.log"
	voiceCalibrationPIDFile    = "/run/maccellular-alsaucm.pid"
	voiceCalibrationLogFile    = "/run/maccellular-alsaucm.log"
)

var trustedVoiceRuntimeHashes = map[string]string{
	"COPYING-GPL-2.0":       "af8067302947c01fd9eee72befa54c7e3ef8a48fecde7fd71277f2290b2bf0f7",
	"MODULE-REPORT.md":      "fb9d58336bcfdad8938d7833c113a815c2153d9a04564eb73cddabea737f8be2",
	"manifest.json":         "f4f6c266ced7015d4e61d993a6e31247c26a9e85a8fdf1c6d842c459e1e2970a",
	"mavo-pcm-bridge.armv7": "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc",
	"qdc507_aprv3.ko":       "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a",
	"qdc507_voice.ko":       "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c",
}

var trustedVoiceFileModes = map[string]uint32{
	"qdc507_aprv3.ko":      0o644,
	"qdc507_voice.ko":      0o644,
	trustedVoiceHelperName: 0o755,
}

var trustedVoiceModules = map[string]string{
	"qdc507_aprv3.ko": "qdc507_aprv3",
	"qdc507_voice.ko": "qdc507_voice",
}

var trustedVoiceRequiredDevices = map[string]bool{
	"/dev/snd/controlC0": true,
	"/dev/snd/pcmC0D4p":  true,
	"/dev/snd/pcmC0D4c":  true,
	"/dev/snd/pcmC0D5p":  true,
	"/dev/snd/pcmC0D6c":  true,
}

type voiceRuntimeManifest struct {
	FormatVersion  int    `json:"formatVersion"`
	RuntimeVersion string `json:"runtimeVersion"`
	KernelRelease  string `json:"kernelRelease"`
	CardName       string `json:"cardName"`
	Helper         string `json:"helper"`
	Files          []struct {
		Name string `json:"name"`
		Mode uint32 `json:"mode"`
	} `json:"files"`
	Modules []struct {
		File string `json:"file"`
		Name string `json:"name"`
	} `json:"modules"`
	RequiredDevices []string `json:"requiredDevices"`
}

type externalVoiceRuntime struct {
	manifest voiceRuntimeManifest
	files    map[string][]byte
}

type voiceADBSession struct {
	client     *adbClient
	locationID uint32
}

type voiceRouteSession interface {
	shell(command string, timeout time.Duration) (string, int, error)
	query(command string, timeout time.Duration) (string, int, error)
}

func (s *voiceADBSession) close() {
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *voiceADBSession) reset() { s.close() }

func (s *voiceADBSession) ensure() error {
	if s.client != nil && s.client.isOpen() {
		return nil
	}
	client, err := openDJIUSBADB(s.locationID)
	if err != nil {
		return err
	}
	s.client = client
	return nil
}

func (s *voiceADBSession) shell(command string, timeout time.Duration) (string, int, error) {
	if err := s.ensure(); err != nil {
		return "", 0, err
	}
	output, status, err := s.client.shellChecked(command, timeout)
	if err != nil {
		s.reset()
	}
	return output, status, err
}

func (s *voiceADBSession) query(command string, timeout time.Duration) (string, int, error) {
	output, status, err := s.shell(command, timeout)
	if err == nil {
		return output, status, nil
	}
	time.Sleep(250 * time.Millisecond)
	return s.shell(command, timeout)
}

func (s *voiceADBSession) push(data []byte, remotePath string, mode uint32) error {
	if err := s.ensure(); err != nil {
		return err
	}
	if err := s.client.push(data, remotePath, mode, 30*time.Second); err == nil {
		return nil
	}
	s.reset()
	time.Sleep(250 * time.Millisecond)
	if err := s.ensure(); err != nil {
		return err
	}
	return s.client.push(data, remotePath, mode, 30*time.Second)
}

func hashVoiceRuntimeBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func safeVoiceFileName(value string) bool {
	return value != "" && regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(value)
}

func outputHasExactField(output, expected string) bool {
	for _, field := range strings.Fields(output) {
		if field == expected {
			return true
		}
	}
	return false
}

func (a *app) currentDJIVoiceLocation() (uint32, error) {
	locationID, ok := a.currentUSBATLocation()
	if !ok {
		return 0, errors.New("模块 USB AT 尚未连接")
	}
	return locationID, nil
}

func moduleVoiceUSBIdentity(device *usbAT) (uint16, uint16, bool) {
	identity := device.PhysicalIdentity()
	if identity.VendorID <= 0 || identity.ProductID <= 0 {
		return 0, 0, false
	}
	return uint16(identity.VendorID), uint16(identity.ProductID), true
}

func loadExternalVoiceRuntime(dir string) (*externalVoiceRuntime, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || dir == "" {
		return nil, errors.New("外置通话运行时路径为空")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("读取通话运行时目录: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("通话运行时目录必须是仅当前用户可写的真实目录")
	}
	files := make(map[string][]byte, len(trustedVoiceRuntimeHashes))
	for name, expected := range trustedVoiceRuntimeHashes {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("通话运行时缺少 %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("通话运行时 %s 不是普通文件", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取通话运行时 %s: %w", name, err)
		}
		if hashVoiceRuntimeBytes(data) != expected {
			return nil, fmt.Errorf("通话运行时 %s 校验失败", name)
		}
		files[name] = data
	}
	var manifest voiceRuntimeManifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		return nil, fmt.Errorf("解析通话运行时清单: %w", err)
	}
	if err := validateVoiceRuntimeManifest(manifest); err != nil {
		return nil, err
	}
	return &externalVoiceRuntime{manifest: manifest, files: files}, nil
}

func validateVoiceRuntimeManifest(manifest voiceRuntimeManifest) error {
	if manifest.FormatVersion != 1 || manifest.RuntimeVersion != trustedVoiceRuntimeVersion ||
		manifest.KernelRelease != trustedVoiceKernelRelease || manifest.CardName != trustedVoiceCardName ||
		manifest.Helper != trustedVoiceHelperName || len(manifest.Files) != len(trustedVoiceFileModes) ||
		len(manifest.Modules) != len(trustedVoiceModules) ||
		len(manifest.RequiredDevices) != len(trustedVoiceRequiredDevices) {
		return errors.New("通话运行时清单与固定版本不匹配")
	}
	seenFiles := make(map[string]bool, len(manifest.Files))
	for _, entry := range manifest.Files {
		expectedMode, trusted := trustedVoiceFileModes[entry.Name]
		if !safeVoiceFileName(entry.Name) || !trusted || entry.Mode != expectedMode || seenFiles[entry.Name] {
			return errors.New("通话运行时包含未验证的文件或权限")
		}
		seenFiles[entry.Name] = true
	}
	seenModules := make(map[string]bool, len(manifest.Modules))
	for _, module := range manifest.Modules {
		expectedName, trusted := trustedVoiceModules[module.File]
		if !safeVoiceFileName(module.File) || !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(module.Name) ||
			!trusted || module.Name != expectedName || seenModules[module.File] {
			return errors.New("通话运行时包含未验证的模块")
		}
		seenModules[module.File] = true
	}
	seenDevices := make(map[string]bool, len(manifest.RequiredDevices))
	for _, device := range manifest.RequiredDevices {
		if !trustedVoiceRequiredDevices[device] || seenDevices[device] {
			return errors.New("通话运行时包含未验证的设备路径")
		}
		seenDevices[device] = true
	}
	return nil
}

func locateExternalVoiceRuntime() (*externalVoiceRuntime, error) {
	var candidates []string
	if override := strings.TrimSpace(os.Getenv("MACCELLULAR_DJI_VOICE_RUNTIME")); override != "" {
		runtime, err := loadExternalVoiceRuntime(override)
		if err != nil {
			return nil, fmt.Errorf("MACCELLULAR_DJI_VOICE_RUNTIME 无效: %w", err)
		}
		return runtime, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "MacCellular Runtime", "ModuleVoice"))
	}
	if working, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(working, "local", "ModuleVoice"))
	}
	var failures []string
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, "manifest.json")); err != nil {
			continue
		}
		runtime, err := loadExternalVoiceRuntime(candidate)
		if err == nil {
			return runtime, nil
		}
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		return nil, errors.New(strings.Join(failures, "; "))
	}
	return nil, errors.New("旧版外置通话运行时在 MacCellular 版本中已停用")
}

func voiceSoundDeviceChecks(manifest voiceRuntimeManifest) string {
	checks := make([]string, 0, len(manifest.RequiredDevices)+1)
	for _, device := range manifest.RequiredDevices {
		checks = append(checks, "test -c '"+device+"'")
	}
	checks = append(checks, "grep -Fq '"+manifest.CardName+"' /proc/asound/cards")
	return strings.Join(checks, " && ")
}

const (
	// These markers are valid no-op shell assignments. Do not use a leading
	// '#': every generated command appends its body on the same line, so a
	// comment marker would silently comment out the entire safety operation.
	voiceHelperScanMarker      = "maccellular_voice_helper_phase=scan"
	voiceHelperReadyMarker     = "maccellular_voice_helper_phase=ready"
	voiceHelperTerminateMarker = "maccellular_voice_helper_phase=terminate"
	voiceHelperLaunchMarker    = "maccellular_voice_helper_phase=launch"
	voiceHelperCleanupMarker   = "maccellular_voice_helper_phase=cleanup"
)

type voiceHelperProcess struct {
	PID       uint64
	StartTime uint64
}

func quoteVoiceShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// voiceHelperProcessLoop enumerates every process instead of trusting the
// pidfile. A match requires both the exact helper argv[0] and a standalone
// --voice-route-session argument. A still-existing but unreadable candidate is
// an error, so callers fail closed rather than assuming the helper is absent.
func voiceHelperProcessLoop(helper, matchBody string) string {
	return "expected_helper=" + quoteVoiceShell(helper) + "; " +
		"for proc in /proc/[0-9]*; do " +
		"test -d \"$proc\" || continue; " +
		"cmdline=$(tr '\\000' '\\n' < \"$proc/cmdline\" 2>/dev/null); read_status=$?; " +
		"if test \"$read_status\" -ne 0; then test ! -d \"$proc\" && continue; exit 70; fi; " +
		"argv0=$(printf '%s\\n' \"$cmdline\" | sed -n '1p'); " +
		"test \"$argv0\" = \"$expected_helper\" || continue; " +
		"if printf '%s\\n' \"$cmdline\" | grep -q '^--voice-route-session$'; then :; " +
		"else flag_status=$?; test \"$flag_status\" -eq 1 && continue; exit 71; fi; " +
		"pid=${proc##*/}; starttime=$(cut -d ' ' -f 22 \"$proc/stat\" 2>/dev/null); stat_status=$?; " +
		"if test \"$stat_status\" -ne 0; then test ! -d \"$proc\" && continue; exit 72; fi; " +
		"case \"$pid:$starttime\" in :*|*:|*[!0-9:]*) exit 73;; esac; " +
		matchBody + "; done"
}

func voiceHelperProcessScanCommand(helper string) string {
	return voiceHelperScanMarker + "; " + voiceHelperProcessLoop(helper, "printf '%s %s\\n' \"$pid\" \"$starttime\"")
}

func parseVoiceHelperProcesses(output string) ([]voiceHelperProcess, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	processes := make([]voiceHelperProcess, 0, len(lines))
	seen := make(map[uint64]bool, len(lines))
	statusMarker := regexp.MustCompile(`^__MAVO_STATUS_[0-9a-f]+_[0-9]+__$`)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || statusMarker.MatchString(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.New("模块 helper 进程扫描结果格式无效")
		}
		pid, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || pid == 0 {
			return nil, errors.New("模块 helper 进程 PID 无效")
		}
		startTime, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || startTime == 0 {
			return nil, errors.New("模块 helper 进程启动时间无效")
		}
		if seen[pid] {
			return nil, errors.New("模块 helper 进程扫描结果包含重复 PID")
		}
		seen[pid] = true
		processes = append(processes, voiceHelperProcess{PID: pid, StartTime: startTime})
	}
	return processes, nil
}

func scanVoiceHelperProcesses(session voiceRouteSession, helper string) ([]voiceHelperProcess, error) {
	output, status, err := session.query(voiceHelperProcessScanCommand(helper), 8*time.Second)
	if err != nil {
		return nil, fmt.Errorf("扫描模块 D4 helper: %w", err)
	}
	if status != 0 {
		return nil, fmt.Errorf("扫描模块 D4 helper 失败: status=%d output=%s", status, strings.TrimSpace(output))
	}
	return parseVoiceHelperProcesses(output)
}

func voiceHelperOwnedFunction(helper string) string {
	return "is_voice_helper() { candidate_pid=$1; candidate_start=$2; " +
		"case \"$candidate_pid:$candidate_start\" in :*|*:|*[!0-9:]*) return 2;; esac; " +
		"candidate_proc=/proc/$candidate_pid; " +
		"current_start=$(cut -d ' ' -f 22 \"$candidate_proc/stat\" 2>/dev/null) || return 1; " +
		"test \"$current_start\" = \"$candidate_start\" || return 1; " +
		"candidate_cmdline=$(tr '\\000' '\\n' < \"$candidate_proc/cmdline\" 2>/dev/null) || return 2; " +
		"candidate_argv0=$(printf '%s\\n' \"$candidate_cmdline\" | sed -n '1p'); " +
		"test \"$candidate_argv0\" = " + quoteVoiceShell(helper) + " || return 1; " +
		"printf '%s\\n' \"$candidate_cmdline\" | grep -q '^--voice-route-session$'; }; "
}

func voiceRouteReadyCommand(runtime *externalVoiceRuntime) string {
	helper := voiceRemoteDirectory + "/" + runtime.manifest.Helper
	findOne := "count=$((count+1)); found_pid=$pid; found_start=$starttime"
	return voiceHelperReadyMarker + "; count=0; found_pid=; found_start=; " +
		voiceHelperProcessLoop(helper, findOne) + "; " +
		"test \"$count\" -eq 1 || exit 1; " +
		"test -f '" + voiceRoutePIDFile + "' && test ! -L '" + voiceRoutePIDFile + "' || exit 1; " +
		"read pid expected_start extra < '" + voiceRoutePIDFile + "' || exit 1; " +
		"test -z \"${extra:-}\" || exit 1; " +
		"case \"$pid:$expected_start\" in :*|*:|*[!0-9:]*) exit 1;; esac; " +
		"test \"$pid\" = \"$found_pid\" && test \"$expected_start\" = \"$found_start\" || exit 1; " +
		voiceHelperOwnedFunction(helper) +
		"is_voice_helper \"$pid\" \"$expected_start\" || exit 1; " +
		"grep -q 'VoLTE route session active on hw:0,4' '" + voiceRouteLogFile + "' && " +
		"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 1 && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status"
}

func voiceRouteReadyChecked(session voiceRouteSession, runtime *externalVoiceRuntime) (bool, error) {
	output, status, err := session.query(voiceRouteReadyCommand(runtime), 8*time.Second)
	if err != nil {
		return false, fmt.Errorf("核对模块语音路由: %w", err)
	}
	switch status {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("核对模块语音路由失败: status=%d output=%s", status, strings.TrimSpace(output))
	}
}

func prepareExternalVoiceRuntime(session *voiceADBSession, runtime *externalVoiceRuntime) error {
	output, status, err := session.query("id -u", 8*time.Second)
	if err != nil || status != 0 || !outputHasExactField(output, "0") {
		return errors.New("模块 ADB 没有可用的 root 控制通道")
	}
	output, status, err = session.query("uname -r", 8*time.Second)
	if err != nil || status != 0 || !outputHasExactField(output, runtime.manifest.KernelRelease) {
		return fmt.Errorf("模块内核不匹配；需要 %s，实际为 %s", runtime.manifest.KernelRelease, strings.TrimSpace(output))
	}
	remoteDirectoryCommand := "if test -L '" + voiceRemoteDirectory + "'; then exit 64; fi; " +
		"if test -e '" + voiceRemoteDirectory + "' && test ! -d '" + voiceRemoteDirectory + "'; then exit 65; fi; " +
		"mkdir -p '" + voiceRemoteDirectory + "' && chmod 700 '" + voiceRemoteDirectory + "' && " +
		"test \"$(stat -c %u '" + voiceRemoteDirectory + "')\" = 0 && " +
		"test \"$(stat -c %a '" + voiceRemoteDirectory + "')\" = 700"
	if output, status, err = session.shell(remoteDirectoryCommand, 8*time.Second); err != nil || status != 0 {
		return fmt.Errorf("创建模块临时目录失败: %s %v", strings.TrimSpace(output), err)
	}
	for _, entry := range runtime.manifest.Files {
		data, ok := runtime.files[entry.Name]
		if !ok {
			return fmt.Errorf("已验证的通话运行时缺少 %s", entry.Name)
		}
		if err := session.push(data, voiceRemoteDirectory+"/"+entry.Name, 0o100000|entry.Mode); err != nil {
			return fmt.Errorf("传输 %s: %w", entry.Name, err)
		}
	}
	checks := voiceSoundDeviceChecks(runtime.manifest)
	_, soundStatus, soundErr := session.query(checks, 8*time.Second)
	if soundErr != nil {
		return fmt.Errorf("核对模块音频设备失败: %w", soundErr)
	}
	if soundStatus != 0 {
		_, legacyStatus, legacyErr := session.query("grep -q '^qdc507_afe ' /proc/modules", 8*time.Second)
		if legacyErr != nil || (legacyStatus != 0 && legacyStatus != 1) {
			return errors.New("无法核对模块中现有的音频驱动")
		}
		if legacyStatus == 0 {
			return errors.New("检测到旧版 qdc507_afe 驱动；为避免热切换，请先重启模块")
		}
		for _, module := range runtime.manifest.Modules {
			_, present, probeErr := session.query("grep -q '^"+module.Name+" ' /proc/modules", 8*time.Second)
			if probeErr != nil || (present != 0 && present != 1) {
				if probeErr == nil {
					probeErr = fmt.Errorf("unexpected module probe status %d", present)
				}
				return probeErr
			}
			if present == 0 {
				continue
			}
			command := "insmod '" + voiceRemoteDirectory + "/" + module.File + "'"
			moduleOutput, moduleStatus, moduleErr := session.shell(command, 20*time.Second)
			if moduleErr != nil || moduleStatus != 0 {
				// A lost shell reply is ambiguous. Read back /proc/modules before
				// reporting failure; never issue a duplicate insmod blindly.
				_, present, probeErr = session.query("grep -q '^"+module.Name+" ' /proc/modules", 8*time.Second)
				if probeErr != nil || present != 0 {
					return fmt.Errorf("加载 %s 失败: %s %v", module.Name, strings.TrimSpace(moduleOutput), moduleErr)
				}
			}
		}
		ready := false
		for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			_, status, queryErr := session.query(checks, 8*time.Second)
			if queryErr == nil && status == 0 {
				ready = true
				break
			}
		}
		if !ready {
			return errors.New("模块音频驱动已加载，但 ALSA 设备未就绪")
		}
	}
	calibrationCommand := "owned=0; " +
		"if test -s '" + voiceCalibrationPIDFile + "'; then read pid expected_start < '" + voiceCalibrationPIDFile + "' || true; " +
		"current_start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); argv0=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"test \"$current_start\" = \"$expected_start\" && test \"$argv0\" = /usr/bin/alsaucm_test && owned=1 || true; fi; " +
		"if test \"$owned\" -eq 0; then for proc in /proc/[0-9]*; do test -r \"$proc/cmdline\" || continue; " +
		"argv0=$(tr '\\000' '\\n' < \"$proc/cmdline\" 2>/dev/null | sed -n '1p'); test \"$argv0\" = /usr/bin/alsaucm_test || continue; " +
		"oldpid=${proc##*/}; kill -TERM \"$oldpid\" 2>/dev/null || true; n=0; while kill -0 \"$oldpid\" 2>/dev/null && test \"$n\" -lt 30; do " +
		"sleep 0.1; n=$((n+1)); done; kill -0 \"$oldpid\" 2>/dev/null && exit 71 || true; done; " +
		"rm -f /run/alsaucm_test '" + voiceCalibrationPIDFile + "' '" + voiceCalibrationLogFile + "'; " +
		"nohup /usr/bin/alsaucm_test </dev/null >> '" + voiceCalibrationLogFile + "' 2>&1 & pid=$!; " +
		"starttime=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); printf '%s %s\\n' \"$pid\" \"$starttime\" > '" + voiceCalibrationPIDFile + "'; " +
		"n=0; while test \"$n\" -lt 50 && test ! -p /run/alsaucm_test; do kill -0 \"$pid\" 2>/dev/null || exit 72; sleep 0.1; n=$((n+1)); done; test -p /run/alsaucm_test || exit 73; fi; " +
		"if ! grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLogFile + "' 2>/dev/null; then " +
		"printf 'open snd_soc_msm_9x07_Tomtom_I2S\\n' > /run/alsaucm_test; printf 'set _verb VoLTE\\n' > /run/alsaucm_test; " +
		"printf 'set _enadev Auxpcm Rx\\n' > /run/alsaucm_test; printf 'set _enadev Auxpcm Tx\\n' > /run/alsaucm_test; " +
		"n=0; while test \"$n\" -lt 100; do grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLogFile + "' 2>/dev/null && break; sleep 0.1; n=$((n+1)); done; fi; " +
		"grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLogFile + "'"
	calibrationOutput, calibrationStatus, calibrationErr := session.shell(calibrationCommand, 25*time.Second)
	if calibrationErr != nil || calibrationStatus != 0 {
		return fmt.Errorf("模块 VoLTE 校准未就绪: %s %v", strings.TrimSpace(calibrationOutput), calibrationErr)
	}
	_, status, err = session.query("test -c /dev/ttyGS0 && test -p /run/voc_svr", 8*time.Second)
	if err != nil || status != 0 {
		return errors.New("模块缺少 ttyGS0 或 voc_svr")
	}
	helper := voiceRemoteDirectory + "/" + runtime.manifest.Helper
	output, status, err = session.query("'"+helper+"' --check", 15*time.Second)
	if err != nil || status != 0 {
		return fmt.Errorf("模块 PCM 桥自检失败: %s %v", strings.TrimSpace(output), err)
	}
	return nil
}

func voiceHelperTerminateCommand(helper string, process voiceHelperProcess) string {
	pid := strconv.FormatUint(process.PID, 10)
	startTime := strconv.FormatUint(process.StartTime, 10)
	return voiceHelperTerminateMarker + "; " + voiceHelperOwnedFunction(helper) +
		"is_voice_helper " + pid + " " + startTime + " || exit 74; " +
		"kill -TERM " + pid + " || exit 75"
}

func voiceHelperLaunchCommand(helper string) string {
	return voiceHelperLaunchMarker + "; " +
		"test ! -e '" + voiceRoutePIDFile + "' && test ! -L '" + voiceRoutePIDFile + "' || exit 76; " +
		"nohup " + quoteVoiceShell(helper) + " --voice-route-session --verbose </dev/null >> '" + voiceRouteLogFile + "' 2>&1 & pid=$!; " +
		"starttime=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null) || exit 77; " +
		"case \"$pid:$starttime\" in :*|*:|*[!0-9:]*) exit 78;; esac; " +
		"umask 077; pidtmp='" + voiceRoutePIDFile + ".tmp'; rm -f \"$pidtmp\"; " +
		"printf '%s %s\\n' \"$pid\" \"$starttime\" > \"$pidtmp\" && mv -f \"$pidtmp\" '" + voiceRoutePIDFile + "'"
}

func terminateVoiceHelperProcess(session voiceRouteSession, helper string, process voiceHelperProcess) error {
	output, status, err := session.shell(voiceHelperTerminateCommand(helper, process), 8*time.Second)
	if err != nil || status != 0 {
		return fmt.Errorf("终止旧 D4 helper 失败: %s status=%d err=%v", strings.TrimSpace(output), status, err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		processes, scanErr := scanVoiceHelperProcesses(session, helper)
		if scanErr != nil {
			return scanErr
		}
		if len(processes) == 0 {
			return nil
		}
		if len(processes) > 1 || processes[0] != process {
			return errors.New("终止旧 D4 helper 时进程集合发生变化；拒绝继续")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("旧 D4 helper 拒绝在 SIGTERM 后退出；未发送 SIGKILL")
}

func prepareVoiceHelperStart(session voiceRouteSession, helper string) error {
	processes, err := scanVoiceHelperProcesses(session, helper)
	if err != nil {
		return err
	}
	if len(processes) > 1 {
		return fmt.Errorf("检测到 %d 个 D4 helper 实例；拒绝启动", len(processes))
	}
	if len(processes) == 1 {
		if err := terminateVoiceHelperProcess(session, helper, processes[0]); err != nil {
			return err
		}
	}
	output, status, err := session.shell("rm -f '"+voiceRoutePIDFile+"' '"+voiceRouteLogFile+"'", 8*time.Second)
	if err != nil || status != 0 {
		return fmt.Errorf("清理旧 D4 helper 状态失败: %s status=%d err=%v", strings.TrimSpace(output), status, err)
	}
	processes, err = scanVoiceHelperProcesses(session, helper)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return errors.New("旧 D4 helper 未清理干净；拒绝启动")
	}
	return nil
}

func startExternalVoiceRoute(session voiceRouteSession, runtime *externalVoiceRuntime) error {
	ready, readyErr := voiceRouteReadyChecked(session, runtime)
	if readyErr != nil {
		return readyErr
	}
	if ready {
		return nil
	}
	helper := voiceRemoteDirectory + "/" + runtime.manifest.Helper
	if err := prepareVoiceHelperStart(session, helper); err != nil {
		return err
	}
	command := voiceHelperLaunchCommand(helper)
	launchOutput, launchStatus, launchErr := session.shell(command, 8*time.Second)
	// audio_enable=1 can re-enumerate USB before the shell reply. Verify the
	// owned process instead of repeating the launch command.
	for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); time.Sleep(400 * time.Millisecond) {
		ready, readyErr = voiceRouteReadyChecked(session, runtime)
		if readyErr != nil {
			return readyErr
		}
		if ready {
			return nil
		}
	}
	logOutput, _, _ := session.query("test ! -f '"+voiceRouteLogFile+"' || tail -n 120 '"+voiceRouteLogFile+"'", 8*time.Second)
	return fmt.Errorf("模块 D4/UAC 路由未就绪: %s %s status=%d err=%v", strings.TrimSpace(launchOutput), strings.TrimSpace(logOutput), launchStatus, launchErr)
}

func stopExternalVoiceRoute(session voiceRouteSession, helper string) error {
	processes, err := scanVoiceHelperProcesses(session, helper)
	if err != nil {
		return err
	}
	if len(processes) > 1 {
		return fmt.Errorf("检测到 %d 个 D4 helper 实例；拒绝自动终止", len(processes))
	}
	if len(processes) == 1 {
		if err := terminateVoiceHelperProcess(session, helper, processes[0]); err != nil {
			return err
		}
	}
	processes, err = scanVoiceHelperProcesses(session, helper)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return errors.New("D4 helper 仍在运行；拒绝回滚音频路由")
	}
	var lastDetail string
	for attempt := 0; attempt < 5; attempt++ {
		cleanup := voiceHelperCleanupMarker + "; echo 0 > /sys/class/android_usb/f_audio/audio_enable && " +
			"if test -p /run/voc_svr; then printf 'T\\n' > /run/voc_svr; printf 'T\\n' > /run/voc_svr; printf 'B\\n' > /run/voc_svr; fi; " +
			"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 0"
		output, status, cleanupErr := session.shell(cleanup, 8*time.Second)
		if cleanupErr == nil && status == 0 {
			processes, scanErr := scanVoiceHelperProcesses(session, helper)
			if scanErr != nil {
				return scanErr
			}
			if len(processes) != 0 {
				return errors.New("关闭音频路由后再次发现 D4 helper；拒绝报告成功")
			}
			output, status, cleanupErr = session.shell("rm -f '"+voiceRoutePIDFile+"'; test ! -e '"+voiceRoutePIDFile+"' && test ! -L '"+voiceRoutePIDFile+"'", 8*time.Second)
			if cleanupErr == nil && status == 0 {
				return nil
			}
		}
		lastDetail = fmt.Sprintf("%s status=%d err=%v", strings.TrimSpace(output), status, cleanupErr)
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("D4 helper 已退出，但路由回滚未确认: %s", lastDetail)
}

func (a *app) setModuleVoiceState(ready bool, phase, detail string, locationID uint32, err error) {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	a.moduleVoiceReady = ready
	a.moduleVoiceLast = time.Now()
	a.moduleVoiceDetail = detail
	a.moduleVoicePhase = phase
	a.moduleVoiceLocation = locationID
	if err != nil {
		a.moduleVoiceErr = err.Error()
	} else {
		a.moduleVoiceErr = ""
	}
}

func (a *app) kickModuleVoice() {
	if a == nil || !a.implicitUACVoice {
		return
	}
	go func() {
		// Startup diagnostics and the first SMS/call poll can briefly hold this
		// lock. Queue the warm-up behind them instead of silently losing it; the
		// work stays off the serving goroutine and rechecks that no call appeared.
		a.moduleMutationMu.Lock()
		defer a.moduleMutationMu.Unlock()
		a.callMu.RLock()
		idle := a.activeCall == nil
		a.callMu.RUnlock()
		if !idle {
			return
		}
		a.moduleVoiceOpMu.Lock()
		defer a.moduleVoiceOpMu.Unlock()
		if err := a.ensureModuleVoiceRouteLocked(); err != nil {
			log.Printf("module voice startup prewarm deferred: %v", err)
			return
		}
		log.Printf("module voice route prewarmed at phone relay startup")
	}()
}

func (a *app) kickIncomingModuleVoicePrewarm(call callRecord, generation uint64) {
	if a == nil || !a.implicitUACVoice || generation == 0 || call.ID == "" ||
		call.Direction != "incoming" || (call.State != "incoming" && call.State != "waiting") {
		return
	}
	source := remoteMediaSource{Purpose: "incoming", Call: remoteCallExpectation{
		CallID: call.ID, CallGeneration: generation, CallIndex: call.Index, CallDirection: call.Direction,
	}}
	go func() {
		if err := a.prewarmIncomingModuleVoiceRoute(source); err != nil {
			log.Printf("incoming module voice prewarm deferred: %v", err)
		}
	}()
}

// prewarmIncomingModuleVoiceRoute moves the slow D4/UAC setup ahead of ATA.
// The remote caller therefore continues to hear network ringback while the
// route starts instead of entering a connected-but-silent call.
func (a *app) prewarmIncomingModuleVoiceRoute(source remoteMediaSource) error {
	if a == nil || !a.implicitUACVoice || source.Purpose != "incoming" {
		return nil
	}
	a.callMu.RLock()
	callMatches := a.activeCall != nil && a.activeCall.ID == source.Call.CallID &&
		a.activeCall.Index == source.Call.CallIndex && a.activeCall.Direction == "incoming" &&
		(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
	a.callMu.RUnlock()
	if !callMatches {
		return errors.New("incoming call changed before module audio prewarm")
	}
	a.moduleMutationMu.Lock()
	defer a.moduleMutationMu.Unlock()
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	if err := a.refreshModuleVoiceRouteForGenerationLocked(source.Call.CallGeneration); err != nil {
		return err
	}
	if !a.remoteMediaSourceCurrent(source) {
		return errors.New("incoming call changed during module audio prewarm")
	}
	log.Printf("module voice route prewarmed before incoming answer")
	return nil
}

// prewarmOutgoingModuleVoiceRoute moves the slow D4/UAC helper startup before
// ATD. The far party therefore does not answer into several seconds of silence
// while the Mac starts the module audio route.
func (a *app) prewarmOutgoingModuleVoiceRoute(expectedGeneration uint64) error {
	if a == nil || !a.implicitUACVoice {
		return nil
	}
	if a.outgoingVoicePrewarm != nil {
		return a.outgoingVoicePrewarm(expectedGeneration)
	}
	a.callMu.RLock()
	idle := expectedGeneration != 0 && a.callTopologyKnown && a.callGeneration == expectedGeneration && a.activeCall == nil
	a.callMu.RUnlock()
	if !idle {
		return errors.New("outgoing call state changed before module audio prewarm")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	if err := a.refreshModuleVoiceRouteForGenerationLocked(expectedGeneration); err != nil {
		return err
	}
	a.callMu.RLock()
	idle = a.callTopologyKnown && a.callGeneration == expectedGeneration && a.activeCall == nil
	a.callMu.RUnlock()
	if !idle {
		return errors.New("outgoing call state changed during module audio prewarm")
	}
	log.Printf("module voice route prewarmed before outgoing dial")
	return nil
}

// refreshModuleVoiceRouteForGenerationLocked gives every new cellular call a
// fresh module-side PCM/UAC session while keeping that work before ATD/ATA.
// Repeated poll notifications for the same generation remain idempotent.
// The caller holds moduleVoiceOpMu.
func (a *app) refreshModuleVoiceRouteForGenerationLocked(generation uint64) error {
	if generation == 0 {
		return errors.New("module voice prewarm generation must be non-zero")
	}
	a.moduleVoiceMu.Lock()
	alreadyRefreshed := a.moduleVoicePrewarmGeneration == generation
	a.moduleVoiceMu.Unlock()
	if alreadyRefreshed {
		if a.moduleVoiceRefresh != nil {
			return nil
		}
		return a.ensureModuleVoiceRouteLocked()
	}
	if a.moduleVoiceRefresh != nil {
		if err := a.moduleVoiceRefresh(generation); err != nil {
			return err
		}
	} else {
		runtime, err := locateExternalVoiceRuntime()
		if err != nil {
			return err
		}
		locationID, err := a.currentDJIVoiceLocation()
		if err != nil {
			return err
		}
		session := &voiceADBSession{locationID: locationID}
		ready, err := voiceRouteReadyChecked(session, runtime)
		session.close()
		if err != nil {
			return err
		}
		if ready {
			if err := a.stopModuleVoiceRouteLocked(); err != nil {
				return fmt.Errorf("重置上一通模块音频路由: %w", err)
			}
		}
		if err := a.ensureModuleVoiceRouteLocked(); err != nil {
			return err
		}
	}
	a.moduleVoiceMu.Lock()
	a.moduleVoicePrewarmGeneration = generation
	a.moduleVoiceMu.Unlock()
	return nil
}

func (a *app) ensureModuleVoiceRoute() error {
	return errors.New("直连 UAC 路由必须在 ATD/ATA 前预先 arm；拒绝单独启动")
}

func (a *app) ensureModuleVoiceRouteLocked() error {
	runtime, err := locateExternalVoiceRuntime()
	if err != nil {
		a.setModuleVoiceState(false, "unavailable", "外置运行时不可用", 0, err)
		return err
	}
	locationID, err := a.currentDJIVoiceLocation()
	if err != nil {
		a.setModuleVoiceState(false, "unknown", runtime.manifest.RuntimeVersion, 0, err)
		return err
	}
	session := &voiceADBSession{locationID: locationID}
	defer session.close()
	ready, readyErr := voiceRouteReadyChecked(session, runtime)
	if readyErr != nil {
		a.setModuleVoiceState(false, "unknown", runtime.manifest.RuntimeVersion, locationID, readyErr)
		return readyErr
	}
	if ready {
		a.setModuleVoiceState(true, "ready", runtime.manifest.RuntimeVersion, locationID, nil)
		return nil
	}
	if err := prepareExternalVoiceRuntime(session, runtime); err != nil {
		a.setModuleVoiceState(false, "failed", runtime.manifest.RuntimeVersion, locationID, err)
		return err
	}
	if err := startExternalVoiceRoute(session, runtime); err != nil {
		a.setModuleVoiceState(false, "failed", runtime.manifest.RuntimeVersion, locationID, err)
		return err
	}
	a.setModuleVoiceState(true, "ready", runtime.manifest.RuntimeVersion, locationID, nil)
	return nil
}

func (a *app) stopModuleVoiceRouteChecked() error {
	return a.cleanupDirectQPCMVIfIdleWithMutationLock()
}

func (a *app) stopModuleVoiceRouteForceChecked() error {
	if a.implicitUACVoice {
		if !a.moduleMutationMu.TryLock() {
			return errors.New("another module mutation is active")
		}
		defer a.moduleMutationMu.Unlock()
		a.moduleVoiceOpMu.Lock()
		defer a.moduleVoiceOpMu.Unlock()
		return a.stopModuleVoiceRouteLocked()
	}
	return a.cleanupDirectQPCMVIfIdleWithMutationLock()
}

func (a *app) stopModuleVoiceRouteLocked() error {
	a.moduleVoiceMu.Lock()
	locationID := a.moduleVoiceLocation
	wasReady := a.moduleVoiceReady
	a.moduleVoiceMu.Unlock()
	if locationID == 0 {
		var locationErr error
		locationID, locationErr = a.currentDJIVoiceLocation()
		if locationErr != nil {
			a.setModuleVoiceState(false, "unknown", trustedVoiceRuntimeVersion, 0, locationErr)
			return locationErr
		}
	}
	session := &voiceADBSession{locationID: locationID}
	defer session.close()
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	err := stopExternalVoiceRoute(session, helper)
	if err != nil {
		a.setModuleVoiceState(wasReady, "cleanup_failed", trustedVoiceRuntimeVersion, locationID, err)
		return err
	}
	a.setModuleVoiceState(false, "stopped", trustedVoiceRuntimeVersion, locationID, nil)
	a.setModuleVoiceCallGeneration(0)
	return nil
}

func (a *app) stopModuleVoiceRoute() {
	_ = a.stopModuleVoiceRouteForceChecked()
}

func (a *app) setModuleVoiceCallGeneration(generation uint64) {
	a.moduleVoiceMu.Lock()
	a.moduleVoiceCallGeneration = generation
	a.moduleVoiceMu.Unlock()
}

// stopModuleVoiceRouteForCall is deliberately generation-scoped. The check is
// made only after acquiring moduleVoiceOpMu, so an old queued cleanup cannot
// stop a route that a newer call has already claimed.
func (a *app) stopModuleVoiceRouteForCall(expectedGeneration uint64) error {
	if !a.moduleMutationMu.TryLock() {
		return errors.New("另一项模块配置操作正在进行")
	}
	defer a.moduleMutationMu.Unlock()
	if a.implicitUACVoice {
		// The WebRTC/PCM lease is torn down separately. Keep the slow module-side
		// D4/UAC helper running between calls so the next ATA does not create a
		// connected-but-silent gap. Only release the generation ownership here.
		a.moduleVoiceOpMu.Lock()
		defer a.moduleVoiceOpMu.Unlock()
		a.moduleVoiceMu.Lock()
		if a.moduleVoiceCallGeneration == expectedGeneration {
			a.moduleVoiceCallGeneration = 0
		}
		a.moduleVoiceMu.Unlock()
		return nil
	}
	return a.stopDirectQPCMVForCall(expectedGeneration)
}

func (a *app) ensureModuleVoiceRouteForCall(ticket callMediaTicket) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if !a.moduleMutationMu.TryLock() {
		return errors.New("另一项模块配置操作正在进行")
	}
	defer a.moduleMutationMu.Unlock()
	if a.implicitUACVoice {
		return a.ensureExternalModuleVoiceRouteForCall(ticket)
	}
	return a.adoptArmedDirectQPCMVForCall(ticket)
}

func (a *app) confirmModuleVoiceCall(ticket callMediaTicket) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if !a.moduleMutationMu.TryLock() {
		return errors.New("另一项模块配置操作正在进行")
	}
	defer a.moduleMutationMu.Unlock()
	if a.implicitUACVoice {
		return a.confirmExternalModuleVoiceCall(ticket)
	}
	return a.confirmDirectQPCMVForCall(ticket)
}

func (a *app) ensureExternalModuleVoiceRouteForCall(ticket callMediaTicket) error {
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 || !a.callMediaTicketIsCurrent(ticket) {
		return errors.New("当前通话已变化，未启动模块音频")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if directQPCMVStateOwnsRescueHangup(state, ticket) {
		return nil
	}
	locationID, identityHash, err := a.readModuleIdentity()
	if err != nil {
		return err
	}
	if err := a.ensureModuleVoiceRouteLocked(); err != nil {
		return err
	}
	if !a.callMediaTicketIsCurrent(ticket) {
		_ = a.stopModuleVoiceRouteLocked()
		return errors.New("模块音频启动期间通话已变化")
	}
	a.setDirectQPCMVStateForOwner(
		true,
		"ready",
		"QDC507 D4/UAC route active",
		locationID,
		identityHash,
		directQPCMVCallIntent{direction: ticket.Direction, callID: ticket.CallID, index: ticket.Index},
		ticket.Generation,
		nil,
	)
	return nil
}

func (a *app) confirmExternalModuleVoiceCall(ticket callMediaTicket) error {
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 || !a.callMediaTicketIsCurrent(ticket) {
		return errors.New("当前通话已变化")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if !state.ready || state.phase != "ready" || state.generation != ticket.Generation || state.locationID == 0 {
		return errors.New("模块 D4/UAC 路由尚未就绪")
	}
	_, err := a.withExclusiveUSBATCommandsAtLocationExact(state.locationID, func(command usbATCommandFunc) error {
		response, commandErr := command("AT+CLCC", directQPCMVCommandTimeout)
		if commandErr != nil {
			return commandErr
		}
		if !directQPCMVExplicitOK(response) || !singleActiveCallMatches(parseCLCC(response), ticket) {
			return errors.New("模块未确认同一通活动通话")
		}
		return nil
	})
	return err
}

func (a *app) executeImplicitUACCallWithGate(
	locationID uint32,
	identityHash string,
	callCommand string,
	timeout time.Duration,
	gate func(*usbAT, usbATPhysicalIdentity) error,
) (string, error) {
	var response string
	_, err := a.withExclusiveUSBATCommandsAtLocationPinned(locationID, func(device *usbAT, identity usbATPhysicalIdentity, command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, identityHash); err != nil {
			return err
		}
		if gate != nil {
			if err := gate(device, identity); err != nil {
				return err
			}
		}
		var commandErr error
		response, commandErr = command(callCommand, timeout)
		return directCallCommandError(callCommand, response, commandErr)
	})
	return response, err
}

func (a *app) callAudioAvailable() bool {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	return a.moduleVoiceReady && a.moduleVoicePhase == "ready"
}

func (a *app) voiceStatus() map[string]any {
	a.moduleVoiceMu.Lock()
	ready := a.moduleVoiceReady
	last := a.moduleVoiceLast
	detail := a.moduleVoiceDetail
	phase := a.moduleVoicePhase
	locationID := a.moduleVoiceLocation
	callGeneration := a.moduleVoiceCallGeneration
	lastErr := a.moduleVoiceErr
	a.moduleVoiceMu.Unlock()
	backend := "direct_qpcmv_uac"
	runtimeExternal := false
	runtimeVersion := "none"
	if a.implicitUACVoice {
		backend = "qdc507_d4_uac"
		runtimeExternal = true
		runtimeVersion = trustedVoiceRuntimeVersion
	}
	return map[string]any{
		"ready":              ready,
		"route_backend":      backend,
		"runtime_external":   runtimeExternal,
		"runtime_version":    runtimeVersion,
		"runtime_path_scope": "not_used",
		"detail":             detail,
		"phase":              phase,
		"verified_location":  locationID,
		"call_generation":    callGeneration,
		"last_error":         lastErr,
		"last_attempt_at":    last,
	}
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStartAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要明确 confirm 才会启动模块语音路由")
		return
	}
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	ticket, ok := a.currentCallMediaTicket()
	if !ok {
		writeError(w, http.StatusConflict, "只有恰好一路 active 语音通话时才能启动路由")
		return
	}
	if err := a.ensureModuleVoiceRouteForCall(ticket); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	if err := a.cleanupDirectQPCMVIfIdleWithMutationLock(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	status := a.voiceStatus()
	if lastErr, _ := status["last_error"].(string); lastErr != "" {
		writeError(w, http.StatusBadGateway, lastErr)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
