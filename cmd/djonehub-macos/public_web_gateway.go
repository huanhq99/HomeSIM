package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iniwex5/vohive/internal/publicedge"
	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/turnauth"
)

const (
	defaultPublicWebListen  = "127.0.0.1:7578"
	maxPublicWebAllowlist   = 16 << 10
	maxPublicWebTURNSecret  = 4 << 10
	defaultPublicWebTURNUDP = 3478
	defaultPublicWebTURNTLS = 443
	defaultPublicWebTURNTTL = 5 * time.Minute
)

type publicWebFlagConfig struct {
	Listen            string
	Host              string
	TeamDomain        string
	Audience          string
	AllowedEmailsFile string
	Control           bool
	ExternalVoice     bool
	DirectVoice       bool
	TURNHost          string
	TURNSecretFile    string
	TURNUDPPort       uint
	TURNTLSPort       uint
	TURNCredentialTTL time.Duration
	RecordingsDir     string
	Push              publicWebPushFlagConfig
	HTTPClient        *http.Client
	TURNResolveIPv4   func(context.Context, string) (string, error)
}

type publicWebStartupConfig struct {
	Listen          string
	Host            string
	Control         bool
	Verifier        *publicedge.AccessVerifier
	ExternalVoice   bool
	DirectVoice     bool
	TURNIssuer      *turnauth.Issuer
	TURNHost        string
	TURNResolveIPv4 func(context.Context, string) (string, error)
	RecordingsDir   string
	Push            *publicWebPushStartupConfig
}

