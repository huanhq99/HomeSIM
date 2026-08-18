package remotevoice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const MaxOfferBytes = 64 << 10

var ErrPeerConnectionEnded = errors.New("remotevoice: WebRTC peer connection ended")

// SelectedPairProtocol is a redacted, allowlisted view of the transport used
// by the current ICE selected pair. Unknown values are represented by the
// empty string rather than copying an implementation-provided value.
type SelectedPairProtocol string

const (
	SelectedPairProtocolUnknown SelectedPairProtocol = ""
	SelectedPairProtocolUDP     SelectedPairProtocol = "udp"
	SelectedPairProtocolTCP     SelectedPairProtocol = "tcp"
)

// SelectedCandidateType is a redacted, allowlisted ICE candidate type.
type SelectedCandidateType string

const (
	SelectedCandidateTypeUnknown         SelectedCandidateType = ""
	SelectedCandidateTypeHost            SelectedCandidateType = "host"
	SelectedCandidateTypeServerReflexive SelectedCandidateType = "srflx"
	SelectedCandidateTypePeerReflexive   SelectedCandidateType = "prflx"
	SelectedCandidateTypeRelay           SelectedCandidateType = "relay"
)

// SelectedPairProfile identifies which immutable candidate-pair policy was
// used to produce a redacted snapshot. The empty value is intentionally the
// legacy restricted-host profile so older synthetic snapshots and callers
// retain their existing behavior.
type SelectedPairProfile string

const (
	SelectedPairProfileRestrictedHost SelectedPairProfile = ""
	SelectedPairProfileTURNRelay      SelectedPairProfile = "turn-relay"
)

// SelectedPairSnapshot proves policy properties of the current selected ICE
// pair without retaining or exposing either address, either port, SDP, or ICE
// credentials. In the restricted-host profile AddressAllowed means the
// address passed the same configured CIDR allowlist used by Pion's local or
// remote IP filter. The TURN relay profile relies on relay/relay candidate
// type enforcement instead. LocalAddressAllowed remains false; when Pion
// reports the browser's relayed source as peer-reflexive,
// RemoteAddressAllowed proves that its observed source IP is exactly the
// configured numeric TURN server rather than a direct client address.
type SelectedPairSnapshot struct {
	Established          bool                  `json:"established"`
	Profile              SelectedPairProfile   `json:"profile,omitempty"`
	Protocol             SelectedPairProtocol  `json:"protocol,omitempty"`
	LocalCandidateType   SelectedCandidateType `json:"localCandidateType,omitempty"`
	RemoteCandidateType  SelectedCandidateType `json:"remoteCandidateType,omitempty"`
	LocalAddressAllowed  bool                  `json:"localAddressAllowed"`
	RemoteAddressAllowed bool                  `json:"remoteAddressAllowed"`
}

// PolicyValid reports whether this redacted observation proves the selected
// pair is the permitted UDP RTP path. It deliberately recomputes from every
// field so a partial or synthetic snapshot cannot opt itself into readiness.
func (p SelectedPairSnapshot) PolicyValid() bool {
	if !p.Established || p.Protocol != SelectedPairProtocolUDP {
		return false
	}
	if p.Profile == SelectedPairProfileTURNRelay {
		return p.LocalCandidateType == SelectedCandidateTypeRelay &&
			(p.RemoteCandidateType == SelectedCandidateTypeRelay ||
				p.RemoteCandidateType == SelectedCandidateTypePeerReflexive && p.RemoteAddressAllowed)
	}
	return p.Profile == SelectedPairProfileRestrictedHost &&
		p.LocalCandidateType == SelectedCandidateTypeHost &&
		(p.RemoteCandidateType == SelectedCandidateTypeHost ||
			p.RemoteCandidateType == SelectedCandidateTypePeerReflexive) &&
		p.LocalAddressAllowed && p.RemoteAddressAllowed
}

// NetworkActivitySnapshot is a credential-free view of the WebRTC media
// plane. RemoteTrackReady becomes true only after the first accepted PCMU RTP
// packet, not merely when ICE/DTLS reports connected.
type NetworkActivitySnapshot struct {
	Connected           bool
	SelectedPair        SelectedPairSnapshot
	LocalTrackStarted   bool
	UplinkPackets       uint64
	LastUplinkAt        time.Time
	LastUplinkEpoch     uint64
	RealUplinkPackets   uint64
	LastRealUplinkAt    time.Time
	LastRealUplinkEpoch uint64
	// RealUplinkPacketsAtLastReceiverReport is a causally acknowledged lower
	// bound: each advance proves a real sample preceded a later matching RR.
	// It remains stable during continuous 20 ms sends so freshness does not
	// flicker between periodic reports.
	RealUplinkPacketsAtLastReceiverReport uint64
	RemoteTrackReady                      bool
	RemoteRTPPackets                      uint64
	RemoteRTPSamples                      uint64
	RemoteRTPSignalSamples                uint64
	RemoteRTPPeakPCM16                    uint32
	LastRemoteRTPAt                       time.Time
	LastRemoteRTPEpoch                    uint64
	ReceiverReports                       uint64
	LastReceiverReportAt                  time.Time
	LastReceiverReportEpoch               uint64
}

