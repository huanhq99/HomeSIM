package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
)

var (
	errRemoteRescueHangupRejected  = errors.New("modem explicitly rejected rescue hangup")
	errRemoteRescueHangupAmbiguous = errors.New("rescue hangup outcome is ambiguous")
)

const (
	remoteMediaRescuePhaseLive       = "live"
	remoteMediaRescuePhaseTombstone  = "tombstone"
	remoteMediaRescuePhaseInProgress = "in_progress"
	remoteMediaRescuePhaseConsumed   = "consumed"
)

type remoteMediaRescueReservation struct {
	grantToken   uint64
	attemptToken uint64
	generation   uint64
	sessionID    string
	entry        *remoteMediaLease
}

func (*remoteMediaRescueGrant) String() string   { return "remoteMediaRescueGrant{redacted}" }
func (*remoteMediaRescueGrant) GoString() string { return "remoteMediaRescueGrant{redacted}" }

func (r remoteMediaRescueReservation) valid() bool {
	return r.grantToken != 0 && r.attemptToken != 0 && r.generation != 0 && r.sessionID != ""
}

func (remoteMediaRescueReservation) String() string { return "remoteMediaRescueReservation{redacted}" }
func (remoteMediaRescueReservation) GoString() string {
	return "remoteMediaRescueReservation{redacted}"
}

func remoteMediaRescueGrantOwns(
	grant *remoteMediaRescueGrant,
	now time.Time,
	identity string,
	mediaSessionID string,
	generation uint64,
	ticket callMediaTicket,
) bool {
	if grant == nil || grant.token == 0 || grant.attemptToken != 0 ||
		(grant.phase != remoteMediaRescuePhaseLive && grant.phase != remoteMediaRescuePhaseTombstone) ||
		grant.identity != identity || grant.sessionID != mediaSessionID || grant.generation != generation ||
		grant.activationEpoch == 0 || grant.proofAt.IsZero() || grant.activeTicket != ticket ||
		grant.source.Purpose != "incoming" || grant.source.Call.CallID != ticket.CallID ||
		grant.source.Call.CallIndex != ticket.Index || grant.source.Call.CallDirection != ticket.Direction ||
		grant.source.Call.CallGeneration >= ticket.Generation ||
		grant.claimDevice == nil || grant.claimIdentity.Location == 0 ||
		grant.moduleVoiceIdentityHash == "" {
		return false
	}
	return grant.phase != remoteMediaRescuePhaseTombstone || now.Before(grant.expiresAt)
}

func (m *remoteMediaManager) clearRescueGrantLocked() {
	if m.rescue != nil && m.rescue.expirationTimer != nil {
		m.rescue.expirationTimer.Stop()
	}
	m.rescue = nil
}