func parsePublicWebStartupConfig(flags publicWebFlagConfig) (*publicWebStartupConfig, error) {
	configured := strings.TrimSpace(flags.Host) != "" ||
		strings.TrimSpace(flags.TeamDomain) != "" ||
		strings.TrimSpace(flags.Audience) != "" ||
		strings.TrimSpace(flags.AllowedEmailsFile) != "" || flags.Control ||
		flags.ExternalVoice || flags.DirectVoice || strings.TrimSpace(flags.TURNHost) != "" ||
		strings.TrimSpace(flags.TURNSecretFile) != "" || flags.TURNUDPPort != 0 ||
		flags.TURNTLSPort != 0 || flags.TURNCredentialTTL != 0 || flags.Push.Enabled ||
		strings.TrimSpace(flags.RecordingsDir) != "" ||
		strings.TrimSpace(flags.Push.PrivateKeyFile) != "" ||
		strings.TrimSpace(flags.Push.Subject) != "" ||
		strings.TrimSpace(flags.Push.SubscriptionsFile) != ""
	if !configured {
		return nil, nil
	}

	listen := strings.TrimSpace(flags.Listen)
	if !isLoopbackListenAddress(listen) {
		return nil, errors.New("public web listener must use an explicit loopback address")
	}
	host := strings.TrimSpace(flags.Host)
	if host == "" || host != canonicalRemoteHost(host) || strings.ContainsAny(host, "/:@[]") {
		return nil, errors.New("public web host must be one exact canonical DNS host")
	}
	teamDomain := strings.TrimSpace(flags.TeamDomain)
	audience := strings.TrimSpace(flags.Audience)
	allowlistPath := strings.TrimSpace(flags.AllowedEmailsFile)
	if teamDomain == "" || audience == "" || allowlistPath == "" {
		return nil, errors.New("public web access requires team domain, audience, and email allowlist file")
	}
	if !filepath.IsAbs(allowlistPath) {
		return nil, errors.New("public web email allowlist path must be absolute")
	}
	emails, err := readPublicWebAllowedEmails(allowlistPath)
	if err != nil {
		return nil, fmt.Errorf("public web email allowlist: %w", err)
	}
	verifier, err := publicedge.NewAccessVerifier(publicedge.AccessConfig{
		TeamDomain:    teamDomain,
		Audience:      audience,
		AllowedEmails: emails,
		HTTPClient:    flags.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("public web Access verifier: %w", err)
	}
	startup := &publicWebStartupConfig{
		Listen: listen, Host: host, Control: flags.Control, Verifier: verifier,
	}
	if flags.DirectVoice {
		recordingsDir := strings.TrimSpace(flags.RecordingsDir)
		if recordingsDir == "" {
			recordingsDir = defaultPublicWebRecordingsRoot()
		}
		if recordingsDir == "" || !filepath.IsAbs(recordingsDir) || filepath.Clean(recordingsDir) != recordingsDir {
			return nil, errors.New("public web recordings directory must be one clean absolute path")
		}
		startup.RecordingsDir = recordingsDir
	}
	turnConfigured := strings.TrimSpace(flags.TURNHost) != "" ||
		strings.TrimSpace(flags.TURNSecretFile) != "" || flags.TURNUDPPort != 0 ||
		flags.TURNTLSPort != 0 || flags.TURNCredentialTTL != 0
	if flags.ExternalVoice && flags.DirectVoice {
		return nil, errors.New("public web external and direct voice are mutually exclusive")
	}
	voiceEnabled := flags.ExternalVoice || flags.DirectVoice
	if !voiceEnabled {
		if turnConfigured {
			return nil, errors.New("public web TURN configuration requires an explicit voice mode")
		}
		if flags.Push.Enabled || strings.TrimSpace(flags.Push.PrivateKeyFile) != "" ||
			strings.TrimSpace(flags.Push.Subject) != "" ||
			strings.TrimSpace(flags.Push.SubscriptionsFile) != "" {
			return nil, errors.New("public web push requires an explicit public voice mode")
		}
		return startup, nil
	}
	if !flags.Control {
		return nil, errors.New("public web voice requires public web control")
	}
	turnHost := strings.TrimSpace(flags.TURNHost)
	turnSecretPath := strings.TrimSpace(flags.TURNSecretFile)
	if turnHost == "" || turnSecretPath == "" || !filepath.IsAbs(turnSecretPath) {
		return nil, errors.New("public web voice requires exact TURN host and absolute secret file")
	}
	udpPort := flags.TURNUDPPort
	if udpPort == 0 {
		udpPort = defaultPublicWebTURNUDP
	}
	tlsPort := flags.TURNTLSPort
	if tlsPort == 0 {
		tlsPort = defaultPublicWebTURNTLS
	}
	ttl := flags.TURNCredentialTTL
	if ttl == 0 {
		ttl = defaultPublicWebTURNTTL
	}
	if udpPort > 65535 || tlsPort > 65535 {
		return nil, errors.New("public web TURN ports are invalid")
	}
	secret, err := readPublicWebTURNSecret(turnSecretPath)
	if err != nil {
		return nil, fmt.Errorf("public web TURN secret: %w", err)
	}
	defer func() {
		for index := range secret {
			secret[index] = 0
		}
	}()
	issuer, err := turnauth.New(turnauth.Config{
		Host: turnHost, UDPPort: uint16(udpPort), TLSPort: uint16(tlsPort),
		TTL: ttl, Secret: secret,
	})
	if err != nil {
		return nil, fmt.Errorf("public web TURN issuer: %w", err)
	}
	startup.ExternalVoice = flags.ExternalVoice
	startup.DirectVoice = flags.DirectVoice
	startup.TURNIssuer = issuer
	startup.TURNHost = strings.ToLower(strings.TrimSuffix(turnHost, "."))
	startup.TURNResolveIPv4 = flags.TURNResolveIPv4
	if startup.TURNResolveIPv4 == nil {
		startup.TURNResolveIPv4 = resolvePublicWebTURNIPv4
	}
	push, err := parsePublicWebPushStartupConfig(flags.Push, emails)
	if err != nil {
		_ = issuer.Close()
		return nil, err
	}
	startup.Push = push
	return startup, nil
}

func readPublicWebTURNSecret(path string) ([]byte, error) {
	data, err := readExternalVoicePasswordFile(path, maxPublicWebTURNSecret)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data[len(data)-1] = 0
		data = data[:len(data)-1]
	}
	if len(data) == 0 || strings.TrimSpace(string(data)) != string(data) ||
		strings.ContainsAny(string(data), "\x00\r\n") {
		for index := range data {
			data[index] = 0
		}
		return nil, errors.New("secret must be one non-empty exact line")
	}
	return data, nil
}