type Config struct {
	Token string `json:"-"`
	// Generation is a caller-owned media lease generation; it need not equal a
	// raw modem/CLCC topology counter.
	Generation         uint64
	AllowedInterfaces  []string
	AllowedLocalCIDRs  []string
	AllowedRemoteCIDRs []string
	UDPMin             uint16
	UDPMax             uint16
	AnswerTimeout      time.Duration
	// TURNRelay selects the independent public-Internet media profile. When
	// present, Pion is configured with ICETransportPolicyRelay and readiness
	// requires a UDP relay-to-relay selected pair. TURN server access may use
	// UDP or TLS-over-TCP; coturn still allocates a UDP media relay candidate.
	// Legacy Tailscale callers leave this nil and retain the restricted
	// host/prflx policy above.
	TURNRelay *TURNRelayConfig `json:"-"`
	// allowLoopbackForTests is intentionally unexported so production callers
	// cannot bypass the Tailscale-only address policy.
	allowLoopbackForTests bool
}

// TURNCredentialType is deliberately narrower than Pion's credential union.
// The public relay profile currently accepts only time-limited TURN REST
// username/password credentials.
type TURNCredentialType string

const TURNCredentialTypePassword TURNCredentialType = "password"

// TURNRelayConfig contains one short-lived TURN credential bundle. Endpoint
// and credential material are excluded from JSON, and all default formatting
// is redacted.
type TURNRelayConfig struct {
	URLs           []string `json:"-"`
	Username       string   `json:"-"`
	Password       string   `json:"-"`
	CredentialType TURNCredentialType
}

func (TURNRelayConfig) String() string   { return "remotevoice.TURNRelayConfig{redacted}" }
func (TURNRelayConfig) GoString() string { return "remotevoice.TURNRelayConfig{redacted}" }
func (TURNRelayConfig) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

func (Config) String() string   { return "remotevoice.Config{redacted}" }
func (Config) GoString() string { return "remotevoice.Config{redacted}" }

type Session struct {
	token              string
	generation         uint64
	port               *FramePort
	pc                 *webrtc.PeerConnection
	iceTransport       *webrtc.ICETransport
	selectedPairPolicy selectedPairPolicy
	cancel             context.CancelFunc
	closeOnce          sync.Once
	closeErr           error
	wg                 sync.WaitGroup
	// mediaBarrierMu linearizes completed media operations against Activate
	// without blocking activation on an RTP/RTCP read that is still waiting.
	mediaBarrierMu sync.Mutex

	stateMu                               sync.Mutex
	connected                             bool
	terminal                              bool
	terminalErr                           error
	localTrackStarted                     bool
	uplinkPackets                         uint64
	lastUplinkAt                          time.Time
	lastUplinkEpoch                       uint64
	realUplinkPackets                     uint64
	lastRealUplinkAt                      time.Time
	lastRealUplinkEpoch                   uint64
	realUplinkPacketsAtLastReceiverReport uint64
	realUplinkProofPending                bool
	realUplinkProofEpoch                  uint64
	realUplinkProofAt                     time.Time
	realUplinkProofPackets                uint64
	realUplinkProofReceiverReportBaseline uint64
	remoteTrackReady                      bool
	remoteRTPPackets                      uint64
	remoteRTPSamples                      uint64
	remoteRTPSignalSamples                uint64
	remoteRTPPeakPCM16                    uint32
	lastRemoteRTPAt                       time.Time
	lastRemoteRTPEpoch                    uint64
	receiverReports                       uint64
	lastReceiverReportAt                  time.Time
	lastReceiverReportEpoch               uint64
	mediaEpoch                            uint64
	terminalDone                          chan struct{}
	terminalOnce                          sync.Once
}

// Token returns the bearer identity used to bind this session. Callers must
// never log or serialize it.
func (s *Session) Token() string { return s.token }

func (*Session) String() string   { return "remotevoice.Session{redacted}" }
func (*Session) GoString() string { return "remotevoice.Session{redacted}" }

// Generation returns the caller-owned media lease generation.
func (s *Session) Generation() uint64 { return s.generation }
func (s *Session) Port() *FramePort   { return s.port }

