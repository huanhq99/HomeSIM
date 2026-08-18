package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
	"github.com/iniwex5/vohive/internal/sipgateway/asterisk"
)

const (
	externalVoiceProviderAsterisk = "asterisk"
	externalVoiceSecretMaxBytes   = 4096
	externalVoiceFreshness        = 2 * time.Second
)

var errLegacyVoiceDisabledByExternalSIP = errors.New("local modem call control is disabled while external SIP voice is configured")

type externalVoiceStartupConfig struct {
	Provider                string
	GatewayID               string
	ARIURL                  string
	ARIApplication          string
	ARIArgument             string
	ARIContext              string
	ARIEndpoint             string
	ARIOutgoingEndpoint     string
	ARIUsername             string
	ARIPasswordFile         string
	ARIRecoveryUsername     string
	ARIRecoveryPasswordFile string
	ARIExpectedEntityID     string
	ARIExpectedVersion      string
	ARIIncomingPolicyID     string
	RecoveryStorePath       string
	MediaInterface          string
	MediaLocalCIDRs         []string
	MediaRemoteCIDRs        []string
	MediaUDPMin             uint16
	MediaUDPMax             uint16
	AllowAnswer             bool
	AllowReject             bool
	AllowDTMF               bool
	AllowEnd                bool
	AllowDial               bool
}

func (externalVoiceStartupConfig) String() string   { return "externalVoiceStartupConfig{redacted}" }
func (externalVoiceStartupConfig) GoString() string { return "externalVoiceStartupConfig{redacted}" }

type externalVoiceFlagConfig struct {
	Provider                string
	GatewayID               string
	ARIURL                  string
	ARIApplication          string
	ARIArgument             string
	ARIContext              string
	ARIEndpoint             string
	ARIOutgoingEndpoint     string
	ARIUsername             string
	ARIPasswordFile         string
	ARIRecoveryUsername     string
	ARIRecoveryPasswordFile string
	ARIExpectedEntityID     string
	ARIExpectedVersion      string
	ARIIncomingPolicyID     string
	RecoveryStorePath       string
	MediaInterface          string
	MediaLocalCIDRs         string
	MediaRemoteCIDRs        string
	MediaUDPMin             uint
	MediaUDPMax             uint
	AllowAnswer             bool
	AllowReject             bool
	AllowDTMF               bool
	AllowEnd                bool
	AllowDial               bool
}

func (externalVoiceFlagConfig) String() string   { return "externalVoiceFlagConfig{redacted}" }
func (externalVoiceFlagConfig) GoString() string { return "externalVoiceFlagConfig{redacted}" }