func readPublicWebAllowedEmails(path string) ([]string, error) {
	data, err := readExternalVoicePasswordFile(path, maxPublicWebAllowlist)
	if err != nil {
		return nil, err
	}
	defer func() {
		for index := range data {
			data[index] = 0
		}
	}()
	if len(data) == 0 || !utf8.Valid(data) || strings.ContainsRune(string(data), '\r') {
		return nil, errors.New("allowlist must be non-empty UTF-8 with LF line endings")
	}
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, errors.New("allowlist must contain at least one email")
	}
	for _, email := range lines {
		if email == "" || email != strings.TrimSpace(email) {
			return nil, errors.New("allowlist must contain one exact email per line")
		}
	}
	return lines, nil
}

func (a *app) configurePublicWebAccess(config *publicWebStartupConfig, ledgerPath ...string) error {
	if config == nil {
		if a.publicWebPush != nil {
			_ = a.publicWebPush.Close()
			a.publicWebPush = nil
		}
		a.publicWeb = nil
		return nil
	}
	if config.Verifier == nil || config.Host == "" || !isLoopbackListenAddress(config.Listen) {
		return errors.New("public web startup configuration is incomplete")
	}
	if config.ExternalVoice && config.DirectVoice {
		return errors.New("public web voice modes are mutually exclusive")
	}
	voiceEnabled := config.ExternalVoice || config.DirectVoice
	if voiceEnabled != (config.TURNIssuer != nil) || (voiceEnabled && !config.Control) {
		return errors.New("public web voice configuration is incomplete")
	}
	if config.Push != nil && !voiceEnabled {
		return errors.New("public web push requires public voice")
	}
	if config.DirectVoice && (a.remoteMedia != nil || a.sipVoice != nil || a.sipVoiceStartup != nil) {
		return errors.New("public direct voice cannot share an existing media or external voice runtime")
	}
	if a.remoteCSRF == nil {
		a.remoteCSRF = newRemoteCSRFStore()
	}
	if a.remoteLedger == nil {
		path := ""
		if len(ledgerPath) > 0 {
			path = strings.TrimSpace(ledgerPath[0])
		}
		if path == "" {
			var err error
			path, err = remoteOperationLedgerPath()
			if err != nil {
				return fmt.Errorf("public web operation ledger path: %w", err)
			}
		}
		ledger, err := openRemoteOperationLedger(path)
		if err != nil {
			return fmt.Errorf("public web operation ledger: %w", err)
		}
		a.remoteLedger = ledger
	}
	if config.DirectVoice {
		configBase, err := os.UserConfigDir()
		if err != nil || strings.TrimSpace(configBase) == "" {
			return errors.New("resolve public direct voice runtime directory")
		}
		remoteRoot := filepath.Join(configBase, "MacCellular", "remote")
		manager, err := newRemoteMediaManager(remoteMediaRuntimeConfig{
			IssueTURN: func() (*remotevoice.TURNRelayConfig, error) {
				issueContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				relay, relayErr := issuePublicWebServerRelay(issueContext, config)
				if relayErr != nil {
					return nil, relayErr
				}
				return &remotevoice.TURNRelayConfig{
					URLs: append([]string(nil), relay.URLs...), Username: relay.Username,
					Password: relay.Password, CredentialType: remotevoice.TURNCredentialTypePassword,
				}, nil
			},
			RootDir: filepath.Join(remoteRoot, "pcm"), ControlPath: filepath.Join(remoteRoot, "media-control.sock"),
			OfferTTL: remoteMediaDefaultOfferTTL,
		})
		if err != nil {
			return fmt.Errorf("public direct voice media: %w", err)
		}
		a.remoteMedia = manager
		a.remoteIncomingAnswer = true
		a.remoteRescueHangup = true
	}
	if config.Push != nil {
		manager, err := newPublicWebPushManager(config.Push)
		if err != nil {
			if config.DirectVoice && a.remoteMedia != nil {
				_ = a.remoteMedia.Close()
				a.remoteMedia = nil
				a.remoteIncomingAnswer = false
				a.remoteRescueHangup = false
			}
			return fmt.Errorf("public web push: %w", err)
		}
		a.publicWebPush = manager
	}
	a.publicWeb = config
	return nil
}

