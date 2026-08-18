package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/damonto/euicc-go/driver"
	"github.com/iniwex5/vohive/internal/backend"
	"github.com/iniwex5/vohive/internal/config"
	"github.com/iniwex5/vohive/internal/esim"
	"github.com/iniwex5/vohive/internal/modem"
	"github.com/iniwex5/vohive/pkg/smscodec"
	"go.bug.st/serial"
)

//go:embed web/* remote/*
var webAssets embed.FS

type receivedSMS struct {
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	Code      string    `json:"code,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type profileNote struct {
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

type phonebookProbeResult struct {
	StorageSupported bool              `json:"storage_supported"`
	StorageSelected  bool              `json:"storage_selected"`
	ReadSupported    bool              `json:"read_supported"`
	WriteSupported   bool              `json:"write_supported"`
	StorageStatus    string            `json:"storage_status"`
	Responses        map[string]string `json:"responses"`
}

type moduleProfileNote struct {
	Index int    `json:"index"`
	ICCID string `json:"iccid"`
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

type modulePhonebookEntry struct {
	Index  int
	Number string
	Text   string
}

type app struct {
	modem             *modem.Manager
	esimMu            sync.RWMutex
	esim              *esim.Manager
	esimSwitchAllowed bool
	usbAT             *usbAT
	deviceStateMu     sync.RWMutex
	port              string
	demo              bool
	smsOnlyRuntime    bool
	// This QDC507 firmware routes calls through its persistent UAC profile and
	// does not implement a usable QPCMV selector.
	implicitUACVoice bool
	webConsole       bool
	discoveryError   string
	usbDevice        *usbDeviceStatus
	// Optional discovery seam for deterministic, hardware-free race tests.
	discoverUSBDevice func() *usbDeviceStatus
	usbATBackoffUntil time.Time
	usbATBackoffErr   string
	// Non-zero only while a persistent module setup transaction is in flight.
	// Every background reopen must then bind to this exact physical USB port.
	usbATRequiredLocation uint32
	// Serializes every operation which can persistently alter modem/eUICC
	// state or reboot the module. Module setup holds it for the full transaction.
	moduleMutationMu sync.Mutex
	// Test seam for fault-injecting one-handle AT validation/write sessions.
	usbATSessionOverride func(uint32, func(usbATCommandFunc) error) error
	// Test seam for operations which must prove the exact lifecycle pointer and
	// physical identity of the handle locked for the entire AT transaction.
	usbATPinnedSessionOverride func(uint32, func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error) error

	smsMu          sync.RWMutex
	smsOperationMu sync.Mutex
	sms            []receivedSMS
	smsSendMu      sync.Mutex
	smsReassembler *smscodec.Reassembler
	smsStore       *smsStore
	smsStoreError  string
	// Optional hardware-free seam for durable send-state tests.
	sendTextSMSOverride func(phone, message string) (int, error)

	smsPollInterval  time.Duration
	smsAutoCleanupME bool
	smsLastPoll      time.Time
	smsLastPollError string

	callMu            sync.RWMutex
	activeCall        *callRecord
	callHistory       []callRecord
	callHistoryPath   string
	callPollInterval  time.Duration
	callLastPoll      time.Time
	callLastPollError string
	callConfigured    bool
	lastAnswerAt      time.Time
	callNotifier      func(callRecord)
	callGeneration    uint64
	callTopologyKey   string
	callTopologyKnown bool
	callMediaEligible bool
	// Optional test seam for observing topology-triggered route teardown
	// without touching a physical USB module.
	callMediaRouteStop func(uint64) error
	audio              *audioRouter
	audioManualSet     bool
	audioManualOn      bool
	lastAudioHealthLog time.Time
	// The native notifier registers this before a call.  When present, the
	// backend owns only module control and call state; MaVo's Swift service owns
	// every host-audio callback and media loop.
	swiftAudioHost bool

	moduleVoiceMu       sync.Mutex
	moduleVoiceOpMu     sync.Mutex
	moduleVoiceReady    bool
	moduleVoiceLast     time.Time
	moduleVoiceErr      string
	moduleVoiceDetail   string
	moduleVoicePhase    string
	moduleVoiceLocation uint32
	// Hash of the physical module identity which owns an armed/ready route.
	// Location alone is insufficient because a replacement can enumerate on the
	// same USB port between arm and adopt/cleanup.
	moduleVoiceIdentityHash string
	moduleVoiceIntent       directQPCMVCallIntent
	// Non-zero only for a route started for one exact CLCC generation. It lets
	// delayed cleanup prove that it still owns the route before stopping it.
	moduleVoiceCallGeneration uint64
	// Records the call topology generation for which the persistent module-side
	// D4/UAC helper was last restarted. A warm helper is cheap between calls, but
	// reusing it for a new call can leave CoreAudio receiving valid all-zero UAC
	// frames after a rapid hangup/redial.
	moduleVoicePrewarmGeneration uint64
	// Optional hardware-free seam for proving that an outgoing dial performs
	// the same pre-ring module audio preparation as incoming answer.
	outgoingVoicePrewarm func(uint64) error
	moduleVoiceRefresh   func(uint64) error

	moduleSetupMu sync.RWMutex
	moduleSetup   moduleSetupStatus

	gpsMu          sync.RWMutex
	gpsEnabled     bool
	gpsLastFix     *gpsFix
	gpsLastChecked time.Time
	gpsLastError   string

	profileNotesMu     sync.Mutex
	profileNotes       map[string]profileNote
	profileNotesLoaded bool
	profileNotesPath   string

	moduleNotesMu sync.Mutex

	trafficMu        sync.Mutex
	trafficBaselines map[string]networkByteCounters

	networkPolicyMu     sync.Mutex
	networkPolicyLoaded bool
	force4GOff          bool
	disabled4GServices  []string
	networkPolicyPath   string

	networkRepairMu    sync.Mutex
	autoNetworkRepair  bool
	autoSignalRecovery bool

	usbATOpenMu      sync.Mutex
	recoveryMu       sync.Mutex
	lostSignalCount  int
	lastModemReboot  time.Time
	lastNetworkCheck time.Time
	// Test seam used by persistent USB-mode API contention tests.
	moduleMutationLockedHook func()

	remoteAccess remoteAccessConfig
	remoteLedger *remoteOperationLedger
	remoteCSRF   *remoteCSRFStore
	remoteCallMu sync.Mutex
	remoteListen string
	// publicWeb is a third, independent loopback-only browser listener for a
	// dedicated Cloudflare Tunnel. Direct voice adds only incoming media,
	// answer, and rescue-hangup routes; native, dial, reject, DTMF, USB, AT,
	// ARI and local-administrator routes remain absent.
	publicWeb *publicWebStartupConfig
	// publicWebPush is nil unless an explicit public voice profile also loads a
	// private local VAPID key and durable per-principal subscription store.
	publicWebPush *publicWebPushManager
	// Native clients use a separately enrolled P-256 device key. These fields
	// are nil unless the authenticated remote gateway and its private device
	// store both initialized successfully.
	nativeDeviceStore *nativeDeviceStore
	nativeEnrollment  *nativeEnrollmentManager
	nativeAuth        *nativeAuthManager
	// remoteIncomingAnswer is a separate operator opt-in. Shipping the media
	// stack must not silently turn an existing SMS/status gateway into a live
	// cellular call controller after an upgrade.
	remoteIncomingAnswer bool
	// remoteRescueHangup separately enables a one-shot, media-proven stop-loss
	// ATH for the exact incoming call previously answered through this gateway.
	remoteRescueHangup bool
	// remoteMedia is nil unless the operator explicitly configures an exact
	// Tailscale interface, local address range, remote peer address and bounded
	// UDP range. It owns only in-memory media leases and local sockets.
	remoteMedia *remoteMediaManager
	// sipVoice is the independent external VoLTE-to-SIP/WebRTC runtime. It is
	// never backed by the local QPCMV/CLCC/USB call path above and remains nil
	// unless the operator explicitly configures a validated external provider.
	sipVoice        externalVoiceService
	sipVoiceStartup *externalVoiceStartupConfig
	// publicEdge is the optional, outbound-only connection to the maintainer's public
	// control edge. It publishes coarse read-only state and never exposes a
	// local listener, arbitrary proxy, SMS content, modem command, or voice
	// mutation.
	publicEdge        *publicEdgeConnector
	publicEdgeStartup *publicEdgeStartupConfig
}

type modemRuntimeStartupPlan struct {
	SMS         bool
	CallPoll    bool
	GPSPoll     bool
	GPSReadback bool
}

var errUSBATOnlyWithSerialPort = errors.New("-usb-at-only cannot be combined with -port")

func validateModemRuntimeMode(smsOnly, phoneRelay bool, publicWeb *publicWebStartupConfig) error {
	if smsOnly && phoneRelay {
		return errors.New("-sms-only-runtime and -phone-relay-runtime are mutually exclusive")
	}
	if smsOnly && publicWeb != nil && publicWeb.DirectVoice {
		return errors.New("public direct voice cannot be combined with -sms-only-runtime")
	}
	return nil
}

func shouldProbeSerialAT(explicitPort string, usbATOnly bool) (bool, error) {
	if usbATOnly && strings.TrimSpace(explicitPort) != "" {
		return false, errUSBATOnlyWithSerialPort
	}
	return strings.TrimSpace(explicitPort) == "" && !usbATOnly, nil
}

func planModemRuntimeStartup(smsOnly, phoneRelay bool) modemRuntimeStartupPlan {
	return modemRuntimeStartupPlan{
		SMS:         true,
		CallPoll:    !smsOnly,
		GPSPoll:     !smsOnly && !phoneRelay,
		GPSReadback: !smsOnly && !phoneRelay,
	}
}

func validateRemoteMediaControlStartup(instance *app, control *remoteMediaControlServer, startErr error) error {
	directVoice := instance != nil && instance.publicWeb != nil && instance.publicWeb.DirectVoice
	if !directVoice {
		return nil
	}
	if startErr != nil || control == nil {
		return errors.New("public direct voice requires the local media control socket")
	}
	return nil
}

type usbInterfaceStatus struct {
	Number    int `json:"number"`
	Class     int `json:"class"`
	Subclass  int `json:"subclass"`
	Protocol  int `json:"protocol"`
	Endpoints int `json:"endpoints"`
}

type usbDeviceStatus struct {
	Product    string               `json:"product"`
	Vendor     string               `json:"vendor"`
	VendorID   string               `json:"vendor_id"`
	ProductID  string               `json:"product_id"`
	LocationID string               `json:"location_id"`
	Speed      string               `json:"speed"`
	Mode       string               `json:"mode"`
	Interfaces []usbInterfaceStatus `json:"interfaces"`
}

type networkDiagnostic struct {
	USBNetMode        string            `json:"usbnet_mode"`
	USBCfg            string            `json:"usbcfg"`
	PDPContexts       []pdpContext      `json:"pdp_contexts"`
	ActiveContexts    []int             `json:"active_contexts"`
	PDPAddresses      []string          `json:"pdp_addresses"`
	MacInterfaces     []macNetInterface `json:"mac_interfaces"`
	DefaultRoute      macDefaultRoute   `json:"default_route"`
	USBNetworkPresent bool              `json:"usb_network_present"`
	USBDevice         *usbDeviceStatus  `json:"usb_device,omitempty"`
	Raw               map[string]string `json:"raw,omitempty"`
	Errors            map[string]string `json:"errors,omitempty"`
}

type pdpContext struct {
	ID  int    `json:"id"`
	PDN string `json:"pdn"`
	APN string `json:"apn"`
}

type macNetInterface struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	IPv4   string `json:"ipv4"`
	MAC    string `json:"mac,omitempty"`
	Kind   string `json:"kind"`
}

type macDefaultRoute struct {
	Interface string `json:"interface"`
	Gateway   string `json:"gateway"`
}

type networkByteCounters struct {
	RX uint64
	TX uint64
}

type networkTrafficSnapshot struct {
	Available    bool   `json:"available"`
	Interface    string `json:"interface,omitempty"`
	RXBytes      uint64 `json:"rx_bytes"`
	TXBytes      uint64 `json:"tx_bytes"`
	SessionRX    uint64 `json:"session_rx_bytes"`
	SessionTX    uint64 `json:"session_tx_bytes"`
	SessionTotal uint64 `json:"session_total_bytes"`
	SampledAtMS  int64  `json:"sampled_at_ms"`
	Error        string `json:"error,omitempty"`
}

type networkCheckResult struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
}

type cellularPolicyStatus struct {
	ForceOff bool     `json:"force_off"`
	Services []string `json:"services"`
}

func main() {
	platformCleanup := initPlatformRuntime()
	defer platformCleanup()

	var port string
	var listen string
	var demo bool
	var webConsole bool
	var autoNetworkRepair bool
	var autoSignalRecovery bool
	var remoteAllowedLogins string
	var remoteControl bool
	var remoteCapability string
	var remoteHost string
	var remoteListen string
	var remoteMediaInterface string
	var remoteMediaLocalCIDRs string
	var remoteMediaRemoteCIDRs string
	var remoteMediaUDPMin uint
	var remoteMediaUDPMax uint
	var remoteIncomingAnswer bool
	var remoteRescueHangup bool
	var publicWebFlags publicWebFlagConfig
	var externalVoiceFlags externalVoiceFlagConfig
	var publicEdgeFlags publicEdgeFlagConfig
	var smsStorePath string
	var callHistoryPath string
	var nativeEnrollmentOnce bool
	var nativeDeviceStorePath string
	var smsOnlyRuntime bool
	var phoneRelayRuntime bool
	var usbATOnly bool
	flag.StringVar(&port, "port", "", "AT serial port; auto-detected when omitted")
	flag.BoolVar(&usbATOnly, "usb-at-only", false, "skip serial AT discovery and use the DJI USB AT transport")
	flag.StringVar(&listen, "listen", "127.0.0.1:7576", "HTTP listen address")
	flag.BoolVar(&demo, "demo", false, "run the web UI with simulated modem data")
	flag.BoolVar(&smsOnlyRuntime, "sms-only-runtime", false, "run only SMS modem background work; disable call polling and GPS polling/readback")
	flag.BoolVar(&phoneRelayRuntime, "phone-relay-runtime", false, "run SMS and call polling for the remote phone relay; disable unrelated GPS polling/readback")
	flag.BoolVar(&webConsole, "web-console", false, "serve the embedded compatibility console")
	flag.BoolVar(&autoNetworkRepair, "auto-network-repair", false, "allow automatic DJI network-service DHCP repair")
	flag.BoolVar(&autoSignalRecovery, "auto-signal-recovery", false, "allow automatic radio recovery and bounded modem reboot")
	flag.StringVar(&remoteAllowedLogins, "remote-allowed-logins", "", "comma-separated Tailscale user logins allowed to open the remote gateway")
	flag.BoolVar(&remoteControl, "remote-control", false, "allow authenticated, capability-authorized remote clients to send SMS and control calls")
	flag.StringVar(&remoteCapability, "remote-capability", "", "required Tailscale app capability name (domain/path) for remote access")
	flag.StringVar(&remoteHost, "remote-host", "", "exact Tailscale Serve HTTPS host (for example device.tailnet.ts.net)")
	flag.StringVar(&remoteListen, "remote-listen", "127.0.0.1:7577", "dedicated loopback listen address for the Tailscale Serve remote gateway")
	flag.StringVar(&remoteMediaInterface, "remote-media-interface", "", "exact Tailscale interface for WebRTC media; blank keeps remote voice disabled")
	flag.StringVar(&remoteMediaLocalCIDRs, "remote-media-local-cidrs", "", "comma-separated canonical Tailscale CIDRs allowed for local ICE candidates")
	flag.StringVar(&remoteMediaRemoteCIDRs, "remote-media-remote-cidrs", "", "comma-separated exact remote Tailscale /32 or /128 peers")
	flag.UintVar(&remoteMediaUDPMin, "remote-media-udp-min", 0, "minimum UDP port for the explicitly enabled WebRTC media range")
	flag.UintVar(&remoteMediaUDPMax, "remote-media-udp-max", 0, "maximum UDP port for the explicitly enabled WebRTC media range")
	flag.BoolVar(&remoteIncomingAnswer, "remote-incoming-answer", false, "allow exact media-lease-gated remote ATA for incoming calls")
	flag.BoolVar(&remoteRescueHangup, "remote-rescue-hangup", false, "allow one-shot media-proven remote ATH for the exact answered incoming call")
	flag.StringVar(&publicWebFlags.Listen, "public-web-listen", defaultPublicWebListen, "dedicated loopback listen address for the Cloudflare Access SMS PWA")
	flag.StringVar(&publicWebFlags.Host, "public-web-host", "", "exact public HTTPS host for the Cloudflare Access SMS PWA")
	flag.StringVar(&publicWebFlags.TeamDomain, "public-web-access-team-domain", "", "exact Cloudflare Access team domain")
	flag.StringVar(&publicWebFlags.Audience, "public-web-access-audience", "", "exact Cloudflare Access application audience")
	flag.StringVar(&publicWebFlags.AllowedEmailsFile, "public-web-access-allowed-email-file", "", "absolute mode-0600 file containing one allowed email per line")
	flag.BoolVar(&publicWebFlags.Control, "public-web-control", false, "allow the Cloudflare Access SMS PWA to send and refresh SMS")
	flag.BoolVar(&publicWebFlags.ExternalVoice, "public-web-external-voice", false, "allow the Cloudflare Access PWA to use the configured external SIP voice runtime")
	flag.BoolVar(&publicWebFlags.DirectVoice, "public-web-direct-voice", false, "allow incoming and outgoing QDC507 browser voice through public relay-only WebRTC")
	flag.StringVar(&publicWebFlags.TURNHost, "public-web-turn-host", "", "exact public TURN DNS host for relay-only WebRTC")
	flag.StringVar(&publicWebFlags.TURNSecretFile, "public-web-turn-secret-file", "", "absolute mode-0600 coturn REST shared-secret file")
	flag.UintVar(&publicWebFlags.TURNUDPPort, "public-web-turn-udp-port", 0, "public TURN UDP/TCP listener port; defaults to 3478")
	flag.UintVar(&publicWebFlags.TURNTLSPort, "public-web-turn-tls-port", 0, "public TURN TLS listener port; defaults to 443")
	flag.DurationVar(&publicWebFlags.TURNCredentialTTL, "public-web-turn-credential-ttl", 0, "short-lived TURN credential lifetime; defaults to 5m")
	flag.StringVar(&publicWebFlags.RecordingsDir, "public-web-recordings-dir", "", "private Mac directory for public-web call recordings; defaults under Application Support")
	flag.BoolVar(&publicWebFlags.Push.Enabled, "public-web-push", false, "allow fixed incoming-call Web Push for the public voice PWA")
	flag.StringVar(&publicWebFlags.Push.PrivateKeyFile, "public-web-push-vapid-private-key-file", "", "absolute mode-0600 file containing one base64url P-256 VAPID private key")
	flag.StringVar(&publicWebFlags.Push.Subject, "public-web-push-vapid-subject", "", "exact mailto: or public HTTPS VAPID contact")
	flag.StringVar(&publicWebFlags.Push.SubscriptionsFile, "public-web-push-subscriptions-file", "", "absolute private JSON subscription store on this Mac")
	flag.StringVar(&externalVoiceFlags.Provider, "voice-provider", "", "external voice provider; blank keeps external voice disabled")
	flag.StringVar(&externalVoiceFlags.GatewayID, "voice-gateway-id", "", "stable configured ID for the external voice gateway")
	flag.StringVar(&externalVoiceFlags.ARIURL, "asterisk-ari-url", "", "exact private Asterisk ARI base URL")
	flag.StringVar(&externalVoiceFlags.ARIApplication, "asterisk-ari-application", "", "exact Asterisk Stasis application")
	flag.StringVar(&externalVoiceFlags.ARIArgument, "asterisk-ari-incoming-argument", "", "exact Stasis argument for incoming calls")
	flag.StringVar(&externalVoiceFlags.ARIContext, "asterisk-ari-incoming-context", "", "exact Asterisk dialplan context for incoming calls")
	flag.StringVar(&externalVoiceFlags.ARIEndpoint, "asterisk-ari-incoming-endpoint", "", "exact PJSIP endpoint prefix for incoming calls")
	flag.StringVar(&externalVoiceFlags.ARIOutgoingEndpoint, "asterisk-ari-outgoing-endpoint", "", "explicit PJSIP endpoint for browser-authorized outbound calls")
	flag.StringVar(&externalVoiceFlags.ARIUsername, "asterisk-ari-username", "", "Asterisk ARI username")
	flag.StringVar(&externalVoiceFlags.ARIPasswordFile, "asterisk-ari-password-file", "", "absolute mode-0600 file containing the Asterisk ARI password")
	flag.StringVar(&externalVoiceFlags.ARIRecoveryUsername, "asterisk-recovery-ari-username", "", "read-only Asterisk ARI username for restart recovery")
	flag.StringVar(&externalVoiceFlags.ARIRecoveryPasswordFile, "asterisk-recovery-ari-password-file", "", "absolute mode-0600 file containing the read-only Asterisk recovery password")
	flag.StringVar(&externalVoiceFlags.ARIExpectedEntityID, "asterisk-expected-entity-id", "", "exact Asterisk entity ID required for external voice mutations")
	flag.StringVar(&externalVoiceFlags.ARIExpectedVersion, "asterisk-expected-version", "", "exact validated Asterisk version required for external voice")
	flag.StringVar(&externalVoiceFlags.ARIIncomingPolicyID, "asterisk-incoming-policy-id", "", "exact dialplan policy marker required on incoming calls")
	flag.StringVar(&externalVoiceFlags.RecoveryStorePath, "sip-recovery-store", "", "private external voice mutation recovery file; defaults under the user config directory")
	flag.StringVar(&externalVoiceFlags.MediaInterface, "sip-media-interface", "", "exact Tailscale interface for external voice WebRTC media")
	flag.StringVar(&externalVoiceFlags.MediaLocalCIDRs, "sip-media-local-cidrs", "", "canonical local Tailscale CIDRs for external voice WebRTC")
	flag.StringVar(&externalVoiceFlags.MediaRemoteCIDRs, "sip-media-remote-cidrs", "", "one exact remote Tailscale /32 or /128 peer")
	flag.UintVar(&externalVoiceFlags.MediaUDPMin, "sip-media-udp-min", 0, "minimum external voice WebRTC UDP port")
	flag.UintVar(&externalVoiceFlags.MediaUDPMax, "sip-media-udp-max", 0, "maximum external voice WebRTC UDP port")
	flag.BoolVar(&externalVoiceFlags.AllowAnswer, "sip-answer-incoming", false, "allow media-gated incoming answer through the external SIP gateway")
	flag.BoolVar(&externalVoiceFlags.AllowReject, "sip-reject-incoming", false, "allow rejecting the exact observed incoming external SIP call")
	flag.BoolVar(&externalVoiceFlags.AllowDTMF, "sip-send-dtmf", false, "allow DTMF on the exact active external SIP call")
	flag.BoolVar(&externalVoiceFlags.AllowEnd, "sip-end-active", false, "allow ending the exact externally answered active call")
	flag.BoolVar(&externalVoiceFlags.AllowDial, "sip-dial-outgoing", false, "allow browser-authorized outbound calls through the configured external SIP endpoint")
	flag.StringVar(&publicEdgeFlags.URL, "public-edge-url", "", "exact public edge WSS URL; blank keeps public access disabled")
	flag.StringVar(&publicEdgeFlags.GatewayID, "public-edge-gateway-id", "", "stable configured public edge gateway ID")
	flag.StringVar(&publicEdgeFlags.PrivateKeyFile, "public-edge-private-key-file", "", "absolute mode-0600 PKCS#8 P-256 gateway private key file")
	flag.StringVar(&publicEdgeFlags.AccessClientIDFile, "public-edge-access-client-id-file", "", "absolute mode-0600 Cloudflare Access service-token client ID file")
	flag.StringVar(&publicEdgeFlags.AccessClientSecretFile, "public-edge-access-client-secret-file", "", "absolute mode-0600 Cloudflare Access service-token secret file")
	flag.StringVar(&smsStorePath, "sms-store", defaultSMSStoreRoot(), "private durable SMS store directory")
	flag.StringVar(&callHistoryPath, "call-history-store", defaultCallHistoryStorePath(), "private durable call history file")
	flag.BoolVar(&nativeEnrollmentOnce, "native-enrollment-once", false, "allow one local one-time native device enrollment ticket during this process lifetime")
	flag.StringVar(&nativeDeviceStorePath, "native-device-store", defaultNativeDeviceStoreRoot(), "private native device enrollment store directory")
	flag.Parse()
	probeSerialAT, err := shouldProbeSerialAT(port, usbATOnly)
	if err != nil {
		log.Fatal(err)
	}
	if !isLoopbackListenAddress(listen) {
		log.Fatalf("refusing non-loopback listen address %q", listen)
	}
	if !isLoopbackListenAddress(remoteListen) {
		log.Fatalf("refusing non-loopback remote listen address %q", remoteListen)
	}
	if !isLoopbackListenAddress(publicWebFlags.Listen) {
		log.Fatalf("refusing non-loopback public web listen address %q", publicWebFlags.Listen)
	}
	if strings.TrimSpace(remoteAllowedLogins) != "" &&
		(strings.TrimSpace(remoteCapability) == "" || canonicalRemoteHost(remoteHost) == "") {
		log.Fatal("remote access requires both -remote-capability and -remote-host")
	}
	if remoteMediaUDPMin > 65535 || remoteMediaUDPMax > 65535 {
		log.Fatal("remote media UDP ports must be between 1 and 65535")
	}
	if remoteIncomingAnswer && (!remoteControl || strings.TrimSpace(remoteAllowedLogins) == "" || strings.TrimSpace(remoteMediaInterface) == "") {
		log.Fatal("remote incoming answer requires remote control, an exact login allowlist, and explicit remote media configuration")
	}
	if remoteRescueHangup && !remoteIncomingAnswer {
		log.Fatal("remote rescue hangup requires the separate remote incoming answer opt-in")
	}
	if nativeEnrollmentOnce && strings.TrimSpace(remoteAllowedLogins) == "" {
		log.Fatal("native enrollment requires the authenticated remote gateway")
	}
	externalVoiceStartup, err := parseExternalVoiceStartupConfig(externalVoiceFlags)
	if err != nil {
		log.Fatalf("external voice remains disabled: %v", err)
	}
	if demo && externalVoiceStartup != nil {
		log.Fatal("external voice cannot be combined with demo mode")
	}
	publicEdgeStartup, err := parsePublicEdgeStartupConfig(publicEdgeFlags)
	if err != nil {
		log.Fatalf("public edge remains disabled: %v", err)
	}
	publicWebStartup, err := parsePublicWebStartupConfig(publicWebFlags)
	if err != nil {
		log.Fatalf("public web remains disabled: %v", err)
	}
	if err := validateModemRuntimeMode(smsOnlyRuntime, phoneRelayRuntime, publicWebStartup); err != nil {
		log.Fatal(err)
	}
	configureNative := func(instance *app) {
		if err := instance.configureNativeGateway(nativeDeviceStorePath, nativeEnrollmentOnce); err != nil {
			if nativeEnrollmentOnce {
				log.Fatalf("native enrollment remains disabled: %v", err)
			}
			log.Printf("native device access remains disabled: %v", err)
		}
	}
	configureMedia := func(instance *app) {
		if err := instance.configureRemoteMedia(
			remoteMediaInterface, remoteMediaLocalCIDRs, remoteMediaRemoteCIDRs,
			uint16(remoteMediaUDPMin), uint16(remoteMediaUDPMax),
		); err != nil {
			log.Fatalf("remote media remains disabled: %v", err)
		}
		instance.remoteIncomingAnswer = remoteIncomingAnswer && instance.remoteMedia != nil
		instance.remoteRescueHangup = remoteRescueHangup && instance.remoteIncomingAnswer
	}
	configureExternalVoice := func(instance *app) {
		if err := instance.configureExternalVoiceStartup(externalVoiceStartup); err != nil {
			log.Fatalf("external voice remains disabled: %v", err)
		}
	}
	configurePublicEdge := func(instance *app) {
		instance.configurePublicEdgeStartup(publicEdgeStartup)
	}
	configurePublicWeb := func(instance *app) {
		if err := instance.configurePublicWebAccess(publicWebStartup); err != nil {
			log.Fatalf("public web remains disabled: %v", err)
		}
	}

	if demo {
		instance := newDemoApp()
		instance.smsOnlyRuntime = smsOnlyRuntime
		instance.webConsole = webConsole
		instance.configureRemoteAccess(remoteAllowedLogins, remoteControl, remoteCapability, remoteHost, remoteListen)
		configureNative(instance)
		configureMedia(instance)
		configurePublicWeb(instance)
		configureExternalVoice(instance)
		configurePublicEdge(instance)
		log.Printf("DJOneHub demo mode")
		serve(instance, listen)
		return
	}

	if strings.TrimSpace(port) == "" {
		var err error
		if probeSerialAT {
			port, err = discoverATPort()
		} else {
			err = errors.New("serial AT discovery disabled by -usb-at-only")
		}
		if err != nil {
			usbDevice := discoverDJIUSBDevice()
			usbATDevice, usbATErr := openDJIUSBAT()
			instance := &app{
				port:               "未发现 AT 串口",
				smsOnlyRuntime:     smsOnlyRuntime,
				implicitUACVoice:   phoneRelayRuntime,
				discoveryError:     err.Error(),
				usbDevice:          usbDevice,
				usbAT:              usbATDevice,
				smsPollInterval:    8 * time.Second,
				smsAutoCleanupME:   false,
				smsReassembler:     smscodec.NewReassembler(),
				callPollInterval:   3 * time.Second,
				audio:              newAudioRouter(),
				swiftAudioHost:     phoneRelayRuntime,
				webConsole:         webConsole,
				autoNetworkRepair:  autoNetworkRepair,
				autoSignalRecovery: autoSignalRecovery,
			}
			if err := instance.initializeSMSStore(smsStorePath); err != nil {
				log.Printf("SMS store unavailable: error_code=sms_store_unavailable")
			}
			if err := instance.initializeCallHistoryStore(callHistoryPath); err != nil {
				log.Printf("call history unavailable: %v", err)
			}
			instance.configureRemoteAccess(remoteAllowedLogins, remoteControl, remoteCapability, remoteHost, remoteListen)
			configureNative(instance)
			configureMedia(instance)
			configurePublicWeb(instance)
			configureExternalVoice(instance)
			configurePublicEdge(instance)
			if usbDevice != nil {
				log.Printf("DJI USB device detected without AT serial port: %s %s (%s:%s)",
					usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			}
			if usbATErr != nil {
				log.Printf("USB AT unavailable: %v", usbATErr)
			} else {
				instance.setDeviceConnectionState(usbATDevice.Description(), "")
				defer usbATDevice.Close()
				log.Printf("USB AT bridge opened on DJI %s", usbATDevice.Description())
				instance.initUSBATESIMManager()
				instance.kickModuleVoice()
				go instance.ensureCellularDHCP()
			}
			log.Printf("modem discovery skipped: %v", err)
			startupPlan := planModemRuntimeStartup(smsOnlyRuntime, phoneRelayRuntime)
			if startupPlan.SMS {
				go instance.startSMSPoller(context.Background())
			}
			if startupPlan.CallPoll {
				go instance.startCallPoller(context.Background())
			}
			if startupPlan.GPSPoll {
				go instance.startGPSPoller(context.Background())
			}
			if instance.autoSignalRecovery {
				go instance.startSignalRecovery(context.Background())
			}
			if startupPlan.GPSReadback {
				go instance.syncGPSState()
			}
			serve(instance, listen)
			return
		}
	}

	cfg := config.DeviceConfig{
		ID:            "mac-modem",
		Name:          "DJI 4G Module",
		ATPort:        port,
		ManagePort:    port,
		DeviceBackend: backend.BackendAT,
		ESIMTransport: "at",
		BaudRate:      115200,
		DataBits:      8,
		StopBits:      1,
		Parity:        "none",
		SMSEnabled:    true,
	}
	manager, err := modem.New(cfg)
	if err != nil {
		log.Fatalf("create modem manager: %v", err)
	}

	instance := &app{
		modem: manager, port: port,
		smsOnlyRuntime:   smsOnlyRuntime,
		implicitUACVoice: phoneRelayRuntime,
		smsPollInterval:  8 * time.Second, smsAutoCleanupME: false,
		callPollInterval:   3 * time.Second,
		audio:              newAudioRouter(),
		swiftAudioHost:     phoneRelayRuntime,
		webConsole:         webConsole,
		autoNetworkRepair:  autoNetworkRepair,
		autoSignalRecovery: autoSignalRecovery,
	}
	if err := instance.initializeSMSStore(smsStorePath); err != nil {
		log.Printf("SMS store unavailable: error_code=sms_store_unavailable")
	}
	if err := instance.initializeCallHistoryStore(callHistoryPath); err != nil {
		log.Printf("call history unavailable: %v", err)
	}
	instance.configureRemoteAccess(remoteAllowedLogins, remoteControl, remoteCapability, remoteHost, remoteListen)
	configureNative(instance)
	configureMedia(instance)
	configurePublicWeb(instance)
	configureExternalVoice(instance)
	configurePublicEdge(instance)
	manager.SetSMSCallback(instance.recordSMS)
	if err := manager.Start(); err != nil {
		log.Fatalf("open modem on %s: %v", port, err)
	}
	defer manager.Stop()

	if !manager.WaitReady(15 * time.Second) {
		log.Printf("modem initialization is still running; the web UI will remain available")
	}

	atBackend := backend.NewATBackend(manager)
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-modem",
		Transport: "at",
		Modem:     manager,
		Backend:   atBackend,
	})
	if err != nil {
		log.Printf("eSIM manager unavailable: %v", err)
	} else {
		instance.installESIMManager(esimManager, false)
	}

	startupPlan := planModemRuntimeStartup(smsOnlyRuntime, phoneRelayRuntime)
	if startupPlan.SMS {
		go manager.CheckAllSMS()
	}
	if startupPlan.CallPoll {
		go instance.startCallPoller(context.Background())
	}
	if startupPlan.GPSPoll {
		go instance.startGPSPoller(context.Background())
	}
	if startupPlan.GPSReadback {
		go instance.syncGPSState()
	}

	serve(instance, listen)
}

func (a *app) initUSBATESIMManager() {
	if manager, _ := a.currentESIMManager(); manager != nil {
		return
	}
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-usbat",
		Transport: "custom",
		SmartCardChannelFactory: func() (driver.SmartCardChannel, error) {
			return newUSBATESIMChannel(a.runATCommand), nil
		},
	})
	if err != nil {
		log.Printf("eSIM manager unavailable over USB AT: %v", err)
		return
	}
	if a.installESIMManager(esimManager, true) {
		log.Printf("eSIM manager is available over USB AT with profile switching enabled")
	}
}

func (a *app) currentESIMManager() (*esim.Manager, bool) {
	a.esimMu.RLock()
	defer a.esimMu.RUnlock()
	return a.esim, a.esimSwitchAllowed
}

func (a *app) installESIMManager(manager *esim.Manager, switchAllowed bool) bool {
	if manager == nil {
		return false
	}
	a.esimMu.Lock()
	defer a.esimMu.Unlock()
	if a.esim != nil {
		return false
	}
	a.esim = manager
	a.esimSwitchAllowed = switchAllowed
	return true
}

func serve(instance *app, listen string) {
	defer instance.closeSMSStore()
	defer instance.closeNativeGateway()
	if instance.publicWebPush != nil {
		defer instance.publicWebPush.Close()
	}
	if instance.publicWeb != nil && instance.publicWeb.TURNIssuer != nil {
		defer instance.publicWeb.TURNIssuer.Close()
	}
	if instance.remoteMedia != nil {
		defer instance.remoteMedia.Close()
	}
	if instance.remoteLedger != nil {
		defer func() {
			if err := instance.remoteLedger.Close(); err != nil {
				log.Printf("remote operation ledger close: %v", err)
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr:              listen,
		Handler:           instance.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	mediaControl, err := instance.startRemoteMediaControlServer(ctx)
	if policyErr := validateRemoteMediaControlStartup(instance, mediaControl, err); policyErr != nil {
		if mediaControl != nil {
			_ = mediaControl.Close()
		}
		log.Printf("public direct voice startup failed closed: %v", policyErr)
		return
	}
	if err != nil {
		log.Printf("remote media remains disabled: local control socket: %v", err)
		if instance.remoteMedia != nil {
			_ = instance.remoteMedia.Close()
			instance.remoteMedia = nil
		}
	} else if mediaControl != nil {
		defer mediaControl.Close()
		log.Printf("Remote media local control is ready (path redacted)")
	}
	if !instance.demo {
		go instance.startCellularPolicyGuard(ctx)
	}

	if !instance.demo {
		log.Printf("DJOneHub is using %s", instance.snapshotDeviceState().Port)
	}
	log.Printf("Open http://%s", listen)
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		if platformOpenExistingUI("http://" + listen) {
			return
		}
		log.Printf("HTTP listen failed: %v", err)
		return
	}
	var remoteServer *http.Server
	var remoteListener net.Listener
	if instance.remoteAccess.enabled() {
		remoteServer = &http.Server{
			Addr:              instance.remoteListen,
			Handler:           instance.remoteRoutes(),
			ReadHeaderTimeout: 5 * time.Second,
			MaxHeaderBytes:    32 << 10,
		}
		remoteListener, err = net.Listen("tcp", instance.remoteListen)
		if err != nil {
			_ = listener.Close()
			log.Printf("remote gateway listen failed closed: %v", err)
			return
		}
		log.Printf("Remote gateway is listening on loopback %s for Tailscale Serve", instance.remoteListen)
	}
	var publicWebServer *http.Server
	var publicWebListener net.Listener
	if instance.publicWeb != nil {
		publicWebServer = &http.Server{
			Addr:              instance.publicWeb.Listen,
			Handler:           http.NotFoundHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			MaxHeaderBytes:    32 << 10,
		}
		publicWebListener, err = net.Listen("tcp", instance.publicWeb.Listen)
		if err != nil {
			if remoteListener != nil {
				_ = remoteListener.Close()
			}
			_ = listener.Close()
			log.Printf("public web listen failed closed: %v", err)
			return
		}
		log.Printf("Public phone PWA is listening on dedicated loopback %s for Cloudflare Tunnel", instance.publicWeb.Listen)
	}
	// Bind every public process endpoint before subscribing to the shared
	// Asterisk Stasis application. A duplicate local instance must fail before
	// it can briefly compete for call events with the authoritative process.
	if err := instance.startExternalVoice(ctx); err != nil {
		log.Printf("external voice remains disabled: %v", err)
	}
	if instance.sipVoice != nil {
		defer instance.sipVoice.Close()
	}
	// Route construction depends on whether the SIP supervisor was actually
	// installed. The listener is already bound above, so a duplicate process
	// still fails before either process subscribes to Asterisk events.
	if publicWebServer != nil {
		publicWebServer.Handler = instance.publicWebRoutes()
	}
	if err := instance.startPublicEdge(ctx); err != nil {
		log.Printf("public edge remains disabled: error_code=public_edge_configuration_invalid")
	} else if instance.publicEdge != nil {
		defer instance.closePublicEdge()
		log.Printf("Public edge outbound status connector is enabled")
	}
	openPlatformUI("http://" + listen)

	type serverResult struct {
		name string
		err  error
	}
	serveErr := make(chan serverResult, 3)
	go func() {
		serveErr <- serverResult{name: "local", err: server.Serve(listener)}
	}()
	if remoteServer != nil {
		go func() {
			serveErr <- serverResult{name: "remote", err: remoteServer.Serve(remoteListener)}
		}()
	}
	if publicWebServer != nil {
		go func() {
			serveErr <- serverResult{name: "public-web", err: publicWebServer.Serve(publicWebListener)}
		}()
	}

	select {
	case result := <-serveErr:
		if !errors.Is(result.err, http.ErrServerClosed) {
			log.Printf("%s HTTP server stopped unexpectedly: %v", result.name, result.err)
		}
	case <-ctx.Done():
		log.Printf("DJOneHub is stopping")
		if !instance.demo {
			if err := instance.stopModuleVoiceRouteForceChecked(); err != nil {
				log.Printf("module voice route cleanup during shutdown: %v", err)
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
		if remoteServer != nil {
			if err := remoteServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("remote HTTP server shutdown: %v", err)
			}
		}
		if publicWebServer != nil {
			if err := publicWebServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("public web HTTP server shutdown: %v", err)
			}
		}
	}
}

func newDemoApp() *app {
	now := time.Now()
	return &app{
		demo:             true,
		port:             "Demo · Quectel EG25-G",
		smsPollInterval:  8 * time.Second,
		callPollInterval: 3 * time.Second,
		sms: []receivedSMS{
			{
				Sender:    "10086",
				Content:   "【DJOneHub 演示】本月套餐剩余流量 18.6GB。",
				Timestamp: now.Add(-18 * time.Minute),
			},
			{
				Sender:    "+44 7400 123456",
				Content:   "Your verification code is 482913. It expires in 10 minutes.",
				Code:      "482913",
				Timestamp: now.Add(-2 * time.Hour),
			},
		},
	}
}

func discoverATPort() (string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return "", fmt.Errorf("list serial ports: %w", err)
	}
	ports = filterCandidateATPorts(ports, runtime.GOOS)

	sort.SliceStable(ports, func(i, j int) bool {
		return portScore(ports[i]) > portScore(ports[j])
	})
	var attempted []string
	for _, port := range ports {
		attempted = append(attempted, port)
		if _, err := modem.ProbeIMEICached(port, 2*time.Second); err == nil {
			return port, nil
		}
	}
	if len(attempted) == 0 {
		if runtime.GOOS == "windows" {
			return "", errors.New("no Windows COM ports found; install the module serial driver or pass -port COMx explicitly")
		}
		return "", errors.New("no Quectel/DJI USB serial ports found; pass -port /dev/cu.* explicitly")
	}
	return "", fmt.Errorf("no AT-capable port found among %s", strings.Join(attempted, ", "))
}

func filterCandidateATPorts(ports []string, goos string) []string {
	filtered := make([]string, 0, len(ports))
	for _, port := range ports {
		name := strings.ToLower(strings.TrimSpace(port))
		if name == "" {
			continue
		}
		if goos == "windows" {
			if strings.HasPrefix(name, "com") {
				filtered = append(filtered, port)
			}
			continue
		}
		if strings.Contains(name, "usbmodem") ||
			strings.Contains(name, "usbserial") ||
			strings.Contains(name, "wchusbserial") ||
			strings.Contains(name, "quectel") ||
			strings.Contains(name, "dji") {
			filtered = append(filtered, port)
		}
	}
	return filtered
}

func portScore(port string) int {
	name := strings.ToLower(port)
	if strings.Contains(name, "quectel") || strings.Contains(name, "dji") {
		return 100
	}
	if strings.Contains(name, "usbmodem") {
		return 80
	}
	if strings.Contains(name, "usbserial") {
		return 60
	}
	if strings.HasPrefix(name, "com") {
		return 50
	}
	return 0
}

func discoverDJIUSBDevice() *usbDeviceStatus {
	out, err := exec.Command("ioreg", "-r", "-c", "IOUSBHostInterface", "-l", "-w", "0").Output()
	if err != nil {
		return nil
	}

	var device *usbDeviceStatus
	for _, block := range strings.Split(string(out), "\n\n") {
		vendorID, okVendor := intProperty(block, "idVendor")
		productID, okProduct := intProperty(block, "idProduct")
		if !okVendor || !okProduct || !isSupportedUSBModuleIdentity(vendorID, productID) {
			continue
		}
		if device == nil {
			device = &usbDeviceStatus{
				Product:    stringProperty(block, "USB Product Name"),
				Vendor:     stringProperty(block, "USB Vendor Name"),
				VendorID:   fmt.Sprintf("%04x", vendorID),
				ProductID:  fmt.Sprintf("%04x", productID),
				LocationID: formatHexProperty(block, "locationID"),
				Speed:      usbSpeedName(intPropertyOrZero(block, "USBSpeed")),
				Mode:       "vendor-specific USB mode",
			}
			if strings.TrimSpace(device.Product) == "" {
				device.Product = "DJI 4G Module"
			}
			if strings.TrimSpace(device.Vendor) == "" {
				device.Vendor = "DJI"
			}
		}
		ifaceNumber, okIface := intProperty(block, "bInterfaceNumber")
		if !okIface {
			continue
		}
		iface := usbInterfaceStatus{
			Number:    ifaceNumber,
			Class:     intPropertyOrZero(block, "bInterfaceClass"),
			Subclass:  intPropertyOrZero(block, "bInterfaceSubClass"),
			Protocol:  intPropertyOrZero(block, "bInterfaceProtocol"),
			Endpoints: intPropertyOrZero(block, "bNumEndpoints"),
		}
		device.Interfaces = append(device.Interfaces, iface)
	}
	if device == nil {
		return nil
	}
	sort.SliceStable(device.Interfaces, func(i, j int) bool {
		return device.Interfaces[i].Number < device.Interfaces[j].Number
	})
	if allVendorSpecific(device.Interfaces) {
		device.Mode = "vendor-specific QMI/diagnostic mode"
	}
	return device
}

func allVendorSpecific(interfaces []usbInterfaceStatus) bool {
	if len(interfaces) == 0 {
		return false
	}
	for _, iface := range interfaces {
		if iface.Class != 255 {
			return false
		}
	}
	return true
}

func intPropertyOrZero(block, name string) int {
	value, _ := intProperty(block, name)
	return value
}

func intProperty(block, name string) (int, bool) {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*(\d+)`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	return value, err == nil
}

