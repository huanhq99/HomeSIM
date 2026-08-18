package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/turnauth"
)

func TestExternalVoiceConfigIsDefaultOffAndRejectsPartialOptions(t *testing.T) {
	config, err := parseExternalVoiceStartupConfig(externalVoiceFlagConfig{})
	if err != nil || config != nil {
		t.Fatalf("default config=%+v err=%v", config, err)
	}
	if _, err := parseExternalVoiceStartupConfig(externalVoiceFlagConfig{
		GatewayID: "synthetic-gateway",
	}); err == nil {
		t.Fatal("partial external voice configuration was accepted")
	}
	if _, err := parseExternalVoiceStartupConfig(externalVoiceFlagConfig{Provider: " "}); err == nil {
		t.Fatal("whitespace provider option was silently treated as disabled")
	}
}

func TestExternalVoiceConfigValidatesPrivateSecretAndExactNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external voice startup is intentionally unsupported on Windows")
	}
	controlSecret := writeExternalVoiceTestSecret(t, "synthetic-control-password\n", 0o600)
	recoverySecret := writeExternalVoiceTestSecret(t, "synthetic-recovery-password\n", 0o600)
	raw := validExternalVoiceFlagConfig(controlSecret, recoverySecret)
	config, err := parseExternalVoiceStartupConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider != externalVoiceProviderAsterisk || config.ARIArgument != "incoming" ||
		config.ARIExpectedEntityID != "02:00:00:00:00:01" || config.ARIExpectedVersion != "22.10.1" ||
		config.ARIIncomingPolicyID != "djonehub-incoming-v1" ||
		config.ARIRecoveryUsername != "synthetic-recovery" || config.RecoveryStorePath == "" ||
		len(config.MediaLocalCIDRs) != 1 || config.MediaLocalCIDRs[0] != "100.64.10.1/32" ||
		len(config.MediaRemoteCIDRs) != 1 || config.MediaRemoteCIDRs[0] != "100.64.10.2/32" {
		t.Fatalf("unexpected normalized config: %+v", config)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", raw), fmt.Sprintf("%+v", raw), fmt.Sprintf("%#v", raw),
		fmt.Sprintf("%v", *config), fmt.Sprintf("%+v", *config), fmt.Sprintf("%#v", *config),
	} {
		if strings.Contains(formatted, "synthetic-control-password") ||
			strings.Contains(formatted, "synthetic-recovery-password") {
			t.Fatalf("configuration formatting leaked secret material: %s", formatted)
		}
	}

	invalid := raw
	invalid.MediaRemoteCIDRs = "100.64.10.2/32,100.64.10.3/32"
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("multiple remote media peers were accepted")
	}
	invalid = raw
	invalid.MediaRemoteCIDRs = "192.0.2.10/32"
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("non-Tailnet media peer was accepted")
	}
	invalid = raw
	invalid.ARIURL = "https://pbx.example.invalid/ari"
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("non-loopback ARI/media endpoint was accepted")
	}
	invalid = raw
	invalid.AllowAnswer = false
	invalid.AllowEnd = true
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("active-call end without answer opt-in was accepted")
	}
	invalid = raw
	invalid.ARIExpectedEntityID = ""
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("external mutations without a pinned PBX entity were accepted")
	}
	invalid = raw
	invalid.ARIExpectedEntityID = "00:11:22:33:44:55"
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("non-local Asterisk entity ID was accepted")
	}
	invalid = raw
	invalid.ARIRecoveryUsername = invalid.ARIUsername
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("shared control and recovery ARI username was accepted")
	}
	invalid = raw
	invalid.ARIRecoveryPasswordFile = invalid.ARIPasswordFile
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("shared control and recovery password file was accepted")
	}
	invalid = raw
	invalid.ARIRecoveryPasswordFile = writeExternalVoiceTestSecret(t, "synthetic-control-password\n", 0o600)
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("shared control and recovery secret was accepted")
	}
	invalid = raw
	invalid.ARIExpectedVersion = ""
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("external voice without an exact Asterisk version was accepted")
	}
	invalid = raw
	invalid.ARIIncomingPolicyID = ""
	if _, err := parseExternalVoiceStartupConfig(invalid); err == nil {
		t.Fatal("external voice without an exact dialplan policy was accepted")
	}

	relayOnly := raw
	relayOnly.MediaInterface = ""
	relayOnly.MediaLocalCIDRs = ""
	relayOnly.MediaRemoteCIDRs = ""
	parsedRelayOnly, err := parseExternalVoiceStartupConfig(relayOnly)
	if err != nil || parsedRelayOnly.MediaInterface != "" ||
		len(parsedRelayOnly.MediaLocalCIDRs) != 0 || len(parsedRelayOnly.MediaRemoteCIDRs) != 0 {
		t.Fatalf("public TURN relay base config=%+v err=%v", parsedRelayOnly, err)
	}
	partialRelay := relayOnly
	partialRelay.MediaInterface = "utun42"
	if _, err := parseExternalVoiceStartupConfig(partialRelay); err == nil {
		t.Fatal("partial Tailnet media configuration was accepted")
	}
}

func TestExternalVoicePasswordFileFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external voice startup is intentionally unsupported on Windows")
	}
	weak := writeExternalVoiceTestSecret(t, "synthetic-password\n", 0o644)
	if _, err := readExternalVoicePassword(weak); err == nil {
		t.Fatal("weak password file mode was accepted")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("synthetic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readExternalVoicePassword(symlink); err == nil {
		t.Fatal("symlink password file was accepted")
	}
	hardlink := filepath.Join(root, "hardlink")
	if err := os.Link(target, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readExternalVoicePassword(target); err == nil {
		t.Fatal("multi-link password file was accepted")
	}
	for name, value := range map[string]string{
		"empty": "", "two-lines": "first\nsecond\n", "carriage-return": "secret\r\n",
	} {
		path := writeExternalVoiceTestSecret(t, value, 0o600)
		if _, err := readExternalVoicePassword(path); err == nil {
			t.Fatalf("%s password file was accepted", name)
		}
	}
}

func TestExternalVoiceRecoveryPasswordFileFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external voice startup is intentionally unsupported on Windows")
	}
	control := writeExternalVoiceTestSecret(t, "synthetic-control-password\n", 0o600)
	recovery := writeExternalVoiceTestSecret(t, "synthetic-recovery-password\n", 0o644)
	if _, err := parseExternalVoiceStartupConfig(validExternalVoiceFlagConfig(control, recovery)); err == nil {
		t.Fatal("weak recovery password file mode was accepted")
	}
}

func TestExternalVoiceStartupRequiresRemoteControlAndRejectsLegacyVoice(t *testing.T) {
	config := &externalVoiceStartupConfig{
		Provider: externalVoiceProviderAsterisk, MediaInterface: "utun42",
		MediaLocalCIDRs:  []string{"100.64.10.1/32"},
		MediaRemoteCIDRs: []string{"100.64.10.2/32"},
	}
	application := &app{remoteAccess: remoteAccessConfig{
		AllowedLogins: map[string]struct{}{"owner@example.invalid": {}},
		Capability:    "example.invalid/cap/phone", Host: "synthetic.tail123.ts.net",
	}}
	if err := application.configureExternalVoiceStartup(config); err == nil {
		t.Fatal("external voice was enabled without remote control")
	}
	application.remoteAccess.Control = true
	if err := application.configureExternalVoiceStartup(config); err != nil {
		t.Fatal(err)
	}
	config.MediaLocalCIDRs[0] = "100.64.99.1/32"
	if application.sipVoiceStartup.MediaLocalCIDRs[0] != "100.64.10.1/32" {
		t.Fatal("startup configuration retained caller-owned network slice")
	}
	application.remoteIncomingAnswer = true
	if err := application.configureExternalVoiceStartup(config); err == nil {
		t.Fatal("external voice was combined with the frozen QPCMV path")
	}
	application.remoteIncomingAnswer = false
	application.remoteMedia = &remoteMediaManager{}
	if err := application.configureExternalVoiceStartup(config); err == nil {
		t.Fatal("external voice was combined with legacy remote media")
	}
}

