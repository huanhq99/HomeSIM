package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type fakeMavoPhase1Hardware struct {
	ops              []string
	failAt           string
	panicAt          string
	setTargetAttempt bool
}

func (f *fakeMavoPhase1Hardware) step(name string) error {
	f.ops = append(f.ops, name)
	if f.panicAt == name {
		panic("injected " + name + " panic")
	}
	if f.failAt == name {
		return errors.New("injected " + name + " failure")
	}
	return nil
}

func phase1TestSnapshot() moduleSetupSnapshot {
	return moduleSetupSnapshot{
		USB: mavoPhase1TargetComposition, USBLocationID: 0x01100000,
		DeviceIdentityHash: strings.Repeat("a", 64), IMSVoLTECapability: 1,
	}
}

func (f *fakeMavoPhase1Hardware) preflight() (moduleSetupSnapshot, error) {
	return phase1TestSnapshot(), f.step("preflight")
}

func (f *fakeMavoPhase1Hardware) armEvidence(moduleSetupSnapshot) (string, error) {
	return "/private/phase1-evidence.json", f.step("arm")
}

func (f *fakeMavoPhase1Hardware) recordEvidence(_ string, phase string, _ *mavoPhase1TargetInspection, _ bool, _ string) error {
	return f.step("record:" + phase)
}

func (f *fakeMavoPhase1Hardware) setTarget(moduleSetupSnapshot) (bool, error) {
	err := f.step("set_target")
	return f.setTargetAttempt, err
}

func (f *fakeMavoPhase1Hardware) rebootAndVerifyTarget(moduleSetupSnapshot) error {
	return f.step("reboot_target")
}

func (f *fakeMavoPhase1Hardware) inspectTarget(moduleSetupSnapshot) (mavoPhase1TargetInspection, error) {
	err := f.step("inspect_target")
	return mavoPhase1TargetInspection{ADBDescriptorValid: true, UACDescriptorValid: true}, err
}

func (f *fakeMavoPhase1Hardware) restoreOriginal(moduleSetupSnapshot) error {
	return f.step("restore")
}

func TestExecuteMavoPhase1SuccessRestoresFactory(t *testing.T) {
	fake := &fakeMavoPhase1Hardware{setTargetAttempt: true}
	result := executeMavoPhase1(fake)
	if result.Err != nil || !result.MutationAttempted || !result.TargetVerified || !result.RollbackVerified {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := []string{
		"preflight", "arm", "set_target", "record:target_configured", "reboot_target",
		"record:target_enumerated", "inspect_target", "record:target_verified", "restore", "record:completed",
	}
	if !slices.Equal(fake.ops, want) {
		t.Fatalf("operation order = %v, want %v", fake.ops, want)
	}
}

func TestExecuteMavoPhase1FailureBoundaries(t *testing.T) {
	tests := []struct {
		name          string
		failAt        string
		attempt       bool
		wantRestore   bool
		wantRollback  bool
		wantAttempted bool
	}{
		{name: "preflight", failAt: "preflight"},
		{name: "evidence arm", failAt: "arm"},
		{name: "before write", failAt: "set_target", attempt: false},
		{name: "write uncertain", failAt: "set_target", attempt: true, wantRestore: true, wantRollback: true, wantAttempted: true},
		{name: "evidence after write", failAt: "record:target_configured", attempt: true, wantRestore: true, wantRollback: true, wantAttempted: true},
		{name: "target reboot", failAt: "reboot_target", attempt: true, wantRestore: true, wantRollback: true, wantAttempted: true},
		{name: "target inspection", failAt: "inspect_target", attempt: true, wantRestore: true, wantRollback: true, wantAttempted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMavoPhase1Hardware{failAt: test.failAt, setTargetAttempt: test.attempt}
			result := executeMavoPhase1(fake)
			if result.Err == nil {
				t.Fatal("failure injection unexpectedly succeeded")
			}
			if result.MutationAttempted != test.wantAttempted || result.RollbackVerified != test.wantRollback {
				t.Fatalf("unexpected result: %+v", result)
			}
			hasRestore := slices.Contains(fake.ops, "restore")
			if hasRestore != test.wantRestore {
				t.Fatalf("restore=%v, operations=%v", hasRestore, fake.ops)
			}
		})
	}
}