func issuePublicWebServerRelay(ctx context.Context, config *publicWebStartupConfig) (turnauth.ServerRelayCredential, error) {
	if config == nil || config.TURNIssuer == nil {
		return turnauth.ServerRelayCredential{}, errors.New("public web TURN relay is unavailable")
	}
	credentials, err := config.TURNIssuer.Issue()
	if err != nil {
		return turnauth.ServerRelayCredential{}, errors.New("public web TURN relay is unavailable")
	}
	if config.TURNResolveIPv4 == nil || config.TURNHost == "" {
		return credentials.ServerRelay()
	}
	address, err := config.TURNResolveIPv4(ctx, config.TURNHost)
	if err != nil {
		return turnauth.ServerRelayCredential{}, errors.New("public web TURN relay is unavailable")
	}
	relay, err := credentials.ServerRelayWithResolvedIPv4(address)
	if err != nil {
		return turnauth.ServerRelayCredential{}, errors.New("public web TURN relay is unavailable")
	}
	return relay, nil
}

func issuePublicWebBrowserTURN(ctx context.Context, config *publicWebStartupConfig) ([]byte, error) {
	if config == nil || config.TURNIssuer == nil {
		return nil, errors.New("public web TURN relay is unavailable")
	}
	credentials, err := config.TURNIssuer.Issue()
	if err != nil {
		return nil, errors.New("public web TURN relay is unavailable")
	}
	if config.TURNResolveIPv4 == nil || config.TURNHost == "" {
		return credentials.EncodeJSON()
	}
	address, err := config.TURNResolveIPv4(ctx, config.TURNHost)
	if err != nil {
		return nil, errors.New("public web TURN relay is unavailable")
	}
	body, err := credentials.EncodeJSONWithResolvedIPv4(address)
	if err != nil {
		return nil, errors.New("public web TURN relay is unavailable")
	}
	return body, nil
}

func resolvePublicWebTURNIPv4(ctx context.Context, host string) (string, error) {
	if ctx == nil || host == "" || host != strings.TrimSpace(host) {
		return "", errors.New("public web TURN address is unavailable")
	}
	addresses, err := newPublicWebPushResolver().LookupIP(ctx, "ip4", host)
	if err != nil || len(addresses) == 0 {
		return "", errors.New("public web TURN address is unavailable")
	}
	selected := ""
	for _, address := range addresses {
		if address.To4() == nil || !publicWebPushIPIsPublic(address) {
			return "", errors.New("public web TURN address is unavailable")
		}
		candidate := address.To4().String()
		if selected == "" || candidate < selected {
			selected = candidate
		}
	}
	if selected == "" || net.ParseIP(selected) == nil {
		return "", errors.New("public web TURN address is unavailable")
	}
	return selected, nil
}