// Answer creates one network media session and returns a fully gathered,
// non-trickle SDP answer. The caller owns token/generation and must Close the
// returned session. The FramePort is not closed by Session.Close.
func Answer(ctx context.Context, offer []byte, cfg Config, port *FramePort) (*Session, []byte, error) {
	if len(offer) == 0 || len(offer) > MaxOfferBytes {
		return nil, nil, fmt.Errorf("remotevoice: offer size must be 1..%d bytes", MaxOfferBytes)
	}
	if port == nil {
		return nil, nil, errors.New("remotevoice: frame port is required")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, nil, err
	}
	totalMedia, audioMedia := countMediaSections(string(offer))
	if totalMedia != 1 || audioMedia != 1 {
		return nil, nil, errors.New("remotevoice: offer must contain exactly one media section and it must be audio")
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: SampleRate,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, nil, fmt.Errorf("remotevoice: register PCMU: %w", err)
	}

	settingEngine, err := restrictedSettings(cfg)
	if err != nil {
		return nil, nil, err
	}
	pairPolicy, err := newSelectedPairPolicy(cfg)
	if err != nil {
		return nil, nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settingEngine))
	pc, err := api.NewPeerConnection(peerConnectionConfiguration(cfg))
	if err != nil {
		if cfg.TURNRelay != nil {
			return nil, nil, errors.New("remotevoice: create TURN relay peer")
		}
		return nil, nil, fmt.Errorf("remotevoice: create peer: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &Session{
		token: cfg.Token, generation: cfg.Generation, port: port, pc: pc, cancel: cancel,
		selectedPairPolicy: pairPolicy, terminalDone: make(chan struct{}),
	}
	failed := true
	defer func() {
		if failed {
			s.Close()
		}
	}()

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1,
	}, "cellular-audio", "cellular")
	if err != nil {
		return nil, nil, fmt.Errorf("remotevoice: create uplink track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, nil, fmt.Errorf("remotevoice: add uplink track: %w", err)
	}
	dtlsTransport := sender.Transport()
	if dtlsTransport == nil || dtlsTransport.ICETransport() == nil {
		return nil, nil, errors.New("remotevoice: ICE transport is unavailable")
	}
	s.iceTransport = dtlsTransport.ICETransport()
	s.wg.Add(1)
	go s.drainRTCP(sessionCtx, sender)
	connected := make(chan struct{})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.setConnected(state == webrtc.PeerConnectionStateConnected)
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
		if state == webrtc.PeerConnectionStateFailed {
			s.signalTerminal(ErrPeerConnectionEnded)
		}
		if state == webrtc.PeerConnectionStateClosed {
			s.signalTerminal(nil)
		}
	})

	downlinkTrack := make(chan *webrtc.TrackRemote, 1)
	var remoteTrackClaimed atomic.Bool
	s.wg.Add(1)
	go s.waitForDownlink(sessionCtx, downlinkTrack)
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Kind() != webrtc.RTPCodecTypeAudio || remote.PayloadType() != 0 ||
			!strings.EqualFold(remote.Codec().MimeType, webrtc.MimeTypePCMU) ||
			!remoteTrackClaimed.CompareAndSwap(false, true) {
			// The gateway accepts exactly one PCMU microphone track. Any extra
			// or unexpected track is a protocol violation, not something to
			// leave attached and silently ignore.
			s.signalTerminal(ErrPeerConnectionEnded)
			cancel()
			return
		}
		downlinkTrack <- remote
	})

	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		// Pion parse errors may quote a complete untrusted ICE candidate. Keep
		// the returned error stable so callers cannot accidentally log remote
		// addresses, SDP credentials, or attacker-controlled marker text.
		return nil, nil, errors.New("remotevoice: invalid remote SDP offer")
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("remotevoice: create answer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return nil, nil, fmt.Errorf("remotevoice: set answer: %w", err)
	}

	timeout := cfg.AnswerTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("remotevoice: answer canceled: %w", ctx.Err())
	case <-timer.C:
		return nil, nil, errors.New("remotevoice: ICE gathering timed out")
	case <-gatherComplete:
	}
	local := pc.LocalDescription()
	if local == nil || local.SDP == "" {
		return nil, nil, errors.New("remotevoice: empty local answer")
	}

	s.wg.Add(1)
	go s.sendUplink(sessionCtx, connected, track)
	failed = false
	return s, []byte(local.SDP), nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Token) == "" || cfg.Generation == 0 {
		return errors.New("remotevoice: non-empty token and non-zero generation are required")
	}
	for _, name := range cfg.AllowedInterfaces {
		if strings.TrimSpace(name) == "" {
			return errors.New("remotevoice: blank allowed interface")
		}
	}
	if cfg.UDPMin == 0 || cfg.UDPMax == 0 || cfg.UDPMin > cfg.UDPMax {
		return errors.New("remotevoice: invalid UDP port range")
	}
	if cfg.AnswerTimeout < 0 {
		return errors.New("remotevoice: negative answer timeout")
	}
	if cfg.TURNRelay != nil {
		if len(cfg.AllowedLocalCIDRs) != 0 || len(cfg.AllowedRemoteCIDRs) != 0 {
			return errors.New("remotevoice: TURN relay profile cannot use address allowlists")
		}
		return validateTURNRelayConfig(*cfg.TURNRelay)
	}
	if len(cfg.AllowedInterfaces) == 0 || len(cfg.AllowedLocalCIDRs) == 0 || len(cfg.AllowedRemoteCIDRs) == 0 {
		return errors.New("remotevoice: interface, local CIDR, and remote CIDR allowlists must be non-empty")
	}
	if err := validateMediaCIDRAllowlist(cfg.AllowedLocalCIDRs, cfg.allowLoopbackForTests, false); err != nil {
		return fmt.Errorf("remotevoice: local CIDR allowlist: %w", err)
	}
	if err := validateMediaCIDRAllowlist(cfg.AllowedRemoteCIDRs, cfg.allowLoopbackForTests, true); err != nil {
		return fmt.Errorf("remotevoice: remote CIDR allowlist: %w", err)
	}
	return nil
}