func stringProperty(block, name string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*"([^"]*)"`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func formatHexProperty(block, name string) string {
	value, ok := intProperty(block, name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("0x%x", value)
}

func usbSpeedName(speed int) string {
	switch speed {
	case 1:
		return "low-speed"
	case 2:
		return "full-speed"
	case 3:
		return "high-speed"
	case 4:
		return "super-speed"
	default:
		if speed == 0 {
			return ""
		}
		return fmt.Sprintf("speed-%d", speed)
	}
}

func (a *app) recordSMS(sender, content string, timestamp time.Time) {
	if _, _, err := a.ingestIncomingSMS([]receivedSMS{{
		Sender: sender, Content: content, Timestamp: timestamp,
	}}); err != nil {
		log.Printf("incoming SMS was retained on the module: error_code=sms_store_unavailable")
	}
}

func (a *app) mergeSMSCache(messages []receivedSMS) (newCount int, total int) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	seen := make(map[string]bool, len(a.sms)+len(messages))
	for _, item := range a.sms {
		seen[smsCacheKey(item)] = true
	}
	for _, item := range messages {
		if item.Code == "" {
			item.Code = extractSMSCode(item.Content)
		}
		key := smsCacheKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		a.sms = append(a.sms, item)
		newCount++
	}
	sort.SliceStable(a.sms, func(i, j int) bool {
		return a.sms[i].Timestamp.After(a.sms[j].Timestamp)
	})
	if len(a.sms) > 500 {
		a.sms = a.sms[:500]
	}
	return newCount, len(a.sms)
}