func (a *app) publicWebRoutes() http.Handler {
	if a.publicWeb == nil {
		return http.NotFoundHandler()
	}
	// Keep the external voice paths registered while the supervisor is
	// reconnecting so an already-running listener does not change its route
	// shape.  Capability advertisement and mutations still require the
	// supervisor to report a connected runtime below.
	externalVoice := a.publicWebExternalVoiceConfigured()
	directVoice := a.publicWebDirectVoiceEnabled()
	// Register configured push paths even while the SIP supervisor is
	// reconnecting.  The handlers and capability grant remain readiness-gated,
	// so a later SIP recovery does not require rebuilding the HTTP handler.
	pushEnabled := a.publicWebPushConfigured()
	// Reuse only the asset/session/SMS subset here. Direct voice adds its exact
	// media-bound v1 answer/dial/hangup/DTMF mutations below; reject, native,
	// modem and administrator routes remain outside this listener.
	root := a.remoteBrowserRoutes(true)
	if externalVoice || directVoice {
		mux := http.NewServeMux()
		mux.Handle("/", root)
		mux.HandleFunc("GET "+externalVoiceICECredentialsPath, a.remoteVoiceV2ICECredentials)
		if externalVoice {
			mux.HandleFunc("GET "+remoteVoiceV2SnapshotPath, a.remoteVoiceV2Snapshot)
			mux.HandleFunc("POST "+remoteVoiceV2OfferPath, a.remoteVoiceV2Offer)
			mux.HandleFunc("POST "+remoteVoiceV2AnswerPath, a.remoteVoiceV2Answer)
			mux.HandleFunc("POST "+remoteVoiceV2RejectPath, a.remoteVoiceV2Reject)
			mux.HandleFunc("POST "+remoteVoiceV2DTMFPath, a.remoteVoiceV2DTMF)
			mux.HandleFunc("POST "+remoteVoiceV2EndPath, a.remoteVoiceV2End)
			mux.HandleFunc("POST "+remoteVoiceV2DialPath, a.remoteVoiceV2Dial)
			mux.HandleFunc("POST "+remoteVoiceV2ReconcilePath, a.remoteVoiceV2Reconcile)
		} else {
			mux.HandleFunc("POST /api/remote/v1/media/offers", a.remoteMediaOffer)
			mux.HandleFunc("POST /api/remote/v1/calls/dial", a.remoteDialCall)
			mux.HandleFunc("POST /api/remote/v1/calls/answer", a.remoteAnswerCall)
			mux.HandleFunc("POST /api/remote/v1/calls/hangup", a.remoteHangupCall)
			mux.HandleFunc("POST /api/remote/v1/calls/dtmf", a.remoteDTMFCall)
			mux.HandleFunc("POST "+publicWebRecordingUploadPath, a.publicWebSaveRecording)
			mux.HandleFunc("GET "+publicWebRecordingUploadPath, a.publicWebListRecordings)
			mux.HandleFunc("GET "+publicWebRecordingUploadPath+"/{recordingID}/audio", a.publicWebPlayRecording)
		}
		if pushEnabled {
			mux.HandleFunc("GET "+publicWebPushConfigPath, a.publicWebPushConfig)
			mux.HandleFunc("POST "+publicWebPushSubscriptionsPath, a.publicWebPushSubscribe)
			mux.HandleFunc("DELETE "+publicWebPushSubscriptionsPath+"/{opaqueID}", a.publicWebPushDelete)
		}
		root = mux
	}
	return a.remoteBrowserSecurityHeaders(root, remoteBrowserSecurityPolicy{
		Host:                a.publicWeb.Host,
		Control:             a.publicWeb.Control,
		Transport:           "cloudflare-access",
		SMSOnly:             !a.publicWebExternalVoiceEnabled() && !directVoice,
		NotFoundUnknownPath: true,
		PathAllowed: func(path string) bool {
			return isPublicWebGatewayPath(path, externalVoice, directVoice, pushEnabled)
		},
		Authorize:           a.authorizePublicWebRequest,
		AuthorizationStatus: publicWebAuthorizationStatus,
	})
}