func validateTURNRelayConfig(cfg TURNRelayConfig) error {
	if len(cfg.URLs) != 2 && len(cfg.URLs) != 3 {
		return errors.New("remotevoice: TURN relay requires an exact fallback URL bundle")
	}
	if cfg.CredentialType != TURNCredentialTypePassword {
		return errors.New("remotevoice: TURN relay credential type must be password")
	}
	if cfg.Username == "" || cfg.Password == "" || len(cfg.Username) > 1024 || len(cfg.Password) > 1024 {
		return errors.New("remotevoice: valid TURN relay credentials are required")
	}
	udpHost, udpPort, ok := validTURNRelayURL(cfg.URLs[0], "turn", "udp")
	if !ok {
		return errors.New("remotevoice: invalid TURN relay URL bundle")
	}
	tlsIndex := 1
	if len(cfg.URLs) == 3 {
		tcpHost, tcpPort, tcpOK := validTURNRelayURL(cfg.URLs[1], "turn", "tcp")
		if !tcpOK || tcpHost != udpHost || tcpPort != udpPort || !publicTURNRelayIPv4(udpHost) {
			return errors.New("remotevoice: invalid TURN relay URL bundle")
		}
		tlsIndex = 2
	}
	tlsHost, _, ok := validTURNRelayURL(cfg.URLs[tlsIndex], "turns", "tcp")
	if !ok || len(cfg.URLs) == 2 && !strings.EqualFold(udpHost, tlsHost) {
		// Never include a raw URL: it is caller-owned configuration and could
		// contain embedded credentials or other sensitive material.
		return errors.New("remotevoice: invalid TURN relay URL bundle")
	}
	return nil
}

func validTURNRelayURL(raw, scheme, transport string) (string, string, bool) {
	prefix := scheme + ":"
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > 2048 ||
		!strings.HasPrefix(raw, prefix) || strings.ContainsAny(raw, "@/#\t\r\n ") {
		return "", "", false
	}
	remainder := strings.TrimPrefix(raw, prefix)
	parts := strings.Split(remainder, "?")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "transport="+transport {
		return "", "", false
	}
	endpoint := parts[0]
	host := endpoint
	port := ""
	if strings.HasPrefix(endpoint, "[") {
		closing := strings.IndexByte(endpoint, ']')
		if closing < 2 || net.ParseIP(endpoint[1:closing]) == nil {
			return "", "", false
		}
		host = endpoint[1:closing]
		suffix := endpoint[closing+1:]
		if suffix != "" {
			if !strings.HasPrefix(suffix, ":") || len(suffix) == 1 {
				return "", "", false
			}
			port = suffix[1:]
		}
	} else {
		if strings.Count(endpoint, ":") > 1 {
			return "", "", false
		}
		if before, after, ok := strings.Cut(endpoint, ":"); ok {
			host, port = before, after
		}
		if !validTURNHostname(host) {
			return "", "", false
		}
	}
	if host == "" {
		return "", "", false
	}
	if port == "" {
		return "", "", false
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return "", "", false
	}
	return host, port, true
}

func publicTURNRelayIPv4(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() ||
		ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	v4 := ip.To4()
	first, second, third := v4[0], v4[1], v4[2]
	return first != 0 && first < 224 &&
		!(first == 100 && second >= 64 && second <= 127) &&
		!(first == 192 && second == 0 && (third == 0 || third == 2)) &&
		!(first == 198 && (second == 18 || second == 19 || second == 51 && third == 100)) &&
		!(first == 203 && second == 0 && third == 113)
}

func validTURNHostname(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

var (
	tailscaleIPv4 = mustParseNetwork("100.64.0.0/10")
	tailscaleIPv6 = mustParseNetwork("fd7a:115c:a1e0::/48")
	loopbackIPv4  = mustParseNetwork("127.0.0.0/8")
	loopbackIPv6  = mustParseNetwork("::1/128")
)

func mustParseNetwork(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}

func validateMediaCIDRAllowlist(values []string, allowLoopback, exactHostOnly bool) error {
	for _, value := range values {
		_, candidate, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%q: %w", value, err)
		}
		prefix, bits := candidate.Mask.Size()
		if exactHostOnly && prefix != bits {
			return fmt.Errorf("%q must identify one exact remote host", value)
		}
		if networkContainedBy(candidate, tailscaleIPv4) || networkContainedBy(candidate, tailscaleIPv6) {
			continue
		}
		if allowLoopback && (networkContainedBy(candidate, loopbackIPv4) || networkContainedBy(candidate, loopbackIPv6)) {
			continue
		}
		return fmt.Errorf("%q is outside permitted Tailscale ranges", value)
	}
	return nil
}