func parseExternalVoiceStartupConfig(raw externalVoiceFlagConfig) (*externalVoiceStartupConfig, error) {
	provider := strings.TrimSpace(raw.Provider)
	if provider != raw.Provider {
		return nil, errExternalVoiceInvalid
	}
	if provider == "" {
		if externalVoiceFlagsHaveValues(raw) {
			return nil, errors.New("external voice provider is required when any SIP voice option is set")
		}
		return nil, nil
	}
	if provider != externalVoiceProviderAsterisk || raw.MediaUDPMin > 65535 || raw.MediaUDPMax > 65535 {
		return nil, errExternalVoiceInvalid
	}
	if raw.AllowEnd && !raw.AllowAnswer {
		return nil, errors.New("external active-call end requires incoming answer opt-in")
	}
	if raw.AllowDTMF && !raw.AllowAnswer {
		return nil, errors.New("external DTMF requires incoming answer opt-in")
	}
	if raw.AllowDial && !raw.AllowAnswer {
		return nil, errors.New("external dialing requires incoming answer opt-in")
	}
	if (raw.AllowDial && strings.TrimSpace(raw.ARIOutgoingEndpoint) == "") ||
		(!raw.AllowDial && strings.TrimSpace(raw.ARIOutgoingEndpoint) != "") {
		return nil, errors.New("external outgoing endpoint requires explicit dialing opt-in")
	}
	if raw.ARIOutgoingEndpoint != strings.TrimSpace(raw.ARIOutgoingEndpoint) {
		return nil, errExternalVoiceInvalid
	}
	values := []string{
		raw.GatewayID, raw.ARIURL, raw.ARIApplication, raw.ARIContext, raw.ARIEndpoint,
		raw.ARIUsername, raw.ARIPasswordFile, raw.ARIRecoveryUsername,
		raw.ARIRecoveryPasswordFile, raw.ARIExpectedVersion, raw.ARIIncomingPolicyID,
	}
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return nil, errExternalVoiceInvalid
		}
	}
	argument := raw.ARIArgument
	if argument == "" {
		argument = "incoming"
	} else if argument != strings.TrimSpace(argument) {
		return nil, errExternalVoiceInvalid
	}
	parsedARIURL, err := url.Parse(raw.ARIURL)
	if err != nil || parsedARIURL == nil || !isLoopbackHTTPHost(parsedARIURL.Host) {
		return nil, errors.New("external voice Asterisk endpoint must stay on loopback")
	}
	passwordPath := filepath.Clean(raw.ARIPasswordFile)
	recoveryPasswordPath := filepath.Clean(raw.ARIRecoveryPasswordFile)
	if !filepath.IsAbs(passwordPath) || passwordPath != raw.ARIPasswordFile ||
		!filepath.IsAbs(recoveryPasswordPath) || recoveryPasswordPath != raw.ARIRecoveryPasswordFile {
		return nil, errors.New("Asterisk password files must be clean absolute paths")
	}
	if passwordPath == recoveryPasswordPath || raw.ARIUsername == raw.ARIRecoveryUsername {
		return nil, errors.New("Asterisk control and recovery credentials must be independent")
	}
	expectedEntityID := strings.TrimSpace(raw.ARIExpectedEntityID)
	if expectedEntityID != raw.ARIExpectedEntityID || (raw.AllowAnswer || raw.AllowEnd) && expectedEntityID == "" {
		return nil, errors.New("external voice mutations require an exact Asterisk entity ID")
	}
	if expectedEntityID != "" && !validLocallyAdministeredEntityID(expectedEntityID) {
		return nil, errors.New("Asterisk entity ID must be a stable locally administered unicast identifier")
	}
	recoveryPath := strings.TrimSpace(raw.RecoveryStorePath)
	if recoveryPath == "" {
		recoveryPath = defaultExternalVoiceRecoveryStorePath()
	}
	if recoveryPath == "" || !filepath.IsAbs(recoveryPath) || filepath.Clean(recoveryPath) != recoveryPath {
		return nil, errors.New("external voice recovery store must be a clean absolute path")
	}
	mediaInterface := strings.TrimSpace(raw.MediaInterface)
	localRaw := strings.TrimSpace(raw.MediaLocalCIDRs)
	remoteRaw := strings.TrimSpace(raw.MediaRemoteCIDRs)
	if mediaInterface != raw.MediaInterface || localRaw != raw.MediaLocalCIDRs || remoteRaw != raw.MediaRemoteCIDRs {
		return nil, errExternalVoiceInvalid
	}
	var localCIDRs, remoteCIDRs []string
	tailnetMedia := mediaInterface != "" || localRaw != "" || remoteRaw != ""
	if tailnetMedia {
		if mediaInterface == "" || localRaw == "" || remoteRaw == "" ||
			!remoteMediaInterfacePattern.MatchString(mediaInterface) {
			return nil, errors.New("external voice Tailnet media configuration is incomplete")
		}
		localCIDRs, err = parseRemoteMediaCIDRs(localRaw, false)
		if err != nil {
			return nil, fmt.Errorf("external voice local media allowlist: %w", err)
		}
		remoteCIDRs, err = parseRemoteMediaCIDRs(remoteRaw, true)
		if err != nil {
			return nil, fmt.Errorf("external voice remote media allowlist: %w", err)
		}
		if len(remoteCIDRs) != 1 {
			return nil, errors.New("external voice requires exactly one Tailnet media peer")
		}
		for _, values := range [][]string{localCIDRs, remoteCIDRs} {
			for _, value := range values {
				_, network, parseErr := net.ParseCIDR(value)
				if parseErr != nil || !remoteMediaNetworkWithin(network, remoteMediaTailscaleIPv4) &&
					!remoteMediaNetworkWithin(network, remoteMediaTailscaleIPv6) {
					return nil, errors.New("external voice media CIDRs must stay inside Tailscale ranges")
				}
			}
		}
	}
	udpMin, udpMax := uint16(raw.MediaUDPMin), uint16(raw.MediaUDPMax)
	if udpMin == 0 || udpMax < udpMin || udpMax-udpMin > 1024 {
		return nil, errors.New("external voice UDP range must contain between 1 and 1025 ports")
	}
	password, err := readExternalVoicePassword(raw.ARIPasswordFile)
	if err != nil {
		return nil, fmt.Errorf("Asterisk password file is unavailable: %w", err)
	}
	recoveryPassword, err := readExternalVoicePassword(raw.ARIRecoveryPasswordFile)
	if err != nil {
		return nil, fmt.Errorf("Asterisk recovery password file is unavailable: %w", err)
	}
	if password == recoveryPassword {
		return nil, errors.New("Asterisk control and recovery secrets must differ")
	}
	baseConfig := asterisk.Config{
		GatewayID: raw.GatewayID, BaseURL: raw.ARIURL,
		Application: raw.ARIApplication, IncomingArgument: argument,
		IncomingContext: raw.ARIContext, IncomingEndpoint: raw.ARIEndpoint,
		OutgoingEndpoint: raw.ARIOutgoingEndpoint,
		ExpectedVersion:  raw.ARIExpectedVersion, IncomingPolicyID: raw.ARIIncomingPolicyID,
		ExpectedEntityID:    expectedEntityID,
		RequireCleanStartup: raw.AllowAnswer || raw.AllowEnd,
	}
	controlConfig := baseConfig
	controlConfig.Username, controlConfig.Password = raw.ARIUsername, password
	if err := asterisk.ValidateConfig(controlConfig); err != nil {
		return nil, errors.New("Asterisk configuration is invalid")
	}
	recoveryConfig := baseConfig
	recoveryConfig.Username, recoveryConfig.Password = raw.ARIRecoveryUsername, recoveryPassword
	if err := asterisk.ValidateConfig(recoveryConfig); err != nil {
		return nil, errors.New("Asterisk recovery configuration is invalid")
	}
	return &externalVoiceStartupConfig{
		Provider: provider, GatewayID: raw.GatewayID,
		ARIURL: raw.ARIURL, ARIApplication: raw.ARIApplication,
		ARIArgument: argument, ARIContext: raw.ARIContext, ARIEndpoint: raw.ARIEndpoint,
		ARIOutgoingEndpoint: raw.ARIOutgoingEndpoint,
		ARIUsername:         raw.ARIUsername, ARIPasswordFile: passwordPath,
		ARIRecoveryUsername:     raw.ARIRecoveryUsername,
		ARIRecoveryPasswordFile: recoveryPasswordPath,
		ARIExpectedEntityID:     expectedEntityID, ARIExpectedVersion: raw.ARIExpectedVersion,
		ARIIncomingPolicyID: raw.ARIIncomingPolicyID, RecoveryStorePath: recoveryPath,
		MediaInterface: mediaInterface, MediaLocalCIDRs: localCIDRs,
		MediaRemoteCIDRs: remoteCIDRs, MediaUDPMin: udpMin, MediaUDPMax: udpMax,
		AllowAnswer: raw.AllowAnswer, AllowReject: raw.AllowReject, AllowDTMF: raw.AllowDTMF,
		AllowEnd: raw.AllowEnd, AllowDial: raw.AllowDial,
	}, nil
}