func (a *app) publicWebExternalVoiceConfigured() bool {
	return a != nil && a.publicWeb != nil && a.publicWeb.ExternalVoice &&
		a.publicWeb.Control && a.publicWeb.TURNIssuer != nil && a.sipVoice != nil
}

func (a *app) publicWebExternalVoiceEnabled() bool {
	if !a.publicWebExternalVoiceConfigured() {
		return false
	}
	status := a.sipVoice.Status("")
	return status.Enabled && status.Health == "connected" && !status.RecoveryRequired
}

func (a *app) publicWebDirectVoiceEnabled() bool {
	return a != nil && a.publicWeb != nil && a.publicWeb.DirectVoice &&
		a.publicWeb.Control && a.publicWeb.TURNIssuer != nil && a.sipVoice == nil &&
		a.remoteMedia != nil && a.remoteMedia.cfg.networkMode() == remoteMediaNetworkPublicTURN &&
		a.remoteIncomingAnswer && a.remoteRescueHangup
}

func (a *app) publicWebVoiceEnabled() bool {
	return a.publicWebExternalVoiceEnabled() || a.publicWebDirectVoiceEnabled()
}

func isPublicWebGatewayPath(path string, externalVoice, directVoice, pushEnabled bool) bool {
	if path == "/" || path == "/remote" || strings.HasPrefix(path, "/remote/") {
		return true
	}
	switch path {
	case "/api/remote/v1/session",
		"/api/remote/v1/snapshot",
		"/api/remote/v1/sms/sync",
		"/api/remote/v1/sms/send",
		"/api/remote/v1/sms/refresh":
		return true
	default:
		if pushEnabled && isPublicWebPushPath(path) {
			return true
		}
		if directVoice {
			if path == publicWebRecordingUploadPath || strings.HasPrefix(path, publicWebRecordingUploadPath+"/") {
				return true
			}
			switch path {
			case externalVoiceICECredentialsPath, "/api/remote/v1/media/offers",
				"/api/remote/v1/calls/dial", "/api/remote/v1/calls/answer",
				"/api/remote/v1/calls/hangup", "/api/remote/v1/calls/dtmf":
				return true
			default:
				return false
			}
		}
		if externalVoice {
			switch path {
			case remoteVoiceV2SnapshotPath, externalVoiceICECredentialsPath,
				remoteVoiceV2OfferPath, remoteVoiceV2AnswerPath,
				remoteVoiceV2RejectPath, remoteVoiceV2DTMFPath,
				remoteVoiceV2EndPath, remoteVoiceV2DialPath, remoteVoiceV2ReconcilePath:
				return true
			}
		}
		return false
	}
}

func (a *app) authorizePublicWebRequest(request *http.Request) (remoteAuthorization, error) {
	if a.publicWeb == nil || a.publicWeb.Verifier == nil || request == nil ||
		request.Host != a.publicWeb.Host {
		return remoteAuthorization{}, publicedge.ErrAccessUnauthorized
	}
	identity, err := a.publicWeb.Verifier.VerifyRequest(request)
	if err != nil {
		if request.Header.Get(publicedge.AccessAssertionHeader) != "" {
			log.Printf("Public web Access verification rejected: code=%s", publicedge.AccessFailureCode(err))
		}
		return remoteAuthorization{}, err
	}
	actions := map[string]bool{"status.read": true, "sms.read": true}
	if a.publicWeb.Control {
		actions["sms.send"] = true
	}
	if a.publicWebVoiceEnabled() {
		actions["calls.read"] = true
		if a.publicWeb.Control {
			actions["calls.media"] = true
			actions["calls.control"] = true
			actions["calls.hangup"] = true
		}
	}
	if a.publicWebPushEnabled() {
		actions["push.manage"] = true
	}
	return remoteAuthorization{Identity: identity.Email, Actions: actions}, nil
}

func publicWebAuthorizationStatus(err error) int {
	if errors.Is(err, publicedge.ErrAccessUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}