func smsCacheKey(item receivedSMS) string {
	return item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
}

func (a *app) setSMSPollStatus(err error) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	a.smsLastPoll = time.Now()
	if err != nil {
		a.smsLastPollError = err.Error()
		return
	}
	a.smsLastPollError = ""
}

func (a *app) startSMSPoller(ctx context.Context) {
	interval := a.smsPollInterval
	if interval <= 0 {
		interval = 8 * time.Second
	}
	timer := time.NewTimer(1200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.pollSMSOnce(); err != nil {
				log.Printf("SMS poll failed: %v", err)
			}
			timer.Reset(interval)
		}
	}
}

func (a *app) pollSMSOnce() error {
	if a.demo || a.modem != nil {
		return nil
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	if err := a.ensureUSBAT(); err != nil {
		a.setSMSPollStatus(err)
		return err
	}
	messages, err := a.readUSBATSMS()
	if err != nil {
		a.setSMSPollStatus(err)
		return err
	}
	newCount, total, storeErr := a.ingestIncomingSMS(messages)
	if storeErr != nil {
		a.setSMSPollStatus(storeErr)
		return storeErr
	}
	// Never bulk-delete after polling. readUSBATSMS may have succeeded for SM
	// while ME failed, and multipart fragments may not yet have produced a
	// complete message. Only the separately confirmed manual cleanup endpoint
	// is allowed to issue CMGD until per-index durable acknowledgements exist.
	a.setSMSPollStatus(nil)
	if newCount > 0 {
		log.Printf("SMS poll cached %d new message(s), total %d", newCount, total)
	}
	return nil
}

func (a *app) ensureUSBAT() error {
	return a.ensureUSBATAtLocation(0)
}

func (a *app) ensureUSBATAtLocation(requestedLocation uint32) error {
	if a.demo || a.modem != nil {
		return nil
	}
	if a.currentUSBDevice() == nil {
		a.setDeviceConnectionState("未检测到 DJI USB 设备", "DJI USB device is not connected")
		return errors.New("DJI USB device is not connected")
	}
	// Serialize opens: a stuck libusb open must not freeze every caller.
	a.usbATOpenMu.Lock()
	if !a.usbATBackoffUntil.IsZero() && time.Now().Before(a.usbATBackoffUntil) {
		backoffErr := a.usbATBackoffErr
		a.usbATOpenMu.Unlock()
		if backoffErr != "" {
			return fmt.Errorf("USB AT is cooling down after disconnect: %s", backoffErr)
		}
		return errors.New("USB AT is cooling down after disconnect")
	}
	requiredLocation := a.usbATRequiredLocation
	if requestedLocation != 0 {
		if requiredLocation != 0 && requiredLocation != requestedLocation {
			a.usbATOpenMu.Unlock()
			return fmt.Errorf("USB AT location guard conflict: required 0x%08x, requested 0x%08x", requiredLocation, requestedLocation)
		}
		requiredLocation = requestedLocation
	}
	if a.usbAT != nil {
		if requiredLocation != 0 && a.usbAT.LocationID() != requiredLocation {
			actualLocation := a.usbAT.LocationID()
			a.usbATOpenMu.Unlock()
			return fmt.Errorf("USB AT is bound to location 0x%08x, expected 0x%08x", actualLocation, requiredLocation)
		}
		a.usbATOpenMu.Unlock()
		return nil
	}

	dev, err := a.openUSBATWithTimeoutAtLocation(requiredLocation)
	if err != nil {
		if strings.Contains(err.Error(), "open timed out") {
			// The stuck libusb call keeps running in the background; back off
			// so retries do not pile up while the module USB is unstable.
			a.usbATBackoffUntil = time.Now().Add(30 * time.Second)
			a.usbATBackoffErr = err.Error()
		}
		a.usbATOpenMu.Unlock()
		return err
	}
	a.usbAT = dev
	a.usbATBackoffUntil = time.Time{}
	a.usbATBackoffErr = ""
	description := dev.Description()
	// Lock order is usbATOpenMu -> deviceStateMu. No path acquires these in
	// reverse order, so connection text cannot be published after a detach.
	a.setDeviceConnectionState(description, "")
	a.usbATOpenMu.Unlock()
	log.Printf("USB AT bridge opened on DJI %s", description)
	// The first open may fail while USB is re-enumerating. When a later poll
	// succeeds, rebuild the eSIM service that startup could not create.
	a.initUSBATESIMManager()
	a.ensureCellularDHCP()
	a.kickModuleVoice()
	return nil
}

const usbATOpenTimeout = 12 * time.Second

// openUSBATWithTimeout opens the USB AT bridge but never blocks the caller for
// longer than usbATOpenTimeout. libusb open/claim calls cannot be cancelled,
// so a slow attempt is abandoned and reaped in the background to avoid leaking
// a claimed interface when it finally completes.
func (a *app) openUSBATWithTimeout() (*usbAT, error) {
	return a.openUSBATWithTimeoutAtLocation(0)
}

func (a *app) openUSBATWithTimeoutAtLocation(requiredLocation uint32) (*usbAT, error) {
	type openResult struct {
		dev *usbAT
		err error
	}
	done := make(chan openResult, 1)
	go func() {
		dev, err := openDJIUSBATAtLocation(requiredLocation)
		done <- openResult{dev: dev, err: err}
	}()
	select {
	case res := <-done:
		return res.dev, res.err
	case <-time.After(usbATOpenTimeout):
		go func() {
			res := <-done
			if res.dev != nil {
				res.dev.Close()
			}
		}()
		return nil, errors.New("USB AT open timed out after 12s; the module USB may be unstable")
	}
}

func (a *app) requireUSBATLocation(locationID uint32) error {
	if locationID == 0 {
		return errors.New("cannot guard an unknown USB location")
	}
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	if a.usbATRequiredLocation != 0 && a.usbATRequiredLocation != locationID {
		return fmt.Errorf("another module transaction already requires USB location 0x%08x", a.usbATRequiredLocation)
	}
	if a.usbAT == nil || a.usbAT.LocationID() != locationID {
		return fmt.Errorf("current USB AT device is not at required location 0x%08x", locationID)
	}
	a.usbATRequiredLocation = locationID
	return nil
}

// claimCurrentUSBATLocation atomically captures the currently opened physical
// module and installs the exact-location reopen guard before setup reads its
// first configuration value.
func (a *app) claimCurrentUSBATLocation() (uint32, error) {
	if err := a.ensureUSBAT(); err != nil {
		return 0, err
	}
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	if a.usbAT == nil { // usbATOpenMu held
		return 0, errors.New("USB AT device is not open")
	}
	locationID := a.usbAT.LocationID()
	if locationID == 0 {
		return 0, errors.New("cannot guard an unknown USB location")
	}
	if a.usbATRequiredLocation != 0 && a.usbATRequiredLocation != locationID {
		return 0, fmt.Errorf("another module transaction already requires USB location 0x%08x", a.usbATRequiredLocation)
	}
	a.usbATRequiredLocation = locationID
	return locationID, nil
}

// withExclusiveUSBATCommandsAtLocation holds both the app's handle lifecycle
// lock and the usbAT command lock. A physical detach can only make the current
// handle fail; it cannot cause the operation to continue on a replacement.
func (a *app) withExclusiveUSBATCommandsAtLocation(locationID uint32, operation func(usbATCommandFunc) error) error {
	_, err := a.withExclusiveUSBATCommandsAtLocationExact(locationID, operation)
	return err
}

func (a *app) withExclusiveUSBATCommandsAtLocationExact(locationID uint32, operation func(usbATCommandFunc) error) (usbATExecution, error) {
	return a.withExclusiveUSBATCommandsAtLocationPinned(
		locationID,
		func(_ *usbAT, _ usbATPhysicalIdentity, command usbATCommandFunc) error {
			return operation(command)
		},
	)
}

// withExclusiveUSBATCommandsAtLocationPinned additionally exposes the exact
// lifecycle pointer and its immutable physical identity to the callback. The
// values are captured only after usbATOpenMu is held and before the device
// command mutex is acquired; a same-location replacement therefore cannot be
// relabelled as the claimed device during the transaction.
func (a *app) withExclusiveUSBATCommandsAtLocationPinned(
	locationID uint32,
	operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error,
) (usbATExecution, error) {
	if operation == nil {
		return usbATExecution{}, errors.New("exclusive pinned USB AT operation is nil")
	}
	if a.usbATPinnedSessionOverride != nil {
		return usbATExecution{}, a.usbATPinnedSessionOverride(locationID, func(device *usbAT, identity usbATPhysicalIdentity, command usbATCommandFunc) error {
			return operation(device, identity, a.moduleVoiceCommand(command))
		})
	}
	if a.usbATSessionOverride != nil {
		return usbATExecution{}, a.usbATSessionOverride(locationID, func(command usbATCommandFunc) error {
			return operation(nil, usbATPhysicalIdentity{}, a.moduleVoiceCommand(command))
		})
	}
	if locationID == 0 {
		return usbATExecution{}, errors.New("exclusive USB AT session requires a location")
	}
	if err := a.ensureUSBATAtLocation(locationID); err != nil {
		return usbATExecution{}, err
	}
	a.usbATOpenMu.Lock()
	device := a.usbAT
	if device == nil || device.LocationID() != locationID {
		a.usbATOpenMu.Unlock()
		return usbATExecution{}, fmt.Errorf("USB AT device is not bound to required location 0x%08x", locationID)
	}
	identity := device.PhysicalIdentity()
	if identity.Location != locationID || identity.VendorID <= 0 || identity.ProductID <= 0 {
		a.usbATOpenMu.Unlock()
		return usbATExecution{}, errors.New("exclusive USB AT session has an invalid physical identity")
	}
	execution := usbATExecution{device: device}
	err := device.WithExclusiveCommands(func(command usbATCommandFunc) error {
		return operation(device, identity, a.moduleVoiceCommand(command))
	})
	a.usbATOpenMu.Unlock()
	a.resetUSBATDeviceIfGone(device, err)
	return execution, err
}

func (a *app) moduleVoiceCommand(command usbATCommandFunc) usbATCommandFunc {
	if a == nil || !a.implicitUACVoice || command == nil {
		return command
	}
	return func(at string, timeout time.Duration) (string, error) {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(at)), "AT+QPCMV") {
			// Do not transmit the unsupported selector. The route code already
			// recognizes this exact response as the modem's implicit-UAC mode.
			return at + "\r\nERROR\r\n", nil
		}
		return command(at, timeout)
	}
}