func externalVoiceFlagsHaveValues(raw externalVoiceFlagConfig) bool {
	return raw.Provider != "" || raw.GatewayID != "" || raw.ARIURL != "" || raw.ARIApplication != "" ||
		raw.ARIArgument != "" || raw.ARIContext != "" || raw.ARIEndpoint != "" || raw.ARIOutgoingEndpoint != "" ||
		raw.ARIUsername != "" || raw.ARIPasswordFile != "" || raw.ARIRecoveryUsername != "" ||
		raw.ARIRecoveryPasswordFile != "" || raw.ARIExpectedEntityID != "" ||
		raw.ARIExpectedVersion != "" || raw.ARIIncomingPolicyID != "" ||
		raw.RecoveryStorePath != "" || raw.MediaInterface != "" ||
		raw.MediaLocalCIDRs != "" || raw.MediaRemoteCIDRs != "" || raw.MediaUDPMin != 0 ||
		raw.MediaUDPMax != 0 || raw.AllowAnswer || raw.AllowReject || raw.AllowDTMF || raw.AllowEnd || raw.AllowDial
}

func validLocallyAdministeredEntityID(value string) bool {
	hardware, err := net.ParseMAC(value)
	return err == nil && len(hardware) == 6 && hardware[0]&0x01 == 0 && hardware[0]&0x02 != 0
}