func TestExecuteMavoPhase1RollbackFailureIsRecoveryRequired(t *testing.T) {
	fake := &fakeMavoPhase1Hardware{setTargetAttempt: true, failAt: "restore"}
	result := executeMavoPhase1(fake)
	if result.Err == nil || result.RollbackVerified || !result.MutationAttempted ||
		!strings.Contains(result.Err.Error(), "automatic rollback failed") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !slices.Contains(fake.ops, "record:recovery_required") {
		t.Fatalf("missing durable recovery-required record: %v", fake.ops)
	}
}

func TestExecuteMavoPhase1PanicStillRestores(t *testing.T) {
	fake := &fakeMavoPhase1Hardware{setTargetAttempt: true, panicAt: "inspect_target"}
	result := executeMavoPhase1(fake)
	if result.Err == nil || !result.RollbackVerified || !slices.Contains(fake.ops, "restore") {
		t.Fatalf("panic did not produce a verified rollback: result=%+v operations=%v", result, fake.ops)
	}
}

func TestParseMavoPhase1TargetOutput(t *testing.T) {
	valid := strings.Join([]string{
		"uid=0", "kernel=3.18.44", "arch=armv7l", "ttygs0=1", "voc_server=1",
		"alsaucm_test=1", "audio_library=1", "armel_loader=1", "qdc_modules_absent=1",
		"__MAVO_STATUS_deadbeef_0__",
	}, "\r\n")
	inspection, err := parseMavoPhase1TargetOutput(valid)
	if err != nil || !inspection.ADBRoot || !inspection.QDCModulesAbsent {
		t.Fatalf("valid target output rejected: inspection=%+v err=%v", inspection, err)
	}
	for name, invalid := range map[string]string{
		"missing":    strings.Replace(valid, "ttygs0=1\r\n", "", 1),
		"duplicate":  valid + "\nttygs0=1\n",
		"unexpected": valid + "\nimei=123456789012345\n",
		"module":     strings.Replace(valid, "qdc_modules_absent=1", "qdc_modules_absent=0", 1),
		"kernel":     strings.Replace(valid, "kernel=3.18.44", "kernel=3.18.45", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMavoPhase1TargetOutput(invalid); err == nil {
				t.Fatal("invalid target output accepted")
			}
		})
	}
}

func TestMavoPhase1AcceptsBothDocumentedFullUSBIdentities(t *testing.T) {
	for _, composition := range []usbComposition{
		mavoPhase1TargetComposition,
		{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}},
	} {
		if !isMavoPhase1FullComposition(composition) {
			t.Fatalf("documented complete composition rejected: %+v", composition)
		}
	}
	if isMavoPhase1FullComposition(usbComposition{
		VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1},
	}) {
		t.Fatal("legacy ADB-off UAC profile must not satisfy the MaVo ADB gate")
	}
}

func TestLegacyUACTargetIsSeparateFromMaVoADBTarget(t *testing.T) {
	if !legacyUACTargetComposition.isUACTarget() || legacyUACTargetComposition.hasADB() {
		t.Fatalf("legacy target is not the documented ADB-off UAC layout: %+v", legacyUACTargetComposition)
	}
	if got := legacyUACTargetComposition.command(); got != `AT+QCFG="USBCFG",0x2C7C,0x0125,1,1,1,1,1,0,1` {
		t.Fatalf("legacy target command = %q", got)
	}
	if isMavoPhase1FullComposition(legacyUACTargetComposition) {
		t.Fatal("legacy target must not satisfy the ADB+UAC phase-1 gate")
	}
}