func (a *app) releaseUSBATLocation(locationID uint32) {
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	if a.usbATRequiredLocation == locationID {
		a.usbATRequiredLocation = 0
	}
}

func (a *app) resetUSBATDeviceIfGone(device *usbAT, err error) {
	if err == nil || device == nil {
		return
	}
	text := strings.ToUpper(err.Error())
	if !strings.Contains(text, "NO_DEVICE") &&
		!strings.Contains(text, "NOT_FOUND") &&
		!strings.Contains(text, "USB AT COMMAND TIMED OUT") {
		return
	}
	a.markUSBATDeviceDetached(device, err.Error())
}

// markUSBATDetached clears state belonging to a physically removed module.
// A later status/SMS poll will discover and open a newly connected module.
func (a *app) markUSBATDetached(reason string) {
	a.markUSBATDeviceDetached(nil, reason)
}

// markUSBATDeviceDetached clears expected when it is still current. A nil
// expected explicitly means "detach whichever handle is current". The pointer
// is removed under usbATOpenMu, then closed outside every app lock; this keeps
// the global lock order independent from usbAT.mu and other subsystem locks.
func (a *app) markUSBATDeviceDetached(expected *usbAT, reason string) {
	a.usbATOpenMu.Lock()
	device := a.usbAT
	if expected != nil && device != expected {
		a.usbATOpenMu.Unlock()
		return
	}
	a.usbAT = nil
	a.usbATBackoffUntil = time.Now().Add(2 * time.Second)
	a.usbATBackoffErr = reason
	a.setDetachedDeviceState()
	a.usbATOpenMu.Unlock()
	if device != nil {
		log.Printf("USB AT bridge detached; waiting for a new enumeration: %s", reason)
		device.Close()
	}
	a.moduleVoiceMu.Lock()
	if a.moduleVoiceReady || a.moduleVoicePhase == "arming" || a.moduleVoicePhase == "armed" ||
		a.moduleVoicePhase == "call_command_ambiguous" || a.moduleVoicePhase == "cleanup_pending" {
		a.moduleVoiceReady = false
		a.moduleVoicePhase = "unknown"
		a.moduleVoiceErr = "USB detached: " + reason
	}
	a.moduleVoiceMu.Unlock()
	a.callMu.Lock()
	a.callConfigured = false
	a.callMu.Unlock()
	if manager, _ := a.currentESIMManager(); manager != nil {
		manager.NotifyModemReset()
	}
}

// startSignalRecovery runs a self-check loop that keeps the USB AT bridge
// open, watches cellular registration, and escalates through gentle recovery
// steps when the module loses the network for a sustained period.
func (a *app) startSignalRecovery(ctx context.Context) {
	const checkInterval = 8 * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.signalRecoveryOnce()
		}
	}
}

func (a *app) signalRecoveryOnce() {
	if a.demo || a.modem != nil || !a.autoSignalRecovery {
		return
	}
	if err := a.ensureUSBAT(); err != nil {
		return
	}
	if !a.hasUSBAT() {
		return
	}

	reg, signal, err := a.probeCellularHealth()
	if err != nil {
		// The USB AT channel itself broke; let the next cycle re-open it.
		return
	}
	if reg == 1 || reg == 5 {
		if a.lostSignalCount > 0 {
			log.Printf("cellular signal recovered: registration=%d signal=%d dBm", reg, signal)
		}
		a.lostSignalCount = 0
	} else {
		a.lostSignalCount++
		log.Printf("cellular signal lost %d/3 checks: registration=%d signal=%d dBm", a.lostSignalCount, reg, signal)
		if a.lostSignalCount >= 3 {
			a.recoverSignal()
		}
	}

	// The USB network interface rarely drops by itself, so only re-check
	// DHCP every minute to avoid hammering networksetup.
	if time.Since(a.lastNetworkCheck) >= 60*time.Second {
		a.lastNetworkCheck = time.Now()
		a.ensureCellularDHCP()
	}
}

// probeCellularHealth cheaply reads registration and signal without running
// the full status command set.
func (a *app) probeCellularHealth() (reg int, signalDBM int, err error) {
	cregResp, err := a.commandUSBAT("AT+CREG?", 3*time.Second)
	if err != nil {
		return 0, 0, err
	}
	ceregResp, _ := a.commandUSBAT("AT+CEREG?", 3*time.Second)
	csqResp, _ := a.commandUSBAT("AT+CSQ", 3*time.Second)
	return firstNonZeroRegistration(cregResp, ceregResp), parseUSBATCSQDBM(csqResp), nil
}