func defaultExternalVoiceRecoveryStorePath() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(base, "MacCellular", "remote", "external-voice", "recovery.json")
}

func (a *app) configureExternalVoiceStartup(cfg *externalVoiceStartupConfig) error {
	if cfg == nil {
		a.sipVoiceStartup = nil
		return nil
	}
	tailnetControl := a.remoteAccess.enabled() && a.remoteAccess.Control
	publicControl := a.publicWeb != nil && a.publicWeb.Control &&
		a.publicWeb.ExternalVoice && a.publicWeb.TURNIssuer != nil
	if !tailnetControl && !publicControl {
		return errors.New("external voice requires an authenticated browser route with control enabled")
	}
	if cfg.MediaInterface == "" && !publicControl {
		return errors.New("external voice without Tailnet media requires public TURN relay mode")
	}
	if a.remoteMedia != nil || a.remoteIncomingAnswer || a.remoteRescueHangup {
		return errors.New("external SIP voice is mutually exclusive with the frozen local QPCMV voice path")
	}
	copy := *cfg
	copy.MediaLocalCIDRs = append([]string(nil), cfg.MediaLocalCIDRs...)
	copy.MediaRemoteCIDRs = append([]string(nil), cfg.MediaRemoteCIDRs...)
	a.sipVoiceStartup = &copy
	return nil
}

func (a *app) requireLegacyModuleVoicePath() error {
	// sipVoiceStartup is installed before any call poller starts and remains
	// immutable for the process lifetime. Keep the frozen QPCMV/AT route blocked
	// even while the external provider is reconnecting or failed to start.
	if a != nil && a.sipVoiceStartup != nil {
		return errLegacyVoiceDisabledByExternalSIP
	}
	return nil
}