func networkContainedBy(candidate, permitted *net.IPNet) bool {
	if candidate == nil || permitted == nil || !permitted.Contains(candidate.IP) {
		return false
	}
	candidateOnes, candidateBits := candidate.Mask.Size()
	permittedOnes, permittedBits := permitted.Mask.Size()
	return candidateBits == permittedBits && candidateOnes >= permittedOnes
}

func restrictedSettings(cfg Config) (webrtc.SettingEngine, error) {
	var localNetworks, remoteNetworks []*net.IPNet
	if cfg.TURNRelay == nil {
		var err error
		localNetworks, err = parseCIDRs(cfg.AllowedLocalCIDRs)
		if err != nil {
			return webrtc.SettingEngine{}, fmt.Errorf("remotevoice: local CIDR allowlist: %w", err)
		}
		remoteNetworks, err = parseCIDRs(cfg.AllowedRemoteCIDRs)
		if err != nil {
			return webrtc.SettingEngine{}, fmt.Errorf("remotevoice: remote CIDR allowlist: %w", err)
		}
	}
	interfaces := make(map[string]struct{}, len(cfg.AllowedInterfaces))
	for _, name := range cfg.AllowedInterfaces {
		interfaces[name] = struct{}{}
	}

	var se webrtc.SettingEngine
	// Pion's default logger honors PION_LOG_* and includes raw SDP candidates
	// and network addresses in several warning paths. This media boundary uses
	// a fixed disabled factory so process environment cannot turn those logs on.
	se.LoggerFactory = disabledPionLoggerFactory{}
	se.DisableActiveTCP(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes(networkTypesForConfig(cfg))
	// Loopback candidates are available only for an explicit test-only config.
	se.SetIncludeLoopbackCandidate(cfg.TURNRelay == nil && cfg.allowLoopbackForTests)
	if len(interfaces) != 0 {
		se.SetInterfaceFilter(func(name string) bool {
			_, ok := interfaces[name]
			return ok
		})
	}
	if cfg.TURNRelay == nil {
		se.SetIPFilter(func(ip net.IP) bool { return containsIP(localNetworks, ip) })
		se.SetRemoteIPFilter(func(ip net.IP) bool { return containsIP(remoteNetworks, ip) })
	}
	if err := se.SetEphemeralUDPPortRange(cfg.UDPMin, cfg.UDPMax); err != nil {
		return webrtc.SettingEngine{}, fmt.Errorf("remotevoice: UDP port range: %w", err)
	}
	return se, nil
}

func networkTypesForConfig(cfg Config) []webrtc.NetworkType {
	if cfg.TURNRelay != nil {
		// UDP4 publishes the coturn UDP relay allocation. TCP4 also permits
		// Pion's client-to-TURN connection to use the TLS/TCP fallback URL;
		// the resulting selected WebRTC candidate pair remains UDP relay/relay.
		return []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	}
	return []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
}

func peerConnectionConfiguration(cfg Config) webrtc.Configuration {
	if cfg.TURNRelay == nil {
		return webrtc.Configuration{}
	}
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{
			URLs:           append([]string(nil), cfg.TURNRelay.URLs...),
			Username:       cfg.TURNRelay.Username,
			Credential:     cfg.TURNRelay.Password,
			CredentialType: webrtc.ICECredentialTypePassword,
		}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}
}

type disabledPionLoggerFactory struct{}

func (disabledPionLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return logging.NewDefaultLeveledLoggerForScope(scope, logging.LogLevelDisabled, io.Discard)
}

func parseCIDRs(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%q: %w", value, err)
		}
		result = append(result, network)
	}
	return result, nil
}

func containsIP(networks []*net.IPNet, ip net.IP) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

type selectedPairPolicy struct {
	profile        SelectedPairProfile
	localNetworks  []*net.IPNet
	remoteNetworks []*net.IPNet
}

func newSelectedPairPolicy(cfg Config) (selectedPairPolicy, error) {
	if cfg.TURNRelay != nil {
		policy := selectedPairPolicy{profile: SelectedPairProfileTURNRelay}
		if len(cfg.TURNRelay.URLs) > 0 {
			host, _, ok := validTURNRelayURL(cfg.TURNRelay.URLs[0], "turn", "udp")
			if ok {
				if ip := net.ParseIP(host).To4(); ip != nil && publicTURNRelayIPv4(host) {
					policy.remoteNetworks = []*net.IPNet{{IP: ip, Mask: net.CIDRMask(32, 32)}}
				}
			}
		}
		return policy, nil
	}
	localNetworks, err := parseCIDRs(cfg.AllowedLocalCIDRs)
	if err != nil {
		return selectedPairPolicy{}, fmt.Errorf("remotevoice: local CIDR allowlist: %w", err)
	}
	remoteNetworks, err := parseCIDRs(cfg.AllowedRemoteCIDRs)
	if err != nil {
		return selectedPairPolicy{}, fmt.Errorf("remotevoice: remote CIDR allowlist: %w", err)
	}
	return selectedPairPolicy{
		profile:       SelectedPairProfileRestrictedHost,
		localNetworks: localNetworks, remoteNetworks: remoteNetworks,
	}, nil
}