// recoverSignal escalates step by step after the module has been without a
// network for several consecutive checks: re-attach, radio cycle, and finally
// a throttled full module reboot.
func (a *app) recoverSignal() {
	if !a.autoSignalRecovery {
		return
	}
	if !a.moduleMutationMu.TryLock() {
		log.Printf("cellular signal recovery skipped: another module mutation is active")
		return
	}
	defer a.moduleMutationMu.Unlock()
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if !a.hasUSBAT() {
		return
	}
	switch {
	case a.lostSignalCount == 3:
		log.Printf("cellular signal recovery: forcing PS attach and automatic network selection")
		if _, attempted, err := a.runFreshNoCallModuleCommand("AT+CGATT=1", 5*time.Second); err != nil || !attempted {
			log.Printf("cellular signal recovery attach skipped: %v", err)
			return
		}
		_, _, _ = a.runFreshNoCallModuleCommand("AT+COPS=0", 5*time.Second)
	case a.lostSignalCount == 6:
		log.Printf("cellular signal recovery: cycling radio (AT+CFUN=0 then 1)")
		if _, attempted, err := a.runFreshNoCallModuleCommand("AT+CFUN=0", 5*time.Second); err != nil || !attempted {
			log.Printf("cellular signal recovery radio cycle skipped: %v", err)
			return
		}
		time.Sleep(3 * time.Second)
		if a.hasUSBAT() {
			_, _, _ = a.runFreshNoCallModuleCommand("AT+CFUN=1", 10*time.Second)
		}
	case a.lostSignalCount >= 9 && time.Since(a.lastModemReboot) >= 10*time.Minute:
		a.lastModemReboot = time.Now()
		log.Printf("cellular signal recovery: rebooting module (AT+CFUN=1,1)")
		_, attempted, execution, _ := a.runFreshNoCallModuleCommandExact("AT+CFUN=1,1", 3*time.Second)
		if attempted {
			a.markUSBATExecutionDetached(execution, "cellular signal recovery reboot")
		}
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/platform", a.platformInfo)
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/sms", a.listSMS)
	mux.HandleFunc("GET /api/sms/status", a.smsStatus)
	mux.HandleFunc("PATCH /api/sms/settings", a.updateSMSSettings)
	mux.HandleFunc("GET /api/sim/identity", a.simIdentity)
	mux.HandleFunc("POST /api/sms/send", a.sendSMS)
	mux.HandleFunc("POST /api/sms/refresh", a.refreshSMS)
	mux.HandleFunc("POST /api/sms/clear-module", a.clearModuleSMS)
	mux.HandleFunc("GET /api/calls/status", a.callStatus)
	mux.HandleFunc("POST /api/calls/reject", a.rejectCall)
	mux.HandleFunc("POST /api/calls/answer", a.answerCall)
	mux.HandleFunc("POST /api/calls/hangup", a.hangupCall)
	mux.HandleFunc("POST /api/calls/dtmf", a.dtmfCall)
	mux.HandleFunc("POST /api/calls/dial", a.dialCall)
	mux.HandleFunc("POST /api/calls/audio/start", a.audioStart)
	mux.HandleFunc("POST /api/calls/audio/stop", a.audioStop)
	mux.HandleFunc("POST /api/calls/audio/mute", a.audioMute)
	mux.HandleFunc("POST /api/calls/audio/record", a.audioRecord)
	mux.HandleFunc("POST /api/calls/audio/host/register", a.audioHostRegister)
	mux.HandleFunc("GET /api/calls/audio/host/config", a.audioHostConfig)
	mux.HandleFunc("POST /api/calls/audio/host/confirm", a.audioHostConfirm)
	mux.HandleFunc("GET /api/voice/status", a.voiceStatusAPI)
	mux.HandleFunc("GET /api/module/setup", a.moduleSetupStatusAPI)
	mux.HandleFunc("POST /api/module/setup", a.moduleSetupStartAPI)
	mux.HandleFunc("POST /api/voice/start", a.voiceStartAPI)
	mux.HandleFunc("POST /api/voice/stop", a.voiceStopAPI)
	mux.HandleFunc("GET /api/gps", a.gpsStatus)
	mux.HandleFunc("POST /api/gps/start", a.startGPS)
	mux.HandleFunc("POST /api/gps/stop", a.stopGPS)
	mux.HandleFunc("POST /api/gps/refresh", a.refreshGPS)
	mux.HandleFunc("POST /api/at", a.executeAT)
	mux.HandleFunc("GET /api/network", a.networkDiagnostic)
	mux.HandleFunc("GET /api/network/traffic", a.networkTraffic)
	mux.HandleFunc("GET /api/network/cellular-policy", a.getCellularPolicy)
	mux.HandleFunc("POST /api/network/cellular-policy", a.setCellularPolicy)
	mux.HandleFunc("POST /api/network/check-4g", a.check4GRoute)
	mux.HandleFunc("POST /api/network/check-proxy", a.checkProxyRoute)
	mux.HandleFunc("POST /api/network/usbnet", a.setUSBNetMode)
	mux.HandleFunc("POST /api/network/reboot-module", a.rebootModule)
	mux.HandleFunc("GET /api/esim", a.esimOverview)
	mux.HandleFunc("GET /api/esim/notes", a.listESIMNotes)
	mux.HandleFunc("PUT /api/esim/notes", a.saveESIMNote)
	mux.HandleFunc("GET /api/esim/module-notes", a.listModuleESIMNotes)
	mux.HandleFunc("PUT /api/esim/module-notes", a.saveModuleESIMNote)
	mux.HandleFunc("GET /api/esim/health", a.esimHealth)
	mux.HandleFunc("POST /api/esim/phonebook/probe", a.probeESIMPhonebook)
	mux.HandleFunc("POST /api/esim/switch", a.switchESIM)
	mux.HandleFunc("PATCH /api/esim/profile", a.renameESIMProfile)
	mux.HandleFunc("DELETE /api/esim/profile", a.deleteESIMProfile)
	mux.HandleFunc("POST /api/esim/download", a.downloadESIMProfile)
	// Native enrollment administration exists only on the full loopback API.
	// The enrolled device protocol itself is served only by the dedicated
	// Tailscale Serve listener below.
	mux.HandleFunc("POST /api/local/v1/native/enrollment/tickets", a.nativeLocalTicketAPI)
	mux.HandleFunc("POST /api/local/v1/native/devices/revoke", a.nativeLocalRevokeAPI)
	mux.HandleFunc("/api/local/v1/native/", http.NotFound)
	mux.HandleFunc("/api/native/", http.NotFound)
	// The remote gateway is served only by the dedicated loopback listener on
	// port 7577. Keep these paths explicitly absent from the full local API so a
	// future catch-all UI handler cannot accidentally expose them on 7576.
	mux.HandleFunc("/api/remote/", http.NotFound)
	mux.HandleFunc("/remote", http.NotFound)
	mux.HandleFunc("/remote/", http.NotFound)
	if runtime.GOOS == "windows" || a.webConsole {
		assets, err := fs.Sub(webAssets, "web")
		if err != nil {
			panic(fmt.Sprintf("open embedded web console: %v", err))
		}
		mux.Handle("/", http.FileServer(http.FS(assets)))
		return securityHeaders(mux)
	}

	// macOS 日常操作迁移到独立 App；根路径保留兼容提示。
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("MacCellular 已迁移到 macOS 应用，请使用 MacCellular App 完成全部操作。"))
	})
	return securityHeaders(mux)
}

func (a *app) platformInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":               "1.0.0-rc.4",
		"os":                    runtime.GOOS,
		"web_console":           runtime.GOOS == "windows" || a.webConsole,
		"call_audio":            runtime.GOOS == "darwin",
		"direct_usb_at":         runtime.GOOS == "darwin",
		"esim_full":             runtime.GOOS == "darwin",
		"network_policy_native": runtime.GOOS == "darwin",
		"native_contacts":       runtime.GOOS == "darwin",
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// This controller can dial, send SMS and write persistent modem state.
		// Reject DNS rebinding and cross-site browser requests even though the
		// listener defaults to loopback. Native clients omit Origin; the embedded
		// console uses a loopback same-origin URL.
		isLoopback := isLoopbackHTTPHost(r.Host)
		if !isLoopback {
			http.Error(w, "loopback host required", http.StatusForbidden)
			return
		}
		if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			parsed, err := url.Parse(origin)
			originOK := err == nil && isLoopbackHTTPHost(parsed.Host)
			if !originOK {
				http.Error(w, "same-origin request required", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHTTPHost(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	host := raw
	if parsed, _, err := net.SplitHostPort(raw); err == nil {
		host = parsed
	} else {
		host = strings.Trim(raw, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	return err == nil && isLoopbackHTTPHost(host)
}

func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	_ = a.currentUSBDevice()
	deviceState := a.snapshotDeviceState()
	esimManager, _ := a.currentESIMManager()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "port": deviceState.Port, "esim_available": a.demo || esimManager != nil, "demo": a.demo,
		"usb_device": deviceState.USBDevice, "discovery_error": deviceState.DiscoveryError,
	})
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, modem.DeviceStatus{
			IMEI:          "867400000000001",
			Firmware:      "EG25GGBR07A08M2G",
			ICCID:         "89860123456789012345",
			IMSI:          "460001234567890",
			Operator:      "中国移动",
			SimInserted:   true,
			SignalDBM:     -73,
			SignalRSRP:    -96,
			SignalRSRQ:    -9,
			RegStatus:     1,
			RegStatusText: "已注册",
			NetworkMode:   "LTE",
			NetworkDuplex: "FDD",
			RadioBand:     "B3",
			USBNetMode:    0,
		})
		return
	}
	if a.modem == nil {
		// A libusb handle may survive a physical unplug. Refresh the macOS USB
		// inventory before using it so the UI never reports a stale connection.
		observedUSBAT := a.currentUSBATSnapshot()
		if observedUSBAT != nil && a.currentUSBDevice() == nil {
			// Inventory discovery is not atomic with a USB re-enumeration. Only
			// detach the exact lifecycle object observed before the scan; a
			// replacement opened while the scan ran must remain untouched.
			a.markUSBATDeviceDetached(observedUSBAT, "DJI USB device disconnected")
		}
		if err := a.ensureUSBAT(); err != nil {
			log.Printf("USB AT retry failed: %v", err)
		}
		if a.hasUSBAT() {
			status, err := a.usbATStatus()
			if err == nil {
				writeJSON(w, http.StatusOK, status)
				return
			}
			log.Printf("USB AT status failed: %v", err)
		}
		_ = a.currentUSBDevice()
		deviceState := a.snapshotDeviceState()
		usbDevice := deviceState.USBDevice
		summary := "未发现 AT 串口"
		operator := "未连接"
		network := "不可用"
		if usbDevice != nil {
			summary = fmt.Sprintf("%s %s (%s:%s)", usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			operator = "已检测到 USB 设备"
			network = usbDevice.Mode
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"operator":        operator,
			"signal_dbm":      nil,
			"network_mode":    network,
			"sim_inserted":    false,
			"hardware_status": summary,
			"discovery_error": deviceState.DiscoveryError,
			"usb_device":      usbDevice,
		})
		return
	}
	writeJSON(w, http.StatusOK, a.modem.GetFullStatus())
}

func (a *app) currentUSBDevice() *usbDeviceStatus {
	if a.modem != nil || a.demo {
		return a.snapshotDeviceState().USBDevice
	}
	discover := a.discoverUSBDevice
	if discover == nil {
		discover = discoverDJIUSBDevice
	}
	usbDevice := discover()
	// Never retain the last successful scan: that is stale after an unplug.
	a.setUSBDeviceState(usbDevice)
	return cloneUSBDeviceStatus(usbDevice)
}

func (a *app) usbATStatus() (modem.DeviceStatus, error) {
	firmwareResp, _ := a.commandUSBAT("ATI", 3*time.Second)
	cpinResp, cpinErr := a.commandUSBAT("AT+CPIN?", 3*time.Second)
	csqResp, _ := a.commandUSBAT("AT+CSQ", 3*time.Second)
	ceregResp, _ := a.commandUSBAT("AT+CEREG?", 3*time.Second)
	cregResp, _ := a.commandUSBAT("AT+CREG?", 3*time.Second)
	copsResp, _ := a.commandUSBAT("AT+COPS?", 3*time.Second)
	qccidResp, _ := a.commandUSBAT("AT+QCCID", 3*time.Second)
	cimiResp, _ := a.commandUSBAT("AT+CIMI", 3*time.Second)
	qnwinfoResp, _ := a.commandUSBAT("AT+QNWINFO", 3*time.Second)
	usbnetResp, _ := a.commandUSBAT(`AT+QCFG="usbnet"`, 3*time.Second)
	cgattResp, _ := a.commandUSBAT("AT+CGATT?", 3*time.Second)
	cgdcontResp, _ := a.commandUSBAT("AT+CGDCONT?", 3*time.Second)
	qimsResp, _ := a.commandUSBAT("AT+QIMS?", 3*time.Second)

	if cpinErr != nil {
		return modem.DeviceStatus{}, cpinErr
	}

	regStatus := firstNonZeroRegistration(ceregResp, cregResp)
	mode, duplex, band, channel := parseUSBATQNWInfo(qnwinfoResp)
	usbnetMode := -1
	if parsedMode, err := strconv.Atoi(parseUSBNetMode(usbnetResp)); err == nil {
		usbnetMode = parsedMode
	}
	status := modem.DeviceStatus{
		Firmware:      parseUSBATFirmware(firmwareResp),
		ICCID:         parseUSBATPrefixed(qccidResp, "+QCCID:"),
		IMSI:          parseUSBATIMSI(cimiResp),
		Operator:      modem.NormalizeServingOperatorName(parseUSBATOperator(copsResp), parseUSBATIMSI(cimiResp)),
		SimInserted:   strings.Contains(strings.ToUpper(cpinResp), "READY"),
		SignalDBM:     parseUSBATCSQDBM(csqResp),
		RegStatus:     regStatus,
		RegStatusText: registrationText(regStatus),
		NetworkMode:   mode,
		NetworkDuplex: duplex,
		RadioBand:     band,
		RadioChannel:  channel,
		USBNetMode:    usbnetMode,
		PSAttached:    parseUSBATPSAttached(cgattResp),
		APN:           parseUSBATPrimaryAPN(cgdcontResp),
		IMSStatus:     parseUSBATIMSStatus(qimsResp),
	}
	if status.Operator == "" && strings.Contains(copsResp, "CHN-UNICOM") {
		status.Operator = "中国联通"
	}
	return status, nil
}

func parseUSBATPSAttached(response string) bool {
	return regexp.MustCompile(`(?m)^\+CGATT:\s*1\s*$`).MatchString(strings.ReplaceAll(response, "\r", ""))
}

func parseUSBATPrimaryAPN(response string) string {
	for _, context := range parsePDPContexts(response) {
		if context.ID == 1 {
			return context.APN
		}
	}
	return ""
}