func TestParsePhase1Registration(t *testing.T) {
	for raw, want := range map[string]int{
		"+CEREG: 1\r\nOK\r\n":   1,
		"+CEREG: 2,5\r\nOK\r\n": 5,
	} {
		got, err := parsePhase1Registration(raw)
		if err != nil || got != want {
			t.Fatalf("parsePhase1Registration(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	if _, err := parsePhase1Registration("OK\r\n"); err == nil {
		t.Fatal("missing CEREG accepted")
	}
}

func TestPhase1ErrorsRedactLongIdentifiers(t *testing.T) {
	got := phase1SafeError(errors.New("identity 123456789012345\nfailed"))
	if strings.Contains(got, "123456789012345") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("identifier was not redacted: %q", got)
	}
}

func TestMavoPhase1APIGatesCannotStartFromGenericConfirmation(t *testing.T) {
	a := &app{}
	for name, body := range map[string]string{
		"unknown experiment": `{"confirm":true,"experiment":"other"}`,
		"missing confirm":    `{"confirm":false,"experiment":"mavo_adb_uac_roundtrip","phase1_confirmation":"` + mavoPhase1Confirmation + `"}`,
		"wrong phrase":       `{"confirm":true,"experiment":"mavo_adb_uac_roundtrip","phase1_confirmation":"yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/module/setup", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			a.moduleSetupStartAPI(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLegacyUACAPIGatesRequireSeparateExactConfirmation(t *testing.T) {
	a := &app{}
	for name, body := range map[string]string{
		"missing confirm":          `{"confirm":false,"experiment":"legacy_uac_roundtrip","legacy_uac_confirmation":"` + legacyUACConfirmation + `"}`,
		"wrong phrase":             `{"confirm":true,"experiment":"legacy_uac_roundtrip","legacy_uac_confirmation":"yes"}`,
		"recovery missing confirm": `{"confirm":false,"experiment":"legacy_uac_restore","legacy_uac_confirmation":"` + legacyUACConfirmation + `"}`,
		"recovery wrong phrase":    `{"confirm":true,"experiment":"legacy_uac_restore","legacy_uac_confirmation":"yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/module/setup", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			a.moduleSetupStartAPI(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLegacyUACRecoveryEvidenceBindsOriginalAndTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "Library", "Application Support", "MacCellular", "module-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := moduleSetupSnapshot{
		USB:        usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 0}},
		USBNetMode: 1, IMSConfiguration: 1, IMSVoLTECapability: 1,
		USBLocationID: 0x01100000, DeviceIdentityHash: strings.Repeat("a", 64),
	}
	target := original
	target.USB = legacyUACTargetComposition
	path := filepath.Join(dir, "legacy-uac-test.json")
	if err := createPrivateJSON(path, mavoPhase1Evidence{
		Schema: legacyUACEvidenceV1, Phase: "recovery_required", Original: original, Target: target.USB,
	}); err != nil {
		t.Fatal(err)
	}
	if err := updateLegacyUACRecoveryEvidence(path, original, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got mavoPhase1Evidence
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != "rolled_back" || !got.RollbackVerified {
		t.Fatalf("recovery evidence was not closed: %+v", got)
	}
	wrongTarget := target
	wrongTarget.USB = mavoPhase1TargetComposition
	if err := updateLegacyUACRecoveryEvidence(path, original, wrongTarget); err == nil {
		t.Fatal("mismatched recovery target updated the evidence")
	}
}

func TestMavoPhase1StatusIsNotOverwrittenByOrdinaryInspection(t *testing.T) {
	a := &app{moduleSetup: moduleSetupStatus{State: "mavo_phase1_rolled_back", Summary: "preserved"}}
	response := httptest.NewRecorder()
	a.moduleSetupStatusAPI(response, httptest.NewRequest(http.MethodGet, "/api/module/setup", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "preserved") {
		t.Fatalf("phase status was not preserved: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMavoPhase1FilesContainNoRuntimeLoadOrCallCommands(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(current), "mavo_phase1*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find phase files: %v", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`AT+QPCMV`, `.push(`, `insmod`, `"ATD`, `"ATA"`, `"ATH"`} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s contains forbidden phase-1 operation %q", filepath.Base(path), forbidden)
			}
		}
	}
}

func TestLegacyUACRoundtripContainsNoRuntimeLoadOrCallCommands(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(current), "legacy_uac_roundtrip.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{".push(", "insmod", `"ATD`, `"ATA`, `"ATH`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("legacy UAC experiment contains forbidden operation %q", forbidden)
		}
	}
}