func TestExternalVoiceStartupAcceptsExplicitPublicTURNWithoutTailnet(t *testing.T) {
	issuer, err := turnauth.New(turnauth.Config{
		Host: "turn.example.invalid", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: []byte(strings.Repeat("S", 48)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = issuer.Close() })
	application := &app{publicWeb: &publicWebStartupConfig{
		Control: true, ExternalVoice: true, TURNIssuer: issuer,
	}}
	config := &externalVoiceStartupConfig{Provider: externalVoiceProviderAsterisk}
	if err := application.configureExternalVoiceStartup(config); err != nil {
		t.Fatalf("configure public TURN voice: %v", err)
	}
	if application.sipVoiceStartup == nil || application.sipVoiceStartup.MediaInterface != "" {
		t.Fatalf("unexpected public TURN startup: %+v", application.sipVoiceStartup)
	}
}

func TestStartExternalVoiceBuildsPublicRelayBaseWithoutTailnet(t *testing.T) {
	controlSecret := writeExternalVoiceTestSecret(t, "synthetic-control-password\n", 0o600)
	recoverySecret := writeExternalVoiceTestSecret(t, "synthetic-recovery-password\n", 0o600)
	raw := validExternalVoiceFlagConfig(controlSecret, recoverySecret)
	raw.MediaInterface = ""
	raw.MediaLocalCIDRs = ""
	raw.MediaRemoteCIDRs = ""
	raw.RecoveryStorePath = filepath.Join(t.TempDir(), "voice", "recovery.json")
	config, err := parseExternalVoiceStartupConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := turnauth.New(turnauth.Config{
		Host: "turn.example.invalid", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: []byte(strings.Repeat("S", 48)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.Close()
	application := &app{publicWeb: &publicWebStartupConfig{
		Control: true, ExternalVoice: true, TURNIssuer: issuer,
	}}
	if err := application.configureExternalVoiceStartup(config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.startExternalVoice(ctx); err != nil {
		t.Fatalf("start public TURN external voice: %v", err)
	}
	if application.sipVoice == nil {
		t.Fatal("public TURN external voice did not install a supervisor")
	}
	if err := application.sipVoice.Close(); err != nil {
		t.Fatalf("close public TURN external voice: %v", err)
	}
}

func TestExternalVoiceConfigurationBlocksEveryLegacyModuleVoiceEntryPoint(t *testing.T) {
	application := &app{sipVoiceStartup: &externalVoiceStartupConfig{Provider: externalVoiceProviderAsterisk}}
	ticket := callMediaTicket{
		Generation: 1,
		CallID:     "synthetic-call",
		Index:      1,
		Direction:  "incoming",
	}
	checks := map[string]func() error{
		"automatic incoming arm": application.autoArmIncomingDirectQPCMV,
		"setup preflight": func() error {
			return application.preflightDirectQPCMVAtLocation(1, "synthetic-identity")
		},
		"direct arm": func() error {
			return application.armDirectQPCMV(1, "synthetic-identity", false)
		},
		"armed answer": func() error {
			_, err := application.executeCallOnArmedDirectQPCMV("ATA", time.Second)
			return err
		},
		"active adopt":   func() error { return application.adoptArmedDirectQPCMVForCall(ticket) },
		"active confirm": func() error { return application.confirmDirectQPCMVForCall(ticket) },
		"remote rescue hangup": func() error {
			_, err := application.executeRemoteRescueHangup("owner@example.invalid", "rms_synthetic", 1, ticket)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); !errors.Is(err, errLegacyVoiceDisabledByExternalSIP) {
				t.Fatalf("error=%v, want external SIP mutual-exclusion gate", err)
			}
		})
	}
}

func TestExternalVoiceConfigurationBlocksLegacyLocalCallHTTP(t *testing.T) {
	application := &app{sipVoiceStartup: &externalVoiceStartupConfig{Provider: externalVoiceProviderAsterisk}}
	tests := []struct {
		name    string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{name: "reject", path: "/api/calls/reject", body: `{}`, handler: application.rejectCall},
		{name: "answer", path: "/api/calls/answer", body: `{}`, handler: application.answerCall},
		{name: "hangup", path: "/api/calls/hangup", body: `{}`, handler: application.hangupCall},
		{name: "dtmf", path: "/api/calls/dtmf", body: `{"digit":"1"}`, handler: application.dtmfCall},
		{name: "dial", path: "/api/calls/dial", body: `{"number":"10086"}`, handler: application.dialCall},
		{name: "audio start", path: "/api/calls/audio/start", body: `{}`, handler: application.audioStart},
		{name: "recording start", path: "/api/calls/audio/record", body: `{"action":"start"}`, handler: application.audioRecord},
		{name: "voice start", path: "/api/voice/start", body: `{"confirm":true}`, handler: application.voiceStartAPI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			test.handler(response, request)
			if response.Code != http.StatusConflict ||
				!strings.Contains(response.Body.String(), "local modem call control is disabled") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func validExternalVoiceFlagConfig(controlSecretPath, recoverySecretPath string) externalVoiceFlagConfig {
	return externalVoiceFlagConfig{
		Provider: externalVoiceProviderAsterisk, GatewayID: "synthetic-home-gateway",
		ARIURL: "http://127.0.0.1:18088/ari", ARIApplication: "maccellular_voice",
		ARIContext: "from-cellular", ARIEndpoint: "cellular-gateway",
		ARIUsername: "synthetic-control", ARIPasswordFile: controlSecretPath,
		ARIRecoveryUsername: "synthetic-recovery", ARIRecoveryPasswordFile: recoverySecretPath,
		ARIExpectedEntityID: "02:00:00:00:00:01", ARIExpectedVersion: "22.10.1",
		ARIIncomingPolicyID: "djonehub-incoming-v1",
		MediaInterface:      "utun42", MediaLocalCIDRs: "100.64.10.1/32",
		MediaRemoteCIDRs: "100.64.10.2/32", MediaUDPMin: 41000, MediaUDPMax: 41015,
		AllowAnswer: true, AllowEnd: true,
	}
}

func writeExternalVoiceTestSecret(t *testing.T, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asterisk-password")
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil && !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	return path
}