func parseUSBATIMSStatus(response string) int {
	match := regexp.MustCompile(`\+QIMS:\s*(\d+)`).FindStringSubmatch(response)
	if len(match) != 2 {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

func parseUSBATFirmware(resp string) string {
	lines := splitATLines(resp)
	var useful []string
	for _, line := range lines {
		up := strings.ToUpper(line)
		if strings.HasPrefix(up, "ATI") || up == "OK" {
			continue
		}
		useful = append(useful, line)
	}
	return strings.Join(useful, " · ")
}

func splitATLines(resp string) []string {
	resp = strings.ReplaceAll(resp, "\r", "\n")
	raw := strings.Split(resp, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseUSBATPrefixed(resp, prefix string) string {
	for _, line := range splitATLines(resp) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func parseUSBATIMSI(resp string) string {
	for _, line := range splitATLines(resp) {
		up := strings.ToUpper(line)
		if up == "OK" || strings.HasPrefix(up, "AT") {
			continue
		}
		if _, err := strconv.ParseUint(line, 10, 64); err == nil && len(line) >= 5 {
			return line
		}
	}
	return ""
}

func parseUSBATCSQDBM(resp string) int {
	re := regexp.MustCompile(`\+CSQ:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	rssi, err := strconv.Atoi(match[1])
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + 2*rssi
}

func parseUSBATOperator(resp string) string {
	re := regexp.MustCompile(`\+COPS:\s*\d+,\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func firstNonZeroRegistration(responses ...string) int {
	for _, resp := range responses {
		re := regexp.MustCompile(`\+(?:CE)?REG:\s*\d+,(\d+)`)
		match := re.FindStringSubmatch(resp)
		if len(match) != 2 {
			continue
		}
		status, err := strconv.Atoi(match[1])
		if err == nil && status != 0 {
			return status
		}
	}
	return 0
}

func registrationText(status int) string {
	switch status {
	case 1:
		return "已注册"
	case 5:
		return "漫游注册"
	case 2:
		return "搜索中"
	case 3:
		return "注册被拒绝"
	default:
		return "未注册"
	}
}

func parseUSBATQNWInfo(resp string) (mode, duplex, band string, channel uint32) {
	re := regexp.MustCompile(`\+QNWINFO:\s*"([^"]*)","[^"]*","([^"]*)",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 4 {
		return "", "", "", 0
	}
	mode = match[1]
	radio := match[2]
	if strings.Contains(strings.ToUpper(mode), "FDD") {
		duplex = "FDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "FDD"))
	}
	if strings.Contains(strings.ToUpper(mode), "TDD") {
		duplex = "TDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "TDD"))
	}
	band = strings.TrimPrefix(radio, "LTE ")
	if value, err := strconv.ParseUint(match[3], 10, 32); err == nil {
		channel = uint32(value)
	}
	return mode, duplex, band, channel
}

func (a *app) readUSBATSMS() ([]receivedSMS, error) {
	if _, err := a.commandUSBAT("AT+CMGF=0", 3*time.Second); err != nil {
		return nil, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	memories := []string{"SM", "ME"}
	seen := make(map[string]bool)
	messages := make([]receivedSMS, 0)
	var errs []string
	for _, memory := range memories {
		items, err := a.readUSBATSMSFromMemory(memory)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", memory, err))
			continue
		}
		for _, item := range items {
			key := item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
			if seen[key] {
				continue
			}
			seen[key] = true
			messages = append(messages, item)
		}
	}
	if len(messages) == 0 && len(errs) == len(memories) {
		return nil, fmt.Errorf("list SMS failed: %s", strings.Join(errs, "; "))
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) readUSBATSMSFromMemory(memory string) ([]receivedSMS, error) {
	if _, err := a.commandUSBAT(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second); err != nil {
		return nil, fmt.Errorf("select storage: %w", err)
	}
	resp, err := a.commandUSBAT("AT+CMGL=4", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("list SMS: %w", err)
	}
	pdus := parseUSBATCMGL(resp)
	messages := make([]receivedSMS, 0, len(pdus))
	parseFailures := 0
	for _, item := range pdus {
		msg, concat, err := decodeUSBATPDU(item.header, item.pdu)
		if err != nil {
			parseFailures++
			continue
		}
		if concat.IsConcat {
			if a.smsReassembler == nil {
				a.smsReassembler = smscodec.NewReassembler()
			}
			complete, content := a.smsReassembler.Add(msg.Sender, concat, msg.Content)
			if !complete {
				continue
			}
			msg.Content = content
		}
		messages = append(messages, msg)
	}
	if a.smsReassembler != nil {
		a.smsReassembler.Cleanup(10 * time.Minute)
	}
	if parseFailures > 0 {
		log.Printf("USB AT SMS decode skipped invalid message(s): count=%d error_code=invalid_pdu", parseFailures)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) clearUSBATSMSMemory(memory string) (before, after int, err error) {
	resp, err := a.commandUSBAT(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("select storage: %w", err)
	}
	before = parseUSBATCPMSUsed(resp)
	if _, err := a.commandUSBAT("AT+CMGD=1,4", 20*time.Second); err != nil {
		return before, 0, fmt.Errorf("delete messages: %w", err)
	}
	resp, err = a.commandUSBAT(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return before, 0, fmt.Errorf("recheck storage: %w", err)
	}
	after = parseUSBATCPMSUsed(resp)
	return before, after, nil
}

type smsStorageClearResult struct {
	Memory string `json:"memory"`
	Before int    `json:"before"`
	After  int    `json:"after"`
}

// clearAllUSBATSMS removes messages from both stores surfaced by readUSBATSMS.
// The previous implementation only cleared ME while the inbox also showed SM,
// which made a successful delete look like it had done nothing.
func (a *app) clearAllUSBATSMS() ([]smsStorageClearResult, error) {
	results := make([]smsStorageClearResult, 0, 2)
	for _, memory := range []string{"SM", "ME"} {
		before, after, err := a.clearUSBATSMSMemory(memory)
		if err != nil {
			return results, fmt.Errorf("clear %s SMS: %w", memory, err)
		}
		results = append(results, smsStorageClearResult{
			Memory: memory,
			Before: before,
			After:  after,
		})
	}
	return results, nil
}

func parseUSBATCPMSUsed(resp string) int {
	re := regexp.MustCompile(`\+CPMS:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	used, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return used
}

type usbATSMSPDU struct {
	header string
	pdu    string
}

func parseUSBATCMGL(resp string) []usbATSMSPDU {
	lines := splitATLines(resp)
	var out []usbATSMSPDU
	for i := 0; i < len(lines)-1; i++ {
		if !strings.HasPrefix(lines[i], "+CMGL:") {
			continue
		}
		next := strings.TrimSpace(lines[i+1])
		if !smscodec.IsHexString(next) {
			continue
		}
		pdu, _ := smscodec.TrimFullPDUHexByATHeader(next, lines[i])
		out = append(out, usbATSMSPDU{header: lines[i], pdu: pdu})
		i++
	}
	return out
}

func decodeUSBATPDU(header, pduHex string) (receivedSMS, smscodec.ConcatInfo, error) {
	raw := strings.TrimSpace(pduHex)
	if trimmed, ok := smscodec.TrimFullPDUHexByATHeader(raw, header); ok {
		raw = trimmed
	}
	full, err := hex.DecodeString(raw)
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if len(full) < 2 {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU too short")
	}
	smscLen := int(full[0])
	tpduOffset := 1 + smscLen
	if tpduOffset >= len(full) {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU has invalid SMSC length")
	}
	sender, content, timestamp, concat, err := smscodec.DecodeDeliverTPDU(full[tpduOffset:])
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return receivedSMS{Sender: sender, Content: content, Timestamp: timestamp}, concat, nil
}

func (a *app) listSMS(w http.ResponseWriter, _ *http.Request) {
	a.smsMu.RLock()
	items := append([]receivedSMS(nil), a.sms...)
	a.smsMu.RUnlock()
	if items == nil {
		items = []receivedSMS{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) smsStatus(w http.ResponseWriter, _ *http.Request) {
	a.smsMu.RLock()
	lastPoll := a.smsLastPoll
	lastPollError := a.smsLastPollError
	count := len(a.sms)
	a.smsMu.RUnlock()
	a.smsOperationMu.Lock()
	autoCleanupME := a.smsAutoCleanupME
	a.smsOperationMu.Unlock()
	storeAvailable, storeError := a.smsStoreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":           count,
		"polling":         !a.demo && a.modem == nil,
		"poll_interval_s": int(a.smsPollInterval.Seconds()),
		"auto_cleanup_me": autoCleanupME,
		"last_poll":       lastPoll,
		"last_poll_error": lastPollError,
		"durable_store":   storeAvailable,
		"store_error":     storeError,
	})
}

func (a *app) updateSMSSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoCleanupME *bool `json:"auto_cleanup_me"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AutoCleanupME == nil {
		writeError(w, http.StatusBadRequest, "auto_cleanup_me is required")
		return
	}
	if *body.AutoCleanupME {
		writeError(w, http.StatusConflict, "自动删除已停用；请在确认本地短信历史完整后手动清理模块存储")
		return
	}
	a.smsOperationMu.Lock()
	a.smsAutoCleanupME = false
	a.smsOperationMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"auto_cleanup_me": false})
}

func (a *app) simIdentity(w http.ResponseWriter, r *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"phone_number": "+8613800138000"})
		return
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	response, err := a.runATCommand("AT+CNUM", 3*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phone_number": parseCNUM(response)})
}

func parseCNUM(response string) string {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "+CNUM:") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line[len("+CNUM:"):]), ",")
		if len(fields) < 2 {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(fields[1]), `"`)
		digits := regexp.MustCompile(`[^0-9+]`).ReplaceAllString(raw, "")
		if len(strings.TrimPrefix(digits, "+")) >= 5 {
			return digits
		}
	}
	return ""
}

func (a *app) refreshSMS(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	if a.modem == nil {
		if err := a.pollSMSOnce(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		a.smsMu.RLock()
		count := len(a.sms)
		a.smsMu.RUnlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "count": count})
		return
	}
	go a.modem.CheckAllSMS()
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (a *app) clearModuleSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "删除模块短信需要明确确认")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true, "before": 0, "after": 0, "storages": []string{"SM", "ME"}})
		return
	}
	if a.modem != nil {
		writeError(w, http.StatusServiceUnavailable, "module SMS cleanup is only available through USB AT")
		return
	}
	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "AT serial port is unavailable: "+err.Error())
		return
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	results, err := a.clearAllUSBATSMS()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	before, after := 0, 0
	for _, result := range results {
		before += result.Before
		after += result.After
	}
	// Deleting module storage must not erase the durable/local history view.
	// The user explicitly confirmed only the SIM/module mutation here.
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared":  true,
		"before":   before,
		"after":    after,
		"storages": results,
	})
}

func (a *app) runATCommand(command string, timeout time.Duration) (string, error) {
	if a.demo {
		responses := map[string]string{
			"AT":                 "OK",
			"AT+CSQ":             "+CSQ: 22,99\r\nOK",
			"AT+COPS?":           "+COPS: 0,0,\"China Mobile\",7\r\nOK",
			"AT+QNWINFO":         "+QNWINFO: \"FDD LTE\",\"46000\",\"LTE BAND 3\",1650\r\nOK",
			"AT+QCFG=\"USBNET\"": "+QCFG: \"usbnet\",1\r\nOK",
			"AT+QCFG=\"USBCFG\"": "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
			"AT+CGDCONT?":        "+CGDCONT: 1,\"IPV4V6\",\"3gnet\",\"0.0.0.0\",0,0,0,0\r\nOK",
			"AT+CGACT?":          "+CGACT: 1,1\r\nOK",
			"AT+CGPADDR=1":       "+CGPADDR: 1,\"10.23.45.67\"\r\nOK",
		}
		response := responses[strings.ToUpper(strings.TrimSpace(command))]
		if response == "" {
			response = "OK"
		}
		return response, nil
	}
	if a.modem == nil {
		if err := a.ensureUSBAT(); err != nil {
			return "", err
		}
		if !a.hasUSBAT() {
			return "", errors.New("AT serial port is unavailable")
		}
		return a.commandUSBAT(command, timeout)
	}
	return a.modem.ExecuteAT(command, timeout)
}

func (a *app) sendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Phone) == "" || strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "phone and message are required")
		return
	}
	if a.demo {
		a.recordSMS("已发送至 "+body.Phone, body.Message, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": 1})
		return
	}
	outgoing, err := a.beginOutgoingSMS(strings.TrimSpace(body.Phone), body.Message, time.Now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "短信持久存储不可用，未发送")
		return
	}
	segments, err := a.sendTextSMS(body.Phone, body.Message)
	if err != nil {
		_ = a.markOutgoingSMSUnknown(outgoing.ID, segments)
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "code": "unknown_outcome", "error": "短信提交结果无法确认；为避免重复发送，请先核对记录",
		})
		return
	}
	if _, err := a.updateOutgoingSMS(outgoing.ID, "submitted", segments); err != nil {
		_ = a.markOutgoingSMSUnknown(outgoing.ID, segments)
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "code": "unknown_outcome", "error": "短信已提交，但持久状态无法确认；请勿重复发送",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": segments})
}

func (a *app) sendTextSMS(phone, message string) (int, error) {
	a.smsSendMu.Lock()
	defer a.smsSendMu.Unlock()
	if a.sendTextSMSOverride != nil {
		return a.sendTextSMSOverride(phone, message)
	}
	if a.modem == nil {
		return a.sendUSBATSMS(phone, message)
	}
	if err := a.modem.SendSMSWithOptions(phone, message, smsSubmitOptions(message)); err != nil {
		return 0, err
	}
	return 1, nil
}

func (a *app) sendUSBATSMS(phone, message string) (int, error) {
	if err := a.ensureUSBAT(); err != nil {
		return 0, err
	}
	if !a.hasUSBAT() {
		return 0, errors.New("AT serial port is unavailable")
	}

	modeResponse, err := a.commandUSBAT("AT+CMGF=0", 5*time.Second)
	if err != nil {
		return 0, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	if !atProbeSucceeded(modeResponse) {
		return 0, fmt.Errorf("set SMS PDU mode failed: %s", modeResponse)
	}

	tpdus, tpduLengths, err := smscodec.BuildSubmitTPDUsWithOptions(phone, message, smsSubmitOptions(message))
	if err != nil {
		return 0, fmt.Errorf("build SMS PDU: %w", err)
	}
	for i, tpdu := range tpdus {
		pdu := append([]byte{0x00}, tpdu...)
		payload := []byte(strings.ToUpper(hex.EncodeToString(pdu)) + "\x1a")
		response, sendErr := a.commandUSBATWithPrompt(
			fmt.Sprintf("AT+CMGS=%d", tpduLengths[i]),
			payload,
			45*time.Second,
		)
		if sendErr != nil {
			return i, fmt.Errorf("send SMS segment %d/%d: %w", i+1, len(tpdus), sendErr)
		}
		if atResponseIsError(response) || !strings.Contains(response, "+CMGS:") || !atProbeSucceeded(response) {
			return i, fmt.Errorf("send SMS segment %d/%d failed: %s", i+1, len(tpdus), response)
		}
		if i+1 < len(tpdus) {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return len(tpdus), nil
}

func smsSubmitOptions(message string) smscodec.SubmitOptions {
	for _, r := range message {
		if r > 127 {
			return smscodec.SubmitOptions{Encoding: smscodec.SMSEncodingUCS2}
		}
	}
	return smscodec.SubmitOptions{}
}

func (a *app) executeAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !isSafeDiagnosticATCommand(body.Command) {
		writeError(w, http.StatusForbidden, "AT 控制台仅允许诊断查询；持久写入、拨号和重启必须使用对应的确认控件")
		return
	}
	if a.demo {
		response, _ := a.runATCommand(body.Command, 20*time.Second)
		writeJSON(w, http.StatusOK, map[string]string{"response": response})
		return
	}
	response, err := a.runATCommand(body.Command, 20*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response})
}

func isSafeDiagnosticATCommand(command string) bool {
	command = strings.ToUpper(strings.TrimSpace(command))
	if command == "" || len(command) > 128 || strings.ContainsAny(command, "\r\n;") {
		return false
	}
	_, allowed := map[string]struct{}{
		"AT": {}, "ATI": {},
		"AT+CGSN": {}, "AT+CIMI": {}, "AT+QCCID": {}, "AT+CNUM": {},
		"AT+CSQ": {}, "AT+QNWINFO": {}, "AT+CGMR": {}, "AT+GMR": {},
		"AT+CPIN?": {}, "AT+COPS?": {}, "AT+CREG?": {}, "AT+CEREG?": {},
		"AT+CGATT?": {}, "AT+CGDCONT?": {}, "AT+CGACT?": {}, "AT+CGPADDR=1": {},
		"AT+CFUN?": {}, "AT+QSIMSTAT?": {}, "AT+QIMS?": {}, "AT+QGPS?": {},
		"AT+CPMS?": {}, "AT+CSCA?": {}, "AT+CLCC": {},
		"AT+QPCMV?": {}, "AT+QPCMV=?": {}, "AT+QDAI?": {},
		`AT+QCFG="USBCFG"`: {}, `AT+QCFG="USBNET"`: {},
		`AT+QCFG="IMS"`: {}, `AT+QCFG="VOLTE_DISABLE"`: {},
	}[command]
	return allowed
}

func (a *app) networkDiagnostic(w http.ResponseWriter, _ *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("network diagnostic panic: %v", recovered)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("network diagnostic failed: %v", recovered))
		}
	}()
	raw := make(map[string]string)
	errs := make(map[string]string)
	diag := networkDiagnostic{
		USBDevice:     a.currentUSBDevice(),
		MacInterfaces: discoverMacNetworkInterfaces(),
		DefaultRoute:  discoverMacDefaultRoute(),
		Raw:           raw,
		Errors:        errs,
	}
	diag.USBNetworkPresent = hasLikelyUSBNetworkInterface(diag.MacInterfaces)

	commands := map[string]string{
		"usbnet":  `AT+QCFG="usbnet"`,
		"usbcfg":  `AT+QCFG="usbcfg"`,
		"cgdcont": `AT+CGDCONT?`,
		"cgact":   `AT+CGACT?`,
		"cgpaddr": `AT+CGPADDR=1`,
	}
	for key, command := range commands {
		resp, err := a.runATCommand(command, 8*time.Second)
		if err != nil {
			errs[key] = err.Error()
			continue
		}
		raw[key] = resp
	}

	diag.USBNetMode = parseUSBNetMode(raw["usbnet"])
	diag.USBCfg = parseUSBATPrefixed(raw["usbcfg"], "+QCFG:")
	diag.PDPContexts = parsePDPContexts(raw["cgdcont"])
	diag.ActiveContexts = parseActivePDPContexts(raw["cgact"])
	diag.PDPAddresses = parsePDPAddresses(raw["cgpaddr"])
	if len(errs) == 0 {
		diag.Errors = nil
	}
	writeJSON(w, http.StatusOK, diag)
}

func (a *app) networkTraffic(w http.ResponseWriter, _ *http.Request) {
	snapshot := networkTrafficSnapshot{
		SampledAtMS: time.Now().UnixMilli(),
	}
	if policy, err := a.cellularPolicyStatus(); err == nil && policy.ForceOff {
		snapshot.Error = "4G 已关闭"
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	interfaces := discoverMacNetworkInterfaces()
	name := selectUSBTrafficInterface(interfaces, discoverMacDefaultRoute())
	if name == "" {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	counters, err := discoverMacInterfaceCounters()
	if err != nil {
		snapshot.Interface = name
		snapshot.Error = err.Error()
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	current, ok := counters[name]
	if !ok {
		snapshot.Interface = name
		snapshot.Error = "未读取到网卡计数"
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	a.trafficMu.Lock()
	if a.trafficBaselines == nil {
		a.trafficBaselines = make(map[string]networkByteCounters)
	}
	baseline, exists := a.trafficBaselines[name]
	if !exists || current.RX < baseline.RX || current.TX < baseline.TX {
		baseline = current
		a.trafficBaselines[name] = baseline
	}
	a.trafficMu.Unlock()

	snapshot.Available = true
	snapshot.Interface = name
	snapshot.RXBytes = current.RX
	snapshot.TXBytes = current.TX
	snapshot.SessionRX, snapshot.SessionTX, snapshot.SessionTotal = sessionTrafficFromCounters(current, baseline)
	writeJSON(w, http.StatusOK, snapshot)
}

func sessionTrafficFromCounters(current, baseline networkByteCounters) (rx, tx, total uint64) {
	rx = current.RX - baseline.RX
	tx = current.TX - baseline.TX
	return rx, tx, rx + tx
}

func (a *app) check4GRoute(w http.ResponseWriter, _ *http.Request) {
	route := discoverMacDefaultRoute()
	interfaces := discoverMacNetworkInterfaces()
	var active *macNetInterface
	for i := range interfaces {
		if interfaces[i].Name == route.Interface {
			active = &interfaces[i]
			break
		}
	}
	if route.Interface == "" {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      false,
			Summary: "未读取到默认出口",
			Detail:  "macOS 没有返回 default route",
		})
		return
	}
	if active != nil && active.Name != "en0" && active.Kind == "ethernet" && active.Status == "active" {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      true,
			Summary: "当前正在走 4G 模块",
			Detail:  fmt.Sprintf("默认出口 %s -> %s，IP %s", route.Interface, route.Gateway, active.IPv4),
		})
		return
	}
	detail := fmt.Sprintf("默认出口 %s -> %s", route.Interface, route.Gateway)
	if active != nil && active.IPv4 != "" {
		detail += "，IP " + active.IPv4
	}
	writeJSON(w, http.StatusOK, networkCheckResult{
		OK:      false,
		Summary: "当前没有优先走 4G 模块",
		Detail:  detail,
	})
}