func observeSelectedPair(pair *webrtc.ICECandidatePair, policy selectedPairPolicy) SelectedPairSnapshot {
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return SelectedPairSnapshot{}
	}
	local := pair.Local
	remote := pair.Remote
	return SelectedPairSnapshot{
		Established:          true,
		Profile:              policy.profile,
		Protocol:             selectedPairProtocol(local.Protocol, remote.Protocol),
		LocalCandidateType:   selectedCandidateType(local.Typ),
		RemoteCandidateType:  selectedCandidateType(remote.Typ),
		LocalAddressAllowed:  containsIP(policy.localNetworks, net.ParseIP(local.Address)),
		RemoteAddressAllowed: containsIP(policy.remoteNetworks, net.ParseIP(remote.Address)),
	}
}

func selectedPairProtocol(local, remote webrtc.ICEProtocol) SelectedPairProtocol {
	if local != remote {
		return SelectedPairProtocolUnknown
	}
	switch local {
	case webrtc.ICEProtocolUDP:
		return SelectedPairProtocolUDP
	case webrtc.ICEProtocolTCP:
		return SelectedPairProtocolTCP
	default:
		return SelectedPairProtocolUnknown
	}
}

func selectedCandidateType(candidateType webrtc.ICECandidateType) SelectedCandidateType {
	switch candidateType {
	case webrtc.ICECandidateTypeHost:
		return SelectedCandidateTypeHost
	case webrtc.ICECandidateTypeSrflx:
		return SelectedCandidateTypeServerReflexive
	case webrtc.ICECandidateTypePrflx:
		return SelectedCandidateTypePeerReflexive
	case webrtc.ICECandidateTypeRelay:
		return SelectedCandidateTypeRelay
	default:
		return SelectedCandidateTypeUnknown
	}
}

func countMediaSections(sdp string) (total, audio int) {
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "m=") {
			continue
		}
		total++
		if strings.HasPrefix(trimmed, "m=audio ") {
			audio++
		}
	}
	return total, audio
}

func (s *Session) sendUplink(ctx context.Context, connected <-chan struct{}, track *webrtc.TrackLocalStaticSample) {
	defer s.wg.Done()
	select {
	case <-ctx.Done():
		return
	case <-connected:
	}
	s.markLocalTrackStarted()
	ticker := time.NewTicker(FrameMillis * time.Millisecond)
	defer ticker.Stop()
	silence := make([]int16, FrameSamples)
	for {
		epoch := s.currentMediaEpoch()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mediaBarrierMu.Lock()
			if s.currentMediaEpoch() != epoch {
				s.mediaBarrierMu.Unlock()
				continue
			}
			frame, ok, portErr := s.port.takeUplinkAtEpoch(epoch)
			if errors.Is(portErr, ErrStaleMediaEpoch) {
				s.mediaBarrierMu.Unlock()
				continue
			}
			if portErr != nil {
				s.mediaBarrierMu.Unlock()
				s.failPeer()
				return
			}
			if !ok {
				frame = silence
			}
			if err := track.WriteSample(media.Sample{Data: EncodePCMU(frame), Duration: FrameMillis * time.Millisecond}); err != nil {
				s.mediaBarrierMu.Unlock()
				select {
				case <-ctx.Done():
					return
				default:
					s.failPeer()
					return
				}
			} else if err == nil {
				s.markUplinkPacket(epoch, ok)
			}
			s.mediaBarrierMu.Unlock()
		}
	}
}

func (s *Session) receiveDownlink(ctx context.Context, track *webrtc.TrackRemote) error {
	assembler := pcmuFrameAssembler{samples: make([]int16, 0, FrameSamples*2)}
	for {
		epoch := s.currentMediaEpoch()
		packet, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		s.mediaBarrierMu.Lock()
		if packet.PayloadType != 0 || packet.Padding || len(packet.Payload) == 0 {
			s.mediaBarrierMu.Unlock()
			continue
		}
		if s.currentMediaEpoch() != epoch {
			assembler.reset()
			s.mediaBarrierMu.Unlock()
			continue
		}
		decoded := DecodePCMU(packet.Payload)
		frames := assembler.append(epoch, decoded)
		// Accept a first packet only after it completes at least one 20 ms PCM
		// frame. Tiny/keepalive payloads cannot authorize call control.
		if len(frames) == 0 {
			s.mediaBarrierMu.Unlock()
			continue
		}
		s.markRemoteRTP(epoch, decoded)
		for _, frame := range frames {
			if err := s.port.pushDownlinkAtEpoch(frame, epoch); errors.Is(err, ErrStaleMediaEpoch) {
				assembler.reset()
				break
			} else if err != nil {
				s.mediaBarrierMu.Unlock()
				return err
			}
		}
		select {
		case <-ctx.Done():
			s.mediaBarrierMu.Unlock()
			return ctx.Err()
		default:
		}
		s.mediaBarrierMu.Unlock()
	}
}

type pcmuFrameAssembler struct {
	samples []int16
	epoch   uint64
	set     bool
}