func (a *app) startExternalVoice(ctx context.Context) error {
	if a.sipVoiceStartup == nil {
		return nil
	}
	if a.sipVoice != nil {
		return errors.New("external voice is already started")
	}
	cfg := *a.sipVoiceStartup
	recoveryStore, err := openExternalVoiceRecoveryStore(cfg.RecoveryStorePath)
	if err != nil {
		return errExternalVoiceUnavailable
	}
	recoveryJournal, err := newExternalVoiceMutationJournal(recoveryStore, cfg.GatewayID, nil)
	if err != nil {
		_ = recoveryStore.Close()
		return err
	}
	network := remotevoice.Config{
		AllowedLocalCIDRs:  append([]string(nil), cfg.MediaLocalCIDRs...),
		AllowedRemoteCIDRs: append([]string(nil), cfg.MediaRemoteCIDRs...),
		UDPMin:             cfg.MediaUDPMin, UDPMax: cfg.MediaUDPMax,
	}
	if cfg.MediaInterface != "" {
		network.AllowedInterfaces = []string{cfg.MediaInterface}
	}
	newAsteriskConfig := func(username, password string, requireCleanStartup bool) (asterisk.Config, func(), error) {
		secret, secretErr := recoveryStore.ProviderSecret()
		if secretErr != nil {
			return asterisk.Config{}, func() {}, errExternalVoiceUnavailable
		}
		secretBytes := append([]byte(nil), secret[:]...)
		zeroExternalVoiceRecoverySecret(&secret)
		cleanup := func() { zeroExternalVoiceRecoveryBytes(secretBytes) }
		return asterisk.Config{
			GatewayID: cfg.GatewayID, BaseURL: cfg.ARIURL,
			Application: cfg.ARIApplication, IncomingArgument: cfg.ARIArgument,
			IncomingContext: cfg.ARIContext, IncomingEndpoint: cfg.ARIEndpoint,
			OutgoingEndpoint: cfg.ARIOutgoingEndpoint,
			Username:         username, Password: password,
			ExpectedVersion: cfg.ARIExpectedVersion, IncomingPolicyID: cfg.ARIIncomingPolicyID,
			ExpectedEntityID:    cfg.ARIExpectedEntityID,
			RequireCleanStartup: requireCleanStartup,
			RecoverySecret:      secretBytes,
		}, cleanup, nil
	}
	supervisor, err := newSIPVoiceSupervisor(ctx, sipVoiceSupervisorConfig{
		Runtime: sipVoiceRuntimeConfig{
			GatewayID: cfg.GatewayID, Network: network,
			Journal: recoveryJournal, Evidence: recoveryJournal.Evidence,
			AllowAnswer: cfg.AllowAnswer, AllowReject: cfg.AllowReject, AllowDTMF: cfg.AllowDTMF,
			AllowEnd:         cfg.AllowEnd,
			AllowDial:        cfg.AllowDial,
			Freshness:        externalVoiceFreshness,
			IncomingNotifier: a.notifyPublicWebExternalIncoming,
		},
		AdapterFactory: func(factoryContext context.Context) (sipgateway.Adapter, error) {
			password, readErr := readExternalVoicePassword(cfg.ARIPasswordFile)
			if readErr != nil {
				return nil, errExternalVoiceUnavailable
			}
			adapterConfig, cleanup, configErr := newAsteriskConfig(cfg.ARIUsername, password, cfg.AllowAnswer || cfg.AllowEnd)
			if configErr != nil {
				return nil, configErr
			}
			defer cleanup()
			return asterisk.New(factoryContext, adapterConfig)
		},
		RecoveryInspectorFactory: func(factoryContext context.Context) (sipgateway.RecoveryInspector, error) {
			password, readErr := readExternalVoicePassword(cfg.ARIRecoveryPasswordFile)
			if readErr != nil {
				return nil, errExternalVoiceUnavailable
			}
			inspectorConfig, cleanup, configErr := newAsteriskConfig(cfg.ARIRecoveryUsername, password, false)
			if configErr != nil {
				return nil, configErr
			}
			defer cleanup()
			return asterisk.NewRecoveryInspector(factoryContext, inspectorConfig)
		},
		RecoveryStore: recoveryStore, RecoveryJournal: recoveryJournal,
	})
	if err != nil {
		_ = recoveryJournal.Close()
		_ = recoveryStore.Close()
		return err
	}
	a.sipVoice = supervisor
	return nil
}

func readExternalVoicePassword(path string) (string, error) {
	data, err := readExternalVoicePasswordFile(path, externalVoiceSecretMaxBytes)
	if err != nil {
		return "", err
	}
	defer func() {
		for index := range data {
			data[index] = 0
		}
	}()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if len(data) == 0 || len(data) > 1024 || strings.ContainsAny(string(data), "\x00\r\n") {
		return "", errors.New("Asterisk password file must contain exactly one non-empty line")
	}
	return string(data), nil
}