func (a *app) checkProxyRoute(w http.ResponseWriter, _ *http.Request) {
	proxyURL, _ := url.Parse("http://127.0.0.1:7890")
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	req, err := http.NewRequest(http.MethodHead, "https://www.google.com/generate_204", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, networkCheckResult{OK: false, Summary: "代理检测请求创建失败", Detail: err.Error()})
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      false,
			Summary: "代理未打通",
			Detail:  "127.0.0.1:7890 代理访问失败：" + err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 200 && resp.StatusCode < 400) {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      true,
			Summary: "代理已打通",
			Detail:  fmt.Sprintf("127.0.0.1:7890 -> google generate_204 返回 %s", resp.Status),
		})
		return
	}
	writeJSON(w, http.StatusOK, networkCheckResult{
		OK:      false,
		Summary: "代理响应异常",
		Detail:  fmt.Sprintf("127.0.0.1:7890 返回 %s", resp.Status),
	})
}

type macNetworkService struct {
	Name         string
	HardwarePort string
	Device       string
	Disabled     bool
}

func discoverMacNetworkServices() ([]macNetworkService, error) {
	out, err := exec.Command("/usr/sbin/networksetup", "-listnetworkserviceorder").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("读取 macOS 网络服务失败: %s", strings.TrimSpace(string(out)))
	}
	return parseMacNetworkServices(string(out)), nil
}

func parseMacNetworkServices(output string) []macNetworkService {
	header := regexp.MustCompile(`^\((\*|\d+)\)\s+(.+)$`)
	detail := regexp.MustCompile(`^\(Hardware Port:\s*([^,]+),\s*Device:\s*([^)]+)\)$`)
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var services []macNetworkService
	for index := 0; index+1 < len(lines); index++ {
		match := header.FindStringSubmatch(strings.TrimSpace(lines[index]))
		if len(match) != 3 {
			continue
		}
		info := detail.FindStringSubmatch(strings.TrimSpace(lines[index+1]))
		if len(info) != 3 {
			continue
		}
		services = append(services, macNetworkService{
			Name:         strings.TrimSpace(match[2]),
			HardwarePort: strings.TrimSpace(info[1]),
			Device:       strings.TrimSpace(info[2]),
			Disabled:     match[1] == "*",
		})
	}
	return services
}

func isDJICellularService(service macNetworkService) bool {
	port := strings.ToLower(strings.TrimSpace(service.HardwarePort))
	name := strings.ToLower(strings.TrimSpace(service.Name))
	matchesBaiwang := strings.Contains(port, "baiwang") || strings.Contains(name, "baiwang")
	return matchesBaiwang && regexp.MustCompile(`^en\d+$`).MatchString(service.Device)
}

func (a *app) isVerifiedDJICellularService(service macNetworkService) bool {
	if !isDJICellularService(service) {
		return false
	}
	locationID, ok := a.currentUSBATLocation()
	return ok && isVerifiedDJINetworkInterfaceAtLocation(service.Device, locationID)
}

func setMacNetworkServiceEnabled(name string, enabled bool) error {
	value := "off"
	if enabled {
		value = "on"
	}
	out, err := exec.Command("/usr/sbin/networksetup", "-setnetworkserviceenabled", name, value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func renewMacNetworkServiceDHCP(name string) error {
	out, err := exec.Command("/usr/sbin/networksetup", "-setdhcp", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureCellularDHCP renews DHCP on the DJI cellular network service when its
// USB network interface has no usable IPv4 address. It runs after the USB AT
// bridge reopens so a re-enumerated module regains its 4G fallback route
// without user interaction.
func (a *app) ensureCellularDHCP() {
	if a.demo || a.modem != nil || !a.autoNetworkRepair {
		return
	}
	a.networkRepairMu.Lock()
	defer a.networkRepairMu.Unlock()

	services, err := discoverMacNetworkServices()
	if err != nil {
		log.Printf("cellular DHCP repair: %v", err)
		return
	}
	// Find the DJI cellular network service. macOS may name it differently on
	// a fresh machine ("Baiwang", "Baiwang 2", localized variants), and it may
	// also be disabled, in which case DHCP renewals never apply.
	var target string
	for _, service := range services {
		if !a.isVerifiedDJICellularService(service) {
			continue
		}
		if service.Disabled {
			log.Printf("cellular DHCP repair: enabling disabled DJI cellular service %s", service.Name)
			if err := setMacNetworkServiceEnabled(service.Name, true); err != nil {
				log.Printf("cellular DHCP repair: enable %s failed: %v", service.Name, err)
				continue
			}
		}
		target = service.Name
		break
	}
	// Never guess an unprovisioned en* device by MAC address. This Mac has an
	// LG Studio Display interface with the same locally-administered pattern;
	// binding it as Baiwang would mutate the wrong network service. A new DJI
	// service must first be created through the VID/PID-aware helper.
	if target == "" {
		log.Printf("cellular DHCP repair: no VID/PID-verified DJI network service; refusing to guess an en* interface")
		return
	}
	if _, err := readMacIPv4ServiceInfo(target); err == nil {
		return
	}
	const attempts = 2
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := renewMacNetworkServiceDHCP(target); err != nil {
			log.Printf("cellular DHCP repair: renew DHCP for %s failed: %v", target, err)
			return
		}
		info, waitErr := waitForMacIPv4Service(target, 15*time.Second)
		if waitErr == nil {
			log.Printf("cellular DHCP repair: %s -> %s", target, info.Address)
			return
		}
		log.Printf("cellular DHCP repair: attempt %d/2: %v", attempt, waitErr)
	}
	// DHCP keeps failing: report the modem USB networking mode so a fresh
	// machine can be diagnosed (usbnet=0 means the adapter never comes up).
	if a.hasUSBAT() {
		if resp, err := a.commandUSBAT(`AT+QCFG="usbnet"`, 3*time.Second); err == nil {
			log.Printf("cellular DHCP repair: AT+QCFG usbnet => %s", strings.TrimSpace(resp))
		}
	}
}

type macIPv4ServiceInfo struct {
	Address string
	Subnet  string
}

func parseMacIPv4ServiceInfo(output string) macIPv4ServiceInfo {
	var info macIPv4ServiceInfo
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "IP address":
			info.Address = strings.TrimSpace(value)
		case "Subnet mask":
			info.Subnet = strings.TrimSpace(value)
		}
	}
	return info
}

func readMacIPv4ServiceInfo(name string) (macIPv4ServiceInfo, error) {
	out, err := exec.Command("/usr/sbin/networksetup", "-getinfo", name).CombinedOutput()
	if err != nil {
		return macIPv4ServiceInfo{}, fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	info := parseMacIPv4ServiceInfo(string(out))
	address := net.ParseIP(info.Address)
	if address == nil || address.IsUnspecified() || address.IsLinkLocalUnicast() || net.ParseIP(info.Subnet) == nil {
		return macIPv4ServiceInfo{}, fmt.Errorf("%s: 4G 网卡尚未取得有效 IPv4 地址", name)
	}
	return info, nil
}

func waitForMacIPv4Service(name string, timeout time.Duration) (macIPv4ServiceInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		info, err := readMacIPv4ServiceInfo(name)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return macIPv4ServiceInfo{}, lastErr
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func blockMacNetworkServiceRoute(name string, info macIPv4ServiceInfo) error {
	// Keep the USB ECM interface and its local address alive, but point its
	// router at itself. macOS therefore cannot use it as an internet fallback.
	// Restoring DHCP later returns the real QDC507 gateway without disabling
	// the network service, which avoids dropping the ECM carrier.
	out, err := exec.Command(
		"/usr/sbin/networksetup",
		"-setmanual",
		name,
		info.Address,
		info.Subnet,
		info.Address,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *app) loadNetworkPolicyLocked() error {
	if a.networkPolicyLoaded {
		return nil
	}
	if a.networkPolicyPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		a.networkPolicyPath = filepath.Join(configDir, "MacCellular", "network-policy.json")
	}
	var state cellularPolicyStatus
	data, err := os.ReadFile(a.networkPolicyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
	}
	a.force4GOff = state.ForceOff
	a.disabled4GServices = append([]string(nil), state.Services...)
	a.networkPolicyLoaded = true
	return nil
}

func (a *app) persistNetworkPolicyLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.networkPolicyPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: a.disabled4GServices,
	}, "", "  ")
	if err != nil {
		return err
	}
	temporary := a.networkPolicyPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, a.networkPolicyPath)
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func (a *app) applyCellularPolicyLocked() error {
	services, err := discoverMacNetworkServices()
	if err != nil {
		return err
	}
	if a.force4GOff {
		// Keep the ECM carrier online for SMS/calls, but remove its usable internet
		// gateway. This is deliberately stronger than changing service priority:
		// an automatic fallback may still select 4G, but it cannot send traffic
		// through a route whose gateway is the interface itself.
		var skipped []string
		applied := 0
		for _, service := range services {
			if !a.isVerifiedDJICellularService(service) || service.Disabled {
				continue
			}
			info, err := readMacIPv4ServiceInfo(service.Name)
			if err != nil {
				skipped = append(skipped, err.Error())
				continue
			}
			if err := blockMacNetworkServiceRoute(service.Name, info); err != nil {
				skipped = append(skipped, err.Error())
				continue
			}
			a.disabled4GServices = appendUnique(a.disabled4GServices, service.Name)
			applied++
		}
		if applied == 0 {
			if len(skipped) == 0 {
				return errors.New("未找到可用的 4G 网络服务")
			}
			return errors.New(strings.Join(skipped, "; "))
		}
		return a.persistNetworkPolicyLocked()
	}
	serviceByName := make(map[string]macNetworkService, len(services))
	for _, service := range services {
		serviceByName[service.Name] = service
	}
	var restoreErrors []string
	for _, name := range a.disabled4GServices {
		service, found := serviceByName[name]
		if !found || !a.isVerifiedDJICellularService(service) {
			restoreErrors = append(restoreErrors, fmt.Sprintf("拒绝恢复未实时绑定到 DJI VID/PID 的网络服务 %q", name))
			continue
		}
		if err := renewMacNetworkServiceDHCP(name); err != nil {
			log.Printf("restore DHCP for 4G network service %q: %v", name, err)
			restoreErrors = append(restoreErrors, err.Error())
		}
	}
	if len(restoreErrors) > 0 {
		if err := a.persistNetworkPolicyLocked(); err != nil {
			restoreErrors = append(restoreErrors, err.Error())
		}
		return errors.New(strings.Join(restoreErrors, "; "))
	}
	a.disabled4GServices = nil
	return a.persistNetworkPolicyLocked()
}

func (a *app) cellularPolicyStatus() (cellularPolicyStatus, error) {
	a.networkPolicyMu.Lock()
	defer a.networkPolicyMu.Unlock()
	if err := a.loadNetworkPolicyLocked(); err != nil {
		return cellularPolicyStatus{}, err
	}
	return cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: append([]string(nil), a.disabled4GServices...),
	}, nil
}

func (a *app) getCellularPolicy(w http.ResponseWriter, _ *http.Request) {
	state, err := a.cellularPolicyStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *app) setCellularPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ForceOff bool `json:"force_off"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.networkPolicyMu.Lock()
	defer a.networkPolicyMu.Unlock()
	if err := a.loadNetworkPolicyLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previousForceOff := a.force4GOff
	previousServices := append([]string(nil), a.disabled4GServices...)
	a.force4GOff = body.ForceOff
	if err := a.applyCellularPolicyLocked(); err != nil {
		a.force4GOff = previousForceOff
		a.disabled4GServices = previousServices
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: append([]string(nil), a.disabled4GServices...),
	})
}

func (a *app) startCellularPolicyGuard(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.networkPolicyMu.Lock()
		if err := a.loadNetworkPolicyLocked(); err != nil {
			log.Printf("load cellular policy: %v", err)
		} else if a.force4GOff {
			if err := a.applyCellularPolicyLocked(); err != nil {
				log.Printf("enforce cellular force-off policy: %v", err)
			}
		}
		a.networkPolicyMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *app) setUSBNetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode    int  `json:"mode"`
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Mode < 0 || body.Mode > 3 {
		writeError(w, http.StatusBadRequest, "only usbnet mode 0, 1, 2 or 3 is allowed")
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "修改 USB 网络模式需要明确确认")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	if a.moduleMutationLockedHook != nil {
		a.moduleMutationLockedHook()
	}
	command := fmt.Sprintf(`AT+QCFG="usbnet",%d`, body.Mode)
	response, attempted, err := a.runFreshNoCallModuleCommand(command, 8*time.Second)
	if err != nil || !attempted || atResponseIsError(response) {
		writeError(w, http.StatusBadGateway, firstNonEmpty(errString(err), response, "模块未执行 USB 网络模式写入"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":         body.Mode,
		"response":     response,
		"needs_reboot": true,
	})
}

func (a *app) rebootModule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "重启模块需要明确确认")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	response, attempted, execution, err := a.runFreshNoCallModuleCommandExact("AT+CFUN=1,1", 3*time.Second)
	if !attempted || (err == nil && atResponseIsError(response)) {
		writeError(w, http.StatusBadGateway, firstNonEmpty(errString(err), response, "模块未执行重启"))
		return
	}
	if attempted {
		a.markUSBATExecutionDetached(execution, "explicit module reboot")
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"response": response,
	})
}

func parseUSBNetMode(resp string) string {
	re := regexp.MustCompile(`\+QCFG:\s*"usbnet",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func parsePDPContexts(resp string) []pdpContext {
	re := regexp.MustCompile(`\+CGDCONT:\s*(\d+),"([^"]*)","([^"]*)"`)
	var contexts []pdpContext
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, pdpContext{ID: id, PDN: match[2], APN: match[3]})
	}
	return contexts
}

func parseActivePDPContexts(resp string) []int {
	re := regexp.MustCompile(`\+CGACT:\s*(\d+),1`)
	var contexts []int
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, id)
	}
	return contexts
}