// maybeLatchRescueProof records the first complete live-media proof without
// relying on an HTTP status poll. The proof is boot-local and binds the exact
// activation epoch, call ticket, USB lifecycle and module identity hash.
func (m *remoteMediaManager) maybeLatchRescueProof(
	entry *remoteMediaLease,
	media remotevoice.MediaLeaseSnapshot,
	now time.Time,
) {
	if m == nil || entry == nil || !media.ActiveFresh {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maybeLatchRescueProofLocked(entry, media, now)
}

func (m *remoteMediaManager) maybeLatchRescueProofLocked(
	entry *remoteMediaLease,
	media remotevoice.MediaLeaseSnapshot,
	now time.Time,
) {
	if m.closed || m.active != entry || entry.activationEpoch == 0 || entry.activeTicket.Generation == 0 ||
		entry.claimDevice == nil || entry.claimIdentity.Location == 0 || entry.moduleVoiceIdentityHash == "" ||
		entry.source.Purpose != "incoming" || entry.source.Call.CallID != entry.activeTicket.CallID ||
		entry.source.Call.CallIndex != entry.activeTicket.Index ||
		entry.source.Call.CallDirection != entry.activeTicket.Direction ||
		entry.source.Call.CallGeneration >= entry.activeTicket.Generation ||
		entry.hostCaptureFrames == 0 || entry.hostPlaybackFrames == 0 || entry.hostCaptureAt.IsZero() ||
		entry.hostPlaybackAt.IsZero() || now.Before(entry.hostCaptureAt) || now.Before(entry.hostPlaybackAt) ||
		now.Sub(entry.hostCaptureAt) > 2*time.Second || now.Sub(entry.hostPlaybackAt) > 2*time.Second {
		return
	}
	// A live grant is idempotent. In-progress and consumed grants are also
	// deliberately sticky for this exact call epoch: later media activity must
	// never manufacture a second one-shot authority after ATH was attempted.
	if grant := m.rescue; grant != nil && grant.generation == entry.generation &&
		grant.activationEpoch == entry.activationEpoch && grant.activeTicket == entry.activeTicket {
		return
	}
	m.clearRescueGrantLocked()
	m.nextRescueToken++
	if m.nextRescueToken == 0 {
		m.nextRescueToken++
	}
	m.rescue = &remoteMediaRescueGrant{
		identity: entry.identity, sessionID: entry.sessionID, generation: entry.generation,
		activationEpoch: entry.activationEpoch, proofAt: now, source: entry.source,
		activeTicket: entry.activeTicket,
		claimDevice:  entry.claimDevice, claimIdentity: entry.claimIdentity,
		moduleVoiceIdentityHash: entry.moduleVoiceIdentityHash,
		phase:                   remoteMediaRescuePhaseLive, token: m.nextRescueToken,
	}
}

func (m *remoteMediaManager) retireRescueGrantAfterTransportEndLocked(
	entry *remoteMediaLease,
	reason remotevoice.MediaLeaseCloseReason,
) {
	grant := m.rescue
	if entry == nil || grant == nil || grant.phase != remoteMediaRescuePhaseLive ||
		grant.generation != entry.generation || grant.sessionID != entry.sessionID ||
		(reason != remotevoice.MediaLeaseClosePeerEnded && reason != remotevoice.MediaLeaseCloseBrokerEnded) {
		m.clearRescueGrantLocked()
		return
	}
	ttl := m.rescueTTL
	if ttl <= 0 {
		ttl = remoteMediaDefaultRescueTTL
	}
	grant.phase = remoteMediaRescuePhaseTombstone
	grant.expiresAt = m.now().Add(ttl)
	grant.expirationTimer = time.AfterFunc(ttl, func() {
		m.expireRescueGrant(grant.token)
	})
}

func (m *remoteMediaManager) expireRescueGrant(token uint64) {
	if m == nil || token == 0 {
		return
	}
	m.mu.Lock()
	if grant := m.rescue; grant != nil && grant.token == token &&
		grant.phase == remoteMediaRescuePhaseTombstone && !m.now().Before(grant.expiresAt) {
		m.rescue = nil
	}
	m.mu.Unlock()
}

func (m *remoteMediaManager) closeForCallGeneration(generation uint64) {
	if m == nil || generation == 0 {
		return
	}
	m.mu.Lock()
	entry := m.active
	closeEntry := entry != nil && entry.activeTicket.Generation == generation
	if closeEntry {
		m.active = nil
	}
	if grant := m.rescue; grant != nil && grant.activeTicket.Generation == generation {
		m.clearRescueGrantLocked()
	}
	m.mu.Unlock()
	if closeEntry {
		closeRemoteMediaLease(entry)
	}
}

func (m *remoteMediaManager) rescueStatusLocked(
	now time.Time,
	identity string,
) (bool, uint64, callMediaTicket) {
	grant := m.rescue
	if grant == nil || grant.attemptToken != 0 || grant.identity != identity ||
		(grant.phase != remoteMediaRescuePhaseLive && grant.phase != remoteMediaRescuePhaseTombstone) {
		return false, 0, callMediaTicket{}
	}
	if grant.phase == remoteMediaRescuePhaseTombstone && !now.Before(grant.expiresAt) {
		m.clearRescueGrantLocked()
		return false, 0, callMediaTicket{}
	}
	return true, grant.generation, grant.activeTicket
}

func (m *remoteMediaManager) authorizeRescueHangup(
	identity string,
	mediaSessionID string,
	generation uint64,
	ticket callMediaTicket,
) error {
	if m == nil || !remoteMediaSessionPattern.MatchString(mediaSessionID) || generation == 0 {
		return errors.New("invalid rescue media owner")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !remoteMediaRescueGrantOwns(m.rescue, m.now(), identity, mediaSessionID, generation, ticket) {
		return errors.New("remote media proof cannot rescue this active call")
	}
	return nil
}

// beginRescueHangup atomically consumes the one-use proof while the caller
// holds the pinned USB lifecycle, device-command and cached-call read locks.
// No failure after this point may authorize another AT attempt.
func (m *remoteMediaManager) beginRescueHangup(
	identity string,
	mediaSessionID string,
	generation uint64,
	ticket callMediaTicket,
	device *usbAT,
	pinnedIdentity usbATPhysicalIdentity,
	moduleIdentityHash string,
) (remoteMediaRescueReservation, error) {
	if m == nil || device == nil || pinnedIdentity.Location == 0 || moduleIdentityHash == "" ||
		pinnedIdentity.VendorID != quectelUSBVendorID || pinnedIdentity.ProductID != quectelUSBProductID {
		return remoteMediaRescueReservation{}, errors.New("invalid pinned rescue device")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	defer m.mu.Unlock()
	grant := m.rescue
	if m.closed || !remoteMediaRescueGrantOwns(grant, m.now(), identity, mediaSessionID, generation, ticket) ||
		grant.claimDevice != device || grant.claimIdentity != pinnedIdentity ||
		grant.moduleVoiceIdentityHash != moduleIdentityHash {
		return remoteMediaRescueReservation{}, errors.New("rescue owner changed before the pinned hangup")
	}
	m.nextActionToken++
	if m.nextActionToken == 0 {
		m.nextActionToken++
	}
	grant.phase = remoteMediaRescuePhaseInProgress
	grant.attemptToken = m.nextActionToken
	if grant.expirationTimer != nil {
		grant.expirationTimer.Stop()
		grant.expirationTimer = nil
	}
	return remoteMediaRescueReservation{
		grantToken: grant.token, attemptToken: grant.attemptToken,
		generation: grant.generation, sessionID: grant.sessionID, entry: m.active,
	}, nil
}

// finishRescueHangupAttempt consumes the authority for every command outcome.
// An explicit OK also closes the exact media lease; rejected or ambiguous
// outcomes leave any live audio intact but can never issue another AT command.
func (m *remoteMediaManager) finishRescueHangupAttempt(
	reservation remoteMediaRescueReservation,
	accepted bool,
) {
	if m == nil || !reservation.valid() {
		return
	}
	m.mu.Lock()
	grant := m.rescue
	if grant == nil || grant.token != reservation.grantToken || grant.attemptToken != reservation.attemptToken ||
		grant.generation != reservation.generation || grant.sessionID != reservation.sessionID {
		m.mu.Unlock()
		return
	}
	entry := m.active
	if !accepted {
		grant.phase = remoteMediaRescuePhaseConsumed
		grant.attemptToken = 0
		grant.expiresAt = time.Time{}
		if grant.expirationTimer != nil {
			grant.expirationTimer.Stop()
			grant.expirationTimer = nil
		}
		m.mu.Unlock()
		return
	}
	m.rescue = nil
	if entry == nil || entry != reservation.entry || entry.generation != reservation.generation ||
		entry.sessionID != reservation.sessionID {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
}

func directQPCMVStateOwnsRescueHangup(state directQPCMVRouteState, ticket callMediaTicket) bool {
	return state.ready && state.phase == "ready" && state.locationID != 0 && state.identityHash != "" &&
		state.generation == ticket.Generation && directQPCMVIntentMatchesTicket(state.intent, ticket)
}

func remoteRescueHangupCommandError(response string, err error) error {
	classified := directCallCommandError("ATH", response, err)
	if classified == nil {
		return nil
	}
	if errors.Is(classified, errDirectCallCommandRejected) {
		return errRemoteRescueHangupRejected
	}
	return fmt.Errorf("%w", errRemoteRescueHangupAmbiguous)
}

func (a *app) confirmVoiceRouteForHangup(command usbATCommandFunc) error {
	if a.implicitUACVoice {
		return nil
	}
	return queryDirectQPCMVEnabled(command)
}

func (a *app) executeRemoteRescueHangup(
	identity string,
	mediaSessionID string,
	generation uint64,
	ticket callMediaTicket,
) (remoteMediaRescueReservation, error) {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return remoteMediaRescueReservation{}, err
	}
	if a.remoteMedia == nil || !a.callMediaTicketIsCurrent(ticket) {
		return remoteMediaRescueReservation{}, errors.New("active call ticket is no longer current")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if !directQPCMVStateOwnsRescueHangup(state, ticket) {
		return remoteMediaRescueReservation{}, errors.New("direct UAC route does not own this exact call")
	}

	var reservation remoteMediaRescueReservation
	_, sessionErr := a.withExclusiveUSBATCommandsAtLocationPinned(
		state.locationID,
		func(device *usbAT, physical usbATPhysicalIdentity, command usbATCommandFunc) error {
			if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
				return err
			}
			clcc, err := command("AT+CLCC", directQPCMVCommandTimeout)
			if err != nil {
				return errors.New("fresh CLCC did not confirm the exact single active call")
			}
			calls, parseErr := parseCLCCForMutation(clcc)
			if parseErr != nil || !singleActiveCallMatches(calls, ticket) {
				return errors.New("fresh CLCC did not confirm the exact single active call")
			}
			if err := a.confirmVoiceRouteForHangup(command); err != nil {
				return err
			}
			a.callMu.RLock()
			defer a.callMu.RUnlock()
			if !a.callMediaTicketIsCurrentLocked(ticket) {
				return errors.New("cached call topology changed before rescue hangup")
			}
			reserved, err := a.remoteMedia.beginRescueHangup(
				identity, mediaSessionID, generation, ticket, device, physical, state.identityHash,
			)
			if err != nil {
				return err
			}
			reservation = reserved
			response, commandErr := command("ATH", 5*time.Second)
			return remoteRescueHangupCommandError(response, commandErr)
		},
	)
	if reservation.valid() {
		a.remoteMedia.finishRescueHangupAttempt(reservation, sessionErr == nil)
	}
	return reservation, sessionErr
}

func (a *app) executeRemoteAuthenticatedHangup(ticket callMediaTicket) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if !a.callMediaTicketIsCurrent(ticket) {
		return errors.New("active call ticket is no longer current")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if !directQPCMVStateOwnsRescueHangup(state, ticket) {
		return errors.New("direct UAC route does not own this exact call")
	}
	_, err := a.withExclusiveUSBATCommandsAtLocationPinned(
		state.locationID,
		func(_ *usbAT, _ usbATPhysicalIdentity, command usbATCommandFunc) error {
			if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
				return err
			}
			clcc, err := command("AT+CLCC", directQPCMVCommandTimeout)
			if err != nil {
				return errors.New("fresh CLCC did not confirm the exact single active call")
			}
			calls, parseErr := parseCLCCForMutation(clcc)
			if parseErr != nil || !singleActiveCallMatches(calls, ticket) {
				return errors.New("fresh CLCC did not confirm the exact single active call")
			}
			if err := a.confirmVoiceRouteForHangup(command); err != nil {
				return err
			}
			a.callMu.RLock()
			defer a.callMu.RUnlock()
			if !a.callMediaTicketIsCurrentLocked(ticket) {
				return errors.New("cached call topology changed before hangup")
			}
			response, commandErr := command("ATH", 5*time.Second)
			return remoteRescueHangupCommandError(response, commandErr)
		},
	)
	return err
}