func (a *pcmuFrameAssembler) append(epoch uint64, decoded []int16) [][]int16 {
	if !a.set || a.epoch != epoch {
		a.samples = a.samples[:0]
		a.epoch = epoch
		a.set = true
	}
	a.samples = append(a.samples, decoded...)
	frames := make([][]int16, 0, len(a.samples)/FrameSamples)
	for len(a.samples) >= FrameSamples {
		frames = append(frames, append([]int16(nil), a.samples[:FrameSamples]...))
		a.samples = a.samples[FrameSamples:]
	}
	return frames
}

func (a *pcmuFrameAssembler) reset() {
	a.samples = a.samples[:0]
	a.set = false
}

func (s *Session) waitForDownlink(ctx context.Context, tracks <-chan *webrtc.TrackRemote) {
	defer s.wg.Done()
	select {
	case <-ctx.Done():
		return
	case track := <-tracks:
		if err := s.receiveDownlink(ctx, track); err != nil && ctx.Err() == nil {
			s.failPeer()
		}
	}
}

func (s *Session) drainRTCP(ctx context.Context, sender *webrtc.RTPSender) {
	defer s.wg.Done()
	parameters := sender.GetParameters()
	if len(parameters.Encodings) != 1 || parameters.Encodings[0].SSRC == 0 {
		s.failPeer()
		return
	}
	expectedSSRC := uint32(parameters.Encodings[0].SSRC)
	for {
		epoch := s.currentMediaEpoch()
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			if ctx.Err() == nil {
				s.failPeer()
			}
			return
		}
		observedAt := time.Now()
		if receiverReportAcknowledgesSSRC(packets, expectedSSRC) {
			s.markReceiverReport(epoch, observedAt)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func receiverReportAcknowledgesSSRC(packets []rtcp.Packet, expectedSSRC uint32) bool {
	if expectedSSRC == 0 {
		return false
	}
	for _, packet := range packets {
		var reports []rtcp.ReceptionReport
		switch report := packet.(type) {
		case *rtcp.ReceiverReport:
			reports = report.Reports
		case *rtcp.SenderReport:
			// A full-duplex browser may carry reception feedback in the report
			// blocks of its SenderReport rather than in a standalone RR.
			reports = report.Reports
		}
		for _, reception := range reports {
			// A zero extended-highest sequence number is an empty/fabricated
			// report block, not evidence that an RTP packet was received.
			if reception.SSRC == expectedSSRC && reception.LastSequenceNumber != 0 {
				return true
			}
		}
	}
	return false
}

// Connected reports the current WebRTC connection state without exposing SDP,
// ICE candidates, or the session token.
func (s *Session) Connected() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.connected
}

// NetworkActivitySnapshot returns media-plane state plus a redacted proof of
// the current selected ICE pair. Pion lookup failures and missing pairs are
// represented by an empty, policy-invalid SelectedPair snapshot.
func (s *Session) NetworkActivitySnapshot() NetworkActivitySnapshot {
	selectedPair := s.selectedPairSnapshot()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.connected || s.terminal {
		selectedPair = SelectedPairSnapshot{}
	}
	return s.networkActivitySnapshotLocked(selectedPair)
}

// Activate establishes a media epoch barrier. Workers that were already
// waiting on a tick, RTP read, or RTCP read retain the preceding epoch.
func (s *Session) Activate(epoch uint64) (NetworkActivitySnapshot, error) {
	if epoch == 0 {
		return NetworkActivitySnapshot{}, errors.New("remotevoice: network activation epoch must be non-zero")
	}
	s.mediaBarrierMu.Lock()
	defer s.mediaBarrierMu.Unlock()
	selectedPair := s.selectedPairSnapshot()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.terminal {
		return NetworkActivitySnapshot{}, ErrPeerConnectionEnded
	}
	if s.mediaEpoch != 0 && s.mediaEpoch != epoch {
		return NetworkActivitySnapshot{}, errors.New("remotevoice: network activation epoch cannot be reset")
	}
	if s.mediaEpoch == 0 {
		if err := s.port.activate(epoch); err != nil {
			return NetworkActivitySnapshot{}, err
		}
		s.mediaEpoch = epoch
	}
	if !s.connected {
		selectedPair = SelectedPairSnapshot{}
	}
	return s.networkActivitySnapshotLocked(selectedPair), nil
}

func (s *Session) currentMediaEpoch() uint64 {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.mediaEpoch
}

func (s *Session) selectedPairSnapshot() SelectedPairSnapshot {
	if s.iceTransport == nil {
		return SelectedPairSnapshot{}
	}
	pair, err := s.iceTransport.GetSelectedCandidatePair()
	if err != nil {
		// Pion errors can contain implementation and candidate details. They are
		// intentionally collapsed into fail-closed, credential-free state.
		return SelectedPairSnapshot{}
	}
	return observeSelectedPair(pair, s.selectedPairPolicy)
}

func (s *Session) networkActivitySnapshotLocked(selectedPair SelectedPairSnapshot) NetworkActivitySnapshot {
	return NetworkActivitySnapshot{
		Connected: s.connected, SelectedPair: selectedPair, LocalTrackStarted: s.localTrackStarted,
		UplinkPackets: s.uplinkPackets, LastUplinkAt: s.lastUplinkAt, LastUplinkEpoch: s.lastUplinkEpoch,
		RealUplinkPackets: s.realUplinkPackets, LastRealUplinkAt: s.lastRealUplinkAt,
		LastRealUplinkEpoch:                   s.lastRealUplinkEpoch,
		RealUplinkPacketsAtLastReceiverReport: s.realUplinkPacketsAtLastReceiverReport,
		RemoteTrackReady:                      s.remoteTrackReady, RemoteRTPPackets: s.remoteRTPPackets,
		RemoteRTPSamples: s.remoteRTPSamples, RemoteRTPSignalSamples: s.remoteRTPSignalSamples,
		RemoteRTPPeakPCM16: s.remoteRTPPeakPCM16,
		LastRemoteRTPAt:    s.lastRemoteRTPAt, LastRemoteRTPEpoch: s.lastRemoteRTPEpoch,
		ReceiverReports: s.receiverReports, LastReceiverReportAt: s.lastReceiverReportAt,
		LastReceiverReportEpoch: s.lastReceiverReportEpoch,
	}
}

func (s *Session) markLocalTrackStarted() {
	s.stateMu.Lock()
	if !s.terminal {
		s.localTrackStarted = true
	}
	s.stateMu.Unlock()
}

func (s *Session) markUplinkPacket(epoch uint64, real bool) {
	s.stateMu.Lock()
	if !s.terminal {
		now := time.Now()
		s.uplinkPackets++
		s.lastUplinkAt = now
		s.lastUplinkEpoch = epoch
		if real {
			s.realUplinkPackets++
			s.lastRealUplinkAt = now
			s.lastRealUplinkEpoch = epoch
			if !s.realUplinkProofPending || s.realUplinkProofEpoch != epoch {
				s.realUplinkProofPending = true
				s.realUplinkProofEpoch = epoch
				s.realUplinkProofAt = now
				s.realUplinkProofPackets = s.realUplinkPackets
				s.realUplinkProofReceiverReportBaseline = s.receiverReports
			}
		}
	}
	s.stateMu.Unlock()
}

func (s *Session) markRemoteRTP(epoch uint64, samples []int16) {
	var signal uint64
	var peak uint32
	for _, sample := range samples {
		magnitude := int32(sample)
		if magnitude < 0 {
			magnitude = -magnitude
		}
		if uint32(magnitude) > peak {
			peak = uint32(magnitude)
		}
		if magnitude > 256 {
			signal++
		}
	}
	s.stateMu.Lock()
	if !s.terminal {
		s.remoteTrackReady = true
		s.remoteRTPPackets++
		s.remoteRTPSamples += uint64(len(samples))
		s.remoteRTPSignalSamples += signal
		if peak > s.remoteRTPPeakPCM16 {
			s.remoteRTPPeakPCM16 = peak
		}
		s.lastRemoteRTPAt = time.Now()
		s.lastRemoteRTPEpoch = epoch
	}
	s.stateMu.Unlock()
}

func (s *Session) markReceiverReport(epoch uint64, observedAt time.Time) {
	s.stateMu.Lock()
	if !s.terminal && s.mediaEpoch == epoch {
		s.receiverReports++
		s.lastReceiverReportAt = observedAt
		s.lastReceiverReportEpoch = epoch
		if s.realUplinkProofPending && s.realUplinkProofEpoch == epoch &&
			s.receiverReports > s.realUplinkProofReceiverReportBaseline &&
			!observedAt.Before(s.realUplinkProofAt) {
			s.realUplinkPacketsAtLastReceiverReport = s.realUplinkProofPackets
			s.realUplinkProofPending = false
		}
	}
	s.stateMu.Unlock()
}

// Wait waits until the peer terminates or the supplied context is canceled.
// A locally initiated Close completes Wait without an error.
func (s *Session) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.terminalDone:
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		return s.terminalErr
	}
}

func (s *Session) setConnected(connected bool) {
	s.stateMu.Lock()
	if !s.terminal {
		s.connected = connected
	}
	s.stateMu.Unlock()
}

func (s *Session) signalTerminal(err error) {
	s.terminalOnce.Do(func() {
		s.stateMu.Lock()
		s.connected = false
		s.terminal = true
		s.terminalErr = err
		s.stateMu.Unlock()
		close(s.terminalDone)
	})
}

func (s *Session) failPeer() {
	s.signalTerminal(ErrPeerConnectionEnded)
	// A media worker cannot call Session.Close because Close waits for every
	// worker. Cancel siblings here; the lease owner (or standalone caller after
	// Wait) closes the Pion peer and unblocks any outstanding RTP read.
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.pc != nil {
			s.closeErr = s.pc.Close()
		}
		s.wg.Wait()
		s.signalTerminal(nil)
	})
	return s.closeErr
}
