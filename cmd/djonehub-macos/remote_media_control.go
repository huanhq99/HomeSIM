package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type remoteMediaControlRequest struct {
	Action          string `json:"action"`
	MediaSessionID  string `json:"media_session_id,omitempty"`
	LeaseGeneration uint64 `json:"lease_generation,omitempty"`
	UACUIDDigest    string `json:"uac_uid_digest,omitempty"`
	ActivationEpoch uint64 `json:"activation_epoch,omitempty"`
	CaptureFrames   uint64 `json:"capture_frames,omitempty"`
	PlaybackFrames  uint64 `json:"playback_frames,omitempty"`
}

type remoteMediaControlLeaseResponse struct {
	MediaSessionID         string `json:"media_session_id"`
	LeaseGeneration        uint64 `json:"lease_generation"`
	Purpose                string `json:"purpose"`
	ExpectedCallGeneration uint64 `json:"expected_call_generation"`
	PCMSocketPath          string `json:"pcm_socket_path"`
	PCMTokenBase64         string `json:"pcm_token_base64"`
	VendorID               uint16 `json:"vendor_id"`
	ProductID              uint16 `json:"product_id"`
	LocationID             uint32 `json:"location_id"`
	ExpiresAt              string `json:"expires_at"`
}

func (*remoteMediaControlLeaseResponse) String() string {
	return "remoteMediaControlLeaseResponse{redacted}"
}
func (*remoteMediaControlLeaseResponse) GoString() string {
	return "remoteMediaControlLeaseResponse{redacted}"
}

type remoteMediaControlClaimEnvelope struct {
	OK    bool                             `json:"ok"`
	Lease *remoteMediaControlLeaseResponse `json:"lease"`
}

type remoteMediaControlSimpleEnvelope struct {
	OK bool `json:"ok"`
}

type remoteMediaControlActivationEnvelope struct {
	OK              bool   `json:"ok"`
	ActivationEpoch uint64 `json:"activation_epoch,omitempty"`
}

type remoteMediaControlServer struct {
	listener *net.UnixListener
	path     string
	app      *app
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	once     sync.Once
	slots    chan struct{}
}

func (a *app) startRemoteMediaControlServer(parent context.Context) (*remoteMediaControlServer, error) {
	if a.remoteMedia == nil {
		return nil, nil
	}
	if err := remoteMediaControlPlatformSupported(); err != nil {
		return nil, err
	}
	path := a.remoteMedia.cfg.ControlPath
	if len([]byte(path)) > 103 {
		return nil, errors.New("remote media control socket path exceeds macOS limit")
	}
	if err := ensurePrivateRuntimeDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := removeStaleRemoteMediaControlSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on remote media control socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	if err := secureRemoteMediaControlSocket(path); err != nil {
		_ = listener.Close()
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 || !fileOwnedByCurrentUser(info) {
		_ = listener.Close()
		return nil, errors.New("remote media control socket failed type/owner/mode validation")
	}
	ctx, cancel := context.WithCancel(parent)
	server := &remoteMediaControlServer{
		listener: listener, path: path, app: a, ctx: ctx, cancel: cancel,
		slots: make(chan struct{}, 4),
	}
	server.wg.Add(1)
	go server.serve()
	return server, nil
}

func removeStaleRemoteMediaControlSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("remote media control socket path cannot be inspected")
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 || !fileOwnedByCurrentUser(info) {
		return errors.New("remote media control socket path already exists with an unexpected type or owner")
	}
	conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return errors.New("remote media control socket is already in use")
	}
	if err := os.Remove(path); err != nil {
		return errors.New("stale remote media control socket cannot be removed")
	}
	return nil
}

func (s *remoteMediaControlServer) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.once.Do(func() {
		s.cancel()
		closeErr = s.listener.Close()
		s.wg.Wait()
	})
	return closeErr
}