func parsePDPAddresses(resp string) []string {
	re := regexp.MustCompile(`\+CGPADDR:\s*\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(match[1], ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func discoverMacNetworkInterfaces() []macNetInterface {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return nil
	}
	var interfaces []macNetInterface
	for _, block := range splitIfconfigBlocks(string(out)) {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		fields := strings.Fields(lines[0])
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name == "" || strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "utun") {
			continue
		}
		item := macNetInterface{Name: name, Status: "unknown", Kind: classifyMacInterfaceName(name)}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "status:") {
				item.Status = strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			}
			if strings.HasPrefix(line, "ether ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.MAC = fields[1]
				}
			}
			if strings.HasPrefix(line, "inet ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.IPv4 = fields[1]
				}
			}
		}
		interfaces = append(interfaces, item)
	}
	return interfaces
}

func discoverMacDefaultRoute() macDefaultRoute {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return macDefaultRoute{}
	}
	var route macDefaultRoute
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			route.Gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			route.Interface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return route
}

func splitIfconfigBlocks(out string) []string {
	var blocks []string
	var current []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' && strings.Contains(line, ":") {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func classifyMacInterfaceName(name string) string {
	switch {
	case strings.HasPrefix(name, "en"):
		return "ethernet"
	case strings.HasPrefix(name, "bridge"):
		return "bridge"
	case strings.HasPrefix(name, "awdl") || strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "ap"):
		return "apple-wireless"
	default:
		return "other"
	}
}

func hasLikelyUSBNetworkInterface(interfaces []macNetInterface) bool {
	for _, item := range interfaces {
		if item.Kind == "ethernet" && item.Name != "en0" && item.Status == "active" {
			return true
		}
	}
	return false
}

func selectUSBTrafficInterface(interfaces []macNetInterface, route macDefaultRoute) string {
	for _, item := range interfaces {
		if item.Name == route.Interface && isUsableUSBTrafficInterface(item) {
			return item.Name
		}
	}
	for _, item := range interfaces {
		if isUsableUSBTrafficInterface(item) {
			return item.Name
		}
	}
	return ""
}

func isUsableUSBTrafficInterface(item macNetInterface) bool {
	if item.Kind != "ethernet" || item.Name == "en0" || item.Status != "active" {
		return false
	}
	address := net.ParseIP(item.IPv4)
	return address != nil && !address.IsUnspecified() && !address.IsLinkLocalUnicast()
}

func discoverMacInterfaceCounters() (map[string]networkByteCounters, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return nil, err
	}
	return parseMacInterfaceCounters(string(out)), nil
}

func parseMacInterfaceCounters(out string) map[string]networkByteCounters {
	counters := make(map[string]networkByteCounters)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[0], "*")
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if name == "" || rxErr != nil || txErr != nil {
			continue
		}
		counters[name] = networkByteCounters{RX: rx, TX: tx}
	}
	return counters
}

func (a *app) loadProfileNotesLocked() error {
	if a.profileNotesLoaded {
		return nil
	}
	path := a.profileNotesPath
	if path == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate profile notes directory: %w", err)
		}
		path = filepath.Join(configDir, "MacCellular", "profile-notes.json")
		a.profileNotesPath = path
	}
	notes := make(map[string]profileNote)
	readPath := path
	if _, err := os.Stat(readPath); errors.Is(err, os.ErrNotExist) {
		legacyPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "VoHive macOS", "profile-notes.json")
		if _, legacyErr := os.Stat(legacyPath); legacyErr == nil {
			readPath = legacyPath
		}
	}
	data, err := os.ReadFile(readPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read profile notes: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &notes); err != nil {
			return fmt.Errorf("parse profile notes: %w", err)
		}
	}
	a.profileNotes = notes
	a.profileNotesLoaded = true
	return nil
}

func (a *app) persistProfileNotesLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.profileNotesPath), 0o700); err != nil {
		return fmt.Errorf("create profile notes directory: %w", err)
	}
	data, err := json.MarshalIndent(a.profileNotes, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile notes: %w", err)
	}
	temporary := a.profileNotesPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write profile notes: %w", err)
	}
	if err := os.Rename(temporary, a.profileNotesPath); err != nil {
		return fmt.Errorf("replace profile notes: %w", err)
	}
	return nil
}

func (a *app) listESIMNotes(w http.ResponseWriter, _ *http.Request) {
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": a.profileNotes})
}

func (a *app) saveESIMNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		Label string `json:"label"`
		Phone string `json:"phone"`
		Tags  string `json:"tags"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Label = strings.TrimSpace(body.Label)
	body.Phone = strings.TrimSpace(body.Phone)
	body.Tags = strings.TrimSpace(body.Tags)
	if body.ICCID == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if len(body.Label) > 80 || len(body.Phone) > 80 || len(body.Tags) > 200 {
		writeError(w, http.StatusBadRequest, "本地备注字段过长")
		return
	}
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Label == "" && body.Phone == "" && body.Tags == "" {
		delete(a.profileNotes, body.ICCID)
	} else {
		a.profileNotes[body.ICCID] = profileNote{Label: body.Label, Phone: body.Phone, Tags: body.Tags}
	}
	if err := a.persistProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "本地备注已保存", "note": a.profileNotes[body.ICCID]})
}

func atCommandSucceeded(response string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(response), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func (a *app) phonebookProbeCommand(command string, result *phonebookProbeResult) bool {
	response, err := a.runATCommand(command, 6*time.Second)
	if err != nil {
		result.Responses[command] = err.Error()
		return false
	}
	result.Responses[command] = strings.TrimSpace(response)
	return atCommandSucceeded(response)
}

// probeESIMPhonebook performs only AT test/read commands. It never writes a
// phonebook entry, so it is safe to use before enabling portable card notes.
func (a *app) probeESIMPhonebook(w http.ResponseWriter, _ *http.Request) {
	result := phonebookProbeResult{Responses: make(map[string]string)}
	if a.demo {
		result.StorageSupported = true
		result.StorageSelected = true
		result.ReadSupported = true
		result.WriteSupported = true
		result.StorageStatus = `+CPBS: "SM",0,250`
		result.Responses[`AT+CPBS=?`] = `+CPBS: ("SM","ME")\r\nOK`
		result.Responses[`AT+CPBR=?`] = `+CPBR: (1-250),40,14\r\nOK`
		result.Responses[`AT+CPBW=?`] = `+CPBW: (1-250),40,(129,145),16\r\nOK`
		writeJSON(w, http.StatusOK, result)
		return
	}

	if !a.phonebookProbeCommand(`AT+CPBS=?`, &result) {
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.StorageSupported = strings.Contains(strings.ToUpper(result.Responses[`AT+CPBS=?`]), `"SM"`)
	if !result.StorageSupported {
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.StorageSelected = a.phonebookProbeCommand(`AT+CPBS="SM"`, &result)
	if !result.StorageSelected {
		writeJSON(w, http.StatusOK, result)
		return
	}
	if a.phonebookProbeCommand(`AT+CPBS?`, &result) {
		result.StorageStatus = result.Responses[`AT+CPBS?`]
	}
	result.ReadSupported = a.phonebookProbeCommand(`AT+CPBR=?`, &result)
	result.WriteSupported = a.phonebookProbeCommand(`AT+CPBW=?`, &result)
	writeJSON(w, http.StatusOK, result)
}

const moduleNotePrefix = "VH1|"

func encodeModuleProfileNote(note moduleProfileNote) (string, error) {
	note.ICCID = strings.TrimSpace(note.ICCID)
	note.Label = strings.TrimSpace(note.Label)
	note.Phone = strings.TrimSpace(note.Phone)
	note.Tags = strings.TrimSpace(note.Tags)
	if note.ICCID == "" {
		return "", errors.New("iccid is required")
	}
	if len(note.Label) > 48 || len(note.Phone) > 40 || len(note.Tags) > 48 {
		return "", errors.New("模块资料名称、手机号或标签过长")
	}
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	encoded := strings.Join([]string{moduleNotePrefix[:len(moduleNotePrefix)-1], note.ICCID, encode(note.Label), encode(note.Phone), encode(note.Tags)}, "|")
	if len(encoded) > 255 {
		return "", errors.New("模块通讯录记录超过容量")
	}
	return encoded, nil
}

func decodeModuleProfileNote(index int, text string) (moduleProfileNote, bool) {
	parts := strings.Split(text, "|")
	if len(parts) != 5 || parts[0] != strings.TrimSuffix(moduleNotePrefix, "|") || strings.TrimSpace(parts[1]) == "" {
		return moduleProfileNote{}, false
	}
	decode := func(value string) (string, bool) {
		data, err := base64.RawURLEncoding.DecodeString(value)
		return string(data), err == nil
	}
	label, labelOK := decode(parts[2])
	phone, phoneOK := decode(parts[3])
	tags, tagsOK := decode(parts[4])
	if !labelOK || !phoneOK || !tagsOK {
		return moduleProfileNote{}, false
	}
	return moduleProfileNote{Index: index, ICCID: parts[1], Label: label, Phone: phone, Tags: tags}, true
}

func (a *app) runATOK(command string, timeout time.Duration) (string, error) {
	response, err := a.runATCommand(command, timeout)
	if err != nil {
		return "", err
	}
	if !atCommandSucceeded(response) {
		return "", fmt.Errorf("%s: %s", command, strings.TrimSpace(response))
	}
	return response, nil
}

func parseMEPhonebookStatus(response string) (used, total int, err error) {
	re := regexp.MustCompile(`\+CPBS:\s*"ME",(\d+),(\d+)`)
	match := re.FindStringSubmatch(response)
	if len(match) != 3 {
		return 0, 0, errors.New("ME 通讯录容量未返回")
	}
	used, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.Atoi(match[2])
	return used, total, err
}

func parseMEPhonebookEntries(response string) []modulePhonebookEntry {
	re := regexp.MustCompile(`(?m)\+CPBR:\s*(\d+),"([^"]*)",\d+,"([^"]*)"`)
	entries := make([]modulePhonebookEntry, 0)
	for _, match := range re.FindAllStringSubmatch(response, -1) {
		index, err := strconv.Atoi(match[1])
		if err == nil {
			entries = append(entries, modulePhonebookEntry{Index: index, Number: match[2], Text: match[3]})
		}
	}
	return entries
}

func (a *app) readModuleESIMNotes() (map[string]moduleProfileNote, map[int]bool, int, int, error) {
	if _, err := a.runATOK(`AT+CPBS="ME"`, 6*time.Second); err != nil {
		return nil, nil, 0, 0, err
	}
	status, err := a.runATOK(`AT+CPBS?`, 6*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	used, total, err := parseMEPhonebookStatus(status)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	notes := make(map[string]moduleProfileNote)
	occupied := make(map[int]bool)
	if used == 0 {
		return notes, occupied, used, total, nil
	}
	response, err := a.runATOK(fmt.Sprintf("AT+CPBR=1,%d", total), 20*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	for _, entry := range parseMEPhonebookEntries(response) {
		occupied[entry.Index] = true
		if note, ok := decodeModuleProfileNote(entry.Index, entry.Text); ok {
			notes[note.ICCID] = note
		}
	}
	return notes, occupied, used, total, nil
}

func (a *app) listModuleESIMNotes(w http.ResponseWriter, _ *http.Request) {
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, _, used, total, err := a.readModuleESIMNotes()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("读取模块资料库失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes, "used": used, "total": total})
}

func (a *app) saveModuleESIMNote(w http.ResponseWriter, r *http.Request) {
	var body moduleProfileNote
	if !decodeJSON(w, r, &body) {
		return
	}
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, occupied, _, total, err := a.readModuleESIMNotes()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("读取模块资料库失败: %v", err))
		return
	}
	current, exists := notes[strings.TrimSpace(body.ICCID)]
	if strings.TrimSpace(body.Label) == "" && strings.TrimSpace(body.Phone) == "" && strings.TrimSpace(body.Tags) == "" {
		if !exists {
			writeJSON(w, http.StatusOK, map[string]string{"message": "模块资料库中没有此记录"})
			return
		}
		if _, err := a.runATOK(fmt.Sprintf("AT+CPBW=%d", current.Index), 8*time.Second); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("删除模块资料失败: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "模块资料已删除"})
		return
	}
	encoded, err := encodeModuleProfileNote(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	index := current.Index
	if !exists {
		for candidate := 1; candidate <= total; candidate++ {
			if !occupied[candidate] {
				index = candidate
				break
			}
		}
	}
	if index == 0 {
		writeError(w, http.StatusConflict, "模块通讯录已满")
		return
	}
	command := fmt.Sprintf(`AT+CPBW=%d,"00000000000",129,"%s"`, index, encoded)
	if _, err := a.runATOK(command, 8*time.Second); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("写入模块资料失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "模块资料已保存", "index": index})
}

func (a *app) esimOverview(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"chip_info": map[string]any{
				"sku_name":      "eUICC Demo Card",
				"serial_number": "DEMO-001",
				"firmware":      "1.0.0",
				"eids": []map[string]any{{
					"aid": "A0000005591010FFFFFFFF8900000100",
					"eid": "89049032000000000000000000000001",
				}},
			},
			"profiles": []map[string]any{{
				"eid":     "89049032000000000000000000000001",
				"aid_hex": "A0000005591010FFFFFFFF8900000100",
				"profiles": []map[string]any{
					{
						"iccid": "89860123456789012345", "name": "中国移动",
						"service_provider_name": "China Mobile", "state": 1, "state_text": "enabled",
					},
					{
						"iccid": "8944100000000000001", "name": "英国旅行卡",
						"service_provider_name": "giffgaff UK", "state": 0, "state_text": "disabled",
					},
					{
						"iccid": "8949020000000000002", "name": "欧洲数据卡",
						"service_provider_name": "Travel Europe", "state": 0, "state_text": "disabled",
					},
				},
			}},
		})
		return
	}
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, map[string]any{
				"card_type": "physical_sim",
				"message":   "当前卡片为实体卡，非 eSIM 卡片",
			})
			return
		}
		log.Printf("eSIM overview failed: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// A normal physical SIM cannot open the GSMA eUICC management AIDs. The
// manager reports that as no eUICC discovered with an AT+CCHO ERROR; expose it
// as a neutral card type instead of leaking an implementation error to the UI.
func isPhysicalSIMESIMProbeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "未发现任何 euicc") &&
		strings.Contains(message, "at+ccho") &&
		strings.Contains(message, "error")
}

func (a *app) esimHealth(w http.ResponseWriter, _ *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, map[string]any{"card_type": "physical_sim"})
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	var active *esim.ProfileItem
	for _, group := range overview.Profiles {
		for index := range group.Profiles {
			if group.Profiles[index].State == 1 {
				active = &group.Profiles[index]
				break
			}
		}
		if active != nil {
			break
		}
	}
	if active == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "eSIM 卡片已识别，但没有已启用的 Profile"})
		return
	}

	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	status, err := a.usbATStatus()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	registered := status.RegStatus == 1 || status.RegStatus == 5
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             status.SimInserted && registered,
		"active_profile": active,
		"module_iccid":   status.ICCID,
		"imsi":           status.IMSI,
		"operator":       status.Operator,
		"registration":   status.RegStatusText,
		"registered":     registered,
		"signal_dbm":     status.SignalDBM,
		"network_mode":   status.NetworkMode,
	})
}

func (a *app) switchESIM(w http.ResponseWriter, r *http.Request) {
	esimManager, switchAllowed := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ICCID) == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"switch_accepted": true,
			"phase":           "done",
			"target_iccid":    body.ICCID,
		})
		return
	}
	if !switchAllowed && a.modem == nil {
		writeError(w, http.StatusServiceUnavailable, "USB AT eSIM/卡片当前暂不允许切换 Profile")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := esimManager.SwitchProfileWithResult(ctx, body.ICCID, body.AID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Enabling a Profile resets the eUICC, but the DJI modem can keep the
	// previous SIM session (and therefore its CNUM) until its own firmware is
	// restarted.  Reload it here so the newly enabled Profile actually becomes
	// the modem's active subscriber identity.
	rebootResponse, rebootAttempted, rebootExecution, rebootErr := a.runFreshNoCallModuleCommandExact("AT+CFUN=1,1", 3*time.Second)
	rebootRequested := rebootAttempted && rebootErr == nil && !atResponseIsError(rebootResponse)
	rebootWarning := ""
	if rebootErr != nil {
		// A USB disconnect immediately after CFUN is expected on some firmware;
		// the command may already have been accepted before the bridge drops.
		upper := strings.ToUpper(rebootErr.Error())
		if rebootAttempted && (strings.Contains(upper, "NO_DEVICE") || strings.Contains(upper, "NOT_FOUND")) {
			rebootRequested = true
		} else {
			rebootWarning = rebootErr.Error()
			log.Printf("eSIM profile switched but module restart was not confirmed: %v", rebootErr)
		}
	}
	if rebootAttempted {
		a.markUSBATExecutionDetached(rebootExecution, "eSIM profile switch reboot")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"switch_accepted":         result.SwitchAccepted,
		"phase":                   result.Phase,
		"target_iccid":            result.TargetICCID,
		"recovery_pending":        result.RecoveryPending,
		"module_reboot_requested": rebootRequested,
		"module_reboot_response":  rebootResponse,
		"module_reboot_warning":   rebootWarning,
		"reconnect_wait_seconds":  10,
	})
}

func (a *app) renameESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Name = strings.TrimSpace(body.Name)
	if body.ICCID == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "iccid and name are required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	if err := esimManager.RenameProfile(body.ICCID, body.Name, body.AID); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("修改 Profile 名称失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
}

func (a *app) deleteESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	if body.ICCID == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 已删除"})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	result, err := esimManager.DeleteProfile(body.ICCID, body.AID)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("删除 Profile 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) downloadESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		SMDP             string `json:"smdp"`
		MatchingID       string `json:"matching_id"`
		ConfirmationCode string `json:"confirmation_code"`
		AID              string `json:"aid"`
		IMEI             string `json:"imei"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SMDP = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(body.SMDP, "https://"), "http://"))
	if body.SMDP == "" {
		writeError(w, http.StatusBadRequest, "smdp is required")
		return
	}
	if strings.TrimSpace(body.IMEI) == "" {
		writeError(w, http.StatusBadRequest, "imei is required for USB AT eSIM download")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "演示：Profile 下载完成"})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	result, err := esimManager.DownloadProfile(ctx, body.AID, body.SMDP, body.MatchingID, body.ConfirmationCode, body.IMEI, func(event esim.DownloadProgressEvent) {
		log.Printf("eSIM download %d%% %s", event.Pct, event.Msg)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("下载 Profile 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain exactly one JSON value")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