func (s *remoteMediaControlServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				return
			}
		}
		select {
		case s.slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *remoteMediaControlServer) handle(conn *net.UnixConn) {
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	uid, err := remoteMediaControlPeerEUID(conn)
	if err != nil || uid != uint32(os.Geteuid()) {
		return
	}
	payload, err := io.ReadAll(io.LimitReader(conn, remoteMediaControlBodyLimit+1))
	if err != nil || len(payload) == 0 || len(payload) > remoteMediaControlBodyLimit ||
		payload[len(payload)-1] != '\n' || bytes.Contains(payload[:len(payload)-1], []byte{'\n'}) ||
		bytes.Contains(payload[:len(payload)-1], []byte{'\r'}) {
		_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
		return
	}
	var request remoteMediaControlRequest
	decoder := json.NewDecoder(bytes.NewReader(payload[:len(payload)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
		return
	}
	switch request.Action {
	case "claim":
		if request.MediaSessionID != "" || request.LeaseGeneration != 0 || request.UACUIDDigest != "" ||
			request.ActivationEpoch != 0 || request.CaptureFrames != 0 || request.PlaybackFrames != 0 {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlClaimEnvelope{OK: false, Lease: nil})
			return
		}
		lease, generation, err := s.app.claimRemoteMediaForHost()
		if err != nil {
			log.Printf("remote media host claim unavailable")
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlClaimEnvelope{OK: true, Lease: nil})
			return
		}
		if lease == nil {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlClaimEnvelope{OK: true, Lease: nil})
			return
		}
		response := remoteMediaControlClaimEnvelope{OK: true, Lease: lease}
		log.Printf("remote media host claim issued: purpose=%s", lease.Purpose)
		if err := writeRemoteMediaControlLine(conn, response); err != nil {
			s.app.remoteMedia.closeIfGeneration(generation)
		}
		// Best-effort clear the short-lived base64 copy held by this response.
		response.Lease.PCMTokenBase64 = ""
	case "prepared":
		if request.MediaSessionID == "" || request.LeaseGeneration == 0 ||
			!remoteMediaDigestPattern.MatchString(request.UACUIDDigest) || request.ActivationEpoch != 0 ||
			request.CaptureFrames != 0 || request.PlaybackFrames != 0 {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
			return
		}
		if err := s.app.markRemoteMediaHostPrepared(request); err != nil {
			log.Printf("remote media host preparation refused")
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
			return
		}
		log.Printf("remote media host prepared")
		_ = s.writeRemoteMediaMutationSuccess(
			conn, request.LeaseGeneration, remoteMediaControlSimpleEnvelope{OK: true},
		)
	case "activation":
		if request.MediaSessionID == "" || request.LeaseGeneration == 0 || request.UACUIDDigest != "" ||
			request.ActivationEpoch != 0 || request.CaptureFrames != 0 || request.PlaybackFrames != 0 {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlActivationEnvelope{OK: false})
			return
		}
		// Activation performs fresh, lifecycle-pinned AT and UAC checks. Keep the
		// short read deadline above for idle clients, but give this authenticated
		// local operation enough time to return its one-shot epoch.
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		epoch, err := s.app.activateRemoteMediaForHost(request)
		if err != nil || epoch == 0 {
			if err != nil {
				log.Printf("remote media host activation refused: %v", err)
			} else {
				log.Printf("remote media host activation refused: zero activation epoch")
			}
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlActivationEnvelope{OK: false})
			return
		}
		if err := s.writeRemoteMediaMutationSuccess(conn, request.LeaseGeneration, remoteMediaControlActivationEnvelope{
			OK: true, ActivationEpoch: epoch,
		}); err != nil {
			return
		}
	case "activity":
		if request.MediaSessionID == "" || request.LeaseGeneration == 0 || request.UACUIDDigest != "" ||
			request.ActivationEpoch == 0 || (request.CaptureFrames == 0 && request.PlaybackFrames == 0) {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
			return
		}
		if err := s.app.markRemoteMediaHostActivity(request); err != nil {
			_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
			return
		}
		_ = s.writeRemoteMediaMutationSuccess(
			conn, request.LeaseGeneration, remoteMediaControlSimpleEnvelope{OK: true},
		)
	default:
		_ = writeRemoteMediaControlLine(conn, remoteMediaControlSimpleEnvelope{OK: false})
	}
}

func writeRemoteMediaControlLine(w io.Writer, response any) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	defer func() {
		for index := range payload {
			payload[index] = 0
		}
	}()
	if len(payload)+1 > remoteMediaControlBodyLimit {
		return errors.New("remote media control response is oversized")
	}
	payload = append(payload, '\n')
	written, err := w.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

// A prepared/activity/activation mutation is committed before its success ACK
// is sent. If the same-UID Swift client cannot observe the complete ACK, the
// exact generation is revoked so a retry can never mistake hidden prior state
// for a fresh success. closeIfGeneration makes late failures ABA-safe.
func (s *remoteMediaControlServer) writeRemoteMediaMutationSuccess(
	w io.Writer,
	generation uint64,
	response any,
) error {
	err := writeRemoteMediaControlLine(w, response)
	if err != nil && s != nil && s.app != nil && s.app.remoteMedia != nil {
		s.app.remoteMedia.closeIfGeneration(generation)
	}
	return err
}

func (a *app) claimRemoteMediaForHost() (*remoteMediaControlLeaseResponse, uint64, error) {
	m := a.remoteMedia
	if m == nil {
		return nil, 0, nil
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil {
		generation := uint64(0)
		if entry != nil {
			generation = entry.generation
		}
		m.mu.Unlock()
		m.closeIfGeneration(generation)
		return nil, 0, nil
	}
	if entry.actionPending || entry.activationPending || entry.activationEpoch != 0 {
		m.mu.Unlock()
		return nil, 0, nil
	}
	if !m.now().Before(entry.expiresAt) {
		generation := entry.generation
		m.mu.Unlock()
		m.closeIfGeneration(generation)
		return nil, 0, nil
	}
	generation := entry.generation
	coordinator := entry.coordinator
	expiresAt := entry.expiresAt
	sessionID := entry.sessionID
	source := entry.source
	m.mu.Unlock()
	if !a.remoteMediaSourceCurrent(source) {
		m.closeIfGeneration(generation)
		return nil, 0, nil
	}

	device, identity, ok := a.currentUSBATIdentitySnapshot()
	validateUAC := m.validateUAC
	if validateUAC == nil {
		validateUAC = validateDirectUACUSB
	}
	if !ok || identity.VendorID != quectelUSBVendorID || identity.ProductID != quectelUSBProductID ||
		identity.Location == 0 || validateUAC(identity.Location) != nil {
		return nil, generation, errors.New("exact target UAC descriptor is unavailable")
	}
	claim, err := coordinator.ClaimIPCLease()
	if err != nil {
		return nil, generation, err
	}

	if !a.isCurrentUSBAT(device, identity.Location) {
		m.closeIfGeneration(generation)
		return nil, generation, errors.New("USB lifecycle changed during claim")
	}
	m.mu.Lock()
	if m.active != entry || m.closed || !m.now().Before(entry.expiresAt) {
		m.mu.Unlock()
		m.closeIfGeneration(generation)
		return nil, generation, errors.New("remote media lease or USB lifecycle changed during claim")
	}
	entry.claimDevice = device
	entry.claimIdentity = identity
	m.mu.Unlock()

	encodedToken := base64.StdEncoding.EncodeToString(claim.SessionToken[:])
	for index := range claim.SessionToken {
		claim.SessionToken[index] = 0
	}
	return &remoteMediaControlLeaseResponse{
		MediaSessionID: sessionID, LeaseGeneration: generation,
		Purpose: source.Purpose, ExpectedCallGeneration: source.ExpectedCallGeneration,
		PCMSocketPath: claim.SocketPath, PCMTokenBase64: encodedToken,
		VendorID: uint16(identity.VendorID), ProductID: uint16(identity.ProductID),
		LocationID: identity.Location, ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}, generation, nil
}

func (a *app) markRemoteMediaHostPrepared(request remoteMediaControlRequest) error {
	m := a.remoteMedia
	if m == nil {
		return errors.New("remote media is disabled")
	}
	m.mu.Lock()
	entry := m.active
	if !remoteMediaDigestPattern.MatchString(request.UACUIDDigest) || m.closed || entry == nil ||
		!entry.coordinator.Owns(request.MediaSessionID, request.LeaseGeneration) ||
		entry.claimDevice == nil || entry.claimIdentity.Location == 0 ||
		entry.actionPending || entry.activationPending || entry.activationEpoch != 0 ||
		!m.now().Before(entry.expiresAt) {
		m.mu.Unlock()
		return errors.New("remote media lease no longer owns the local host")
	}
	device := entry.claimDevice
	identity := entry.claimIdentity
	generation := entry.generation
	source := entry.source
	m.mu.Unlock()

	validateUAC := m.validateUAC
	if validateUAC == nil {
		validateUAC = validateDirectUACUSB
	}
	if !a.remoteMediaSourceCurrent(source) || !a.isCurrentUSBAT(device, identity.Location) ||
		validateUAC(identity.Location) != nil {
		m.closeIfGeneration(generation)
		return errors.New("call or UAC lifecycle changed during host preparation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != entry || m.closed || entry.actionPending || entry.activationPending ||
		entry.activationEpoch != 0 || !m.now().Before(entry.expiresAt) {
		return errors.New("remote media lease changed during host preparation")
	}
	entry.hostPrepared = true
	entry.uacUIDDigest = request.UACUIDDigest
	return nil
}

func (a *app) activateRemoteMediaForHost(request remoteMediaControlRequest) (uint64, error) {
	m := a.remoteMedia
	if m == nil {
		return 0, errors.New("remote media is disabled")
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(request.MediaSessionID, request.LeaseGeneration) ||
		!entry.hostPrepared || entry.claimDevice == nil || entry.claimIdentity.Location == 0 {
		m.mu.Unlock()
		return 0, errors.New("remote media lease no longer owns the local host")
	}
	if !entry.actionPending || entry.actionToken == 0 {
		m.mu.Unlock()
		return 0, errors.New("remote media lease has no completed call action")
	}
	device := entry.claimDevice
	identity := entry.claimIdentity
	generation := entry.generation
	source := entry.source
	m.mu.Unlock()

	ticket, ok := a.currentCallMediaTicket()
	validateUAC := m.validateUAC
	if validateUAC == nil {
		validateUAC = validateDirectUACUSB
	}
	if !ok || !a.remoteMediaSourceMatchesActiveCall(source, ticket) ||
		!a.isCurrentUSBAT(device, identity.Location) || validateUAC(identity.Location) != nil {
		m.closeIfGeneration(generation)
		return 0, errors.New("active call or UAC lifecycle no longer matches the media lease")
	}
	// This performs a new exact-handle CGSN/CLCC/QPCMV read through the direct
	// route owner. It does not start media and does not mutate the modem.
	if err := a.confirmModuleVoiceCall(ticket); err != nil {
		m.closeIfGeneration(generation)
		return 0, errors.New("active QPCMV route could not be freshly confirmed")
	}
	route := a.directQPCMVRouteState()
	if !route.ready || route.phase != "ready" || route.generation != ticket.Generation ||
		route.locationID != identity.Location || route.identityHash == "" {
		m.closeIfGeneration(generation)
		return 0, errors.New("active QPCMV owner changed after fresh confirmation")
	}
	epoch, err := m.activate(
		request.MediaSessionID, request.LeaseGeneration, ticket, route.identityHash,
	)
	if err != nil {
		return 0, err
	}
	if !a.callMediaTicketIsCurrent(ticket) || !a.isCurrentUSBAT(device, identity.Location) {
		m.closeIfGeneration(generation)
		return 0, errors.New("active call or USB lifecycle changed during media activation")
	}
	return epoch, nil
}

func (a *app) remoteMediaSourceMatchesActiveCall(source remoteMediaSource, ticket callMediaTicket) bool {
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 {
		return false
	}
	switch source.Purpose {
	case "incoming":
		return ticket.CallID == source.Call.CallID && ticket.Index == source.Call.CallIndex &&
			ticket.Direction == source.Call.CallDirection
	case "outgoing":
		if ticket.Direction != "outgoing" || ticket.Generation <= source.ExpectedCallGeneration {
			return false
		}
		a.callMu.RLock()
		defer a.callMu.RUnlock()
		if a.activeCall == nil || a.activeCall.ID != ticket.CallID || a.activeCall.Index != ticket.Index ||
			a.callGeneration != ticket.Generation || a.activeCall.Direction != "outgoing" {
			return false
		}
		// Some firmware omits the number from CLCC after the call becomes active.
		// When it is present, bind it to the normalized number that owned ATD.
		return a.activeCall.Number == "" || normalizeDialNumber(a.activeCall.Number) == source.DialNumber
	default:
		return false
	}
}

func (a *app) markRemoteMediaHostActivity(request remoteMediaControlRequest) error {
	m := a.remoteMedia
	if m == nil {
		return errors.New("remote media is disabled")
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(request.MediaSessionID, request.LeaseGeneration) ||
		entry.activationEpoch != request.ActivationEpoch || entry.claimDevice == nil {
		m.mu.Unlock()
		return errors.New("remote media activity has no active owner")
	}
	device := entry.claimDevice
	identity := entry.claimIdentity
	ticket := entry.activeTicket
	generation := entry.generation
	m.mu.Unlock()
	if !a.callMediaTicketIsCurrent(ticket) || !a.isCurrentUSBAT(device, identity.Location) {
		m.closeIfGeneration(generation)
		return errors.New("remote media activity belongs to a stale call or USB lifecycle")
	}
	return m.markHostActivity(
		request.MediaSessionID,
		request.LeaseGeneration,
		request.ActivationEpoch,
		request.CaptureFrames,
		request.PlaybackFrames,
	)
}
