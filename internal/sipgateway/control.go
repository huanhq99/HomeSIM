package sipgateway

import (
	"context"
	"strings"
)

// CallController is the optional provider control surface used by the public
// voice page for controls that are not part of the incoming-media coordinator
// itself. Implementations must keep provider handles private and return a
// bounded CommandResult with the exact dialog snapshot when possible.
//
// Dial is intentionally not part of this first control slice: a provider must
// first prove an explicit outbound trunk/dialplan contract before it can be
// exposed to a browser. RejectIncoming and SendDTMF operate only on a dialog
// already observed by the coordinator.
type CallController interface {
	RejectIncoming(context.Context, CallRef, string) (CommandResult, error)
	SendDTMF(context.Context, CallRef, string, string) (CommandResult, error)
}

// RejectIncomingRequest is the process-local binding for a one-shot reject.
// The public API never serializes the embedded CallRef.
type RejectIncomingRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	CommandID        string
}

// DTMFRequest binds one bounded DTMF command to the exact active call and
// media lease owned by the browser.
type DTMFRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	MediaLeaseID     string
	CommandID        string
	Digits           string
}

// RejectIncoming executes a provider reject for the current ringing dialog.
// It deliberately does not claim media ownership and therefore works before
// the browser opens its microphone.
func (c *Coordinator) RejectIncoming(ctx context.Context, request RejectIncomingRequest) (MutationResult, error) {
	if err := contextError(ctx); err != nil {
		return MutationResult{}, err
	}
	if !validPublicCallRef(request.Call) || request.ExpectedRevision == 0 ||
		!validCommandID(request.CommandID) {
		return MutationResult{}, ErrInvalidCommand
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.isClosed() {
		return MutationResult{}, ErrClosed
	}
	current, err := c.requireCurrent(request.Call, request.ExpectedRevision)
	if err != nil {
		return MutationResult{}, err
	}
	if hasUncertainMutation(current) || current.provider.State != ProviderCallIncoming {
		return MutationResult{}, ErrWrongCallPhase
	}
	controller, ok := c.adapter.(CallController)
	if !ok {
		return MutationResult{}, ErrCapabilityUnavailable
	}
	result, err := controller.RejectIncoming(ctx, current.provider.Ref, request.CommandID)
	if err != nil {
		return MutationResult{}, err
	}
	if result.CommandID != request.CommandID || result.Current == nil ||
		result.Current.Validate() != nil || !result.Current.Ref.SameDialog(current.provider.Ref) {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	if result.Outcome == CommandUnknown {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	if result.Outcome == CommandRejected {
		call, snapshotErr := c.Snapshot(request.Call)
		return MutationResult{CommandID: request.CommandID, Outcome: result.Outcome, Call: call}, snapshotErr
	}
	if result.Current.State != ProviderCallEnded || result.Current.Ref.Revision < current.provider.Ref.Revision {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	var closeSession MediaSession
	c.mu.Lock()
	if c.closed || c.call == nil || c.call.public != current.public ||
		!c.call.provider.Ref.SameDialog(current.provider.Ref) {
		c.mu.Unlock()
		return MutationResult{}, ErrClosed
	}
	c.call.provider = *result.Current
	c.call.phase = PhaseEnded
	closeSession = c.call.media
	c.call.media = nil
	c.call.publicMediaLeaseID = ""
	c.call.mediaActivationEpoch = 0
	c.mu.Unlock()
	closeMedia(closeSession)
	call, err := c.Snapshot(request.Call)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{CommandID: request.CommandID, Outcome: result.Outcome, Call: call}, nil
}

// SendDTMF sends a small, exact DTMF sequence to the currently active dialog.
// It requires the browser's current media lease so a stale page cannot drive a
// different call incarnation.
func (c *Coordinator) SendDTMF(ctx context.Context, request DTMFRequest) (MutationResult, error) {
	if err := contextError(ctx); err != nil {
		return MutationResult{}, err
	}
	if !validPublicCallRef(request.Call) || request.ExpectedRevision == 0 ||
		!validOpaque(request.MediaLeaseID, 1, 128) || !validCommandID(request.CommandID) ||
		!validDTMFDigits(request.Digits) {
		return MutationResult{}, ErrInvalidCommand
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.isClosed() {
		return MutationResult{}, ErrClosed
	}
	current, err := c.requireCurrent(request.Call, request.ExpectedRevision)
	if err != nil {
		return MutationResult{}, err
	}
	if hasUncertainMutation(current) || current.provider.State != ProviderCallActive ||
		current.media == nil || current.publicMediaLeaseID != request.MediaLeaseID ||
		!current.controllerOwned {
		return MutationResult{}, ErrWrongCallPhase
	}
	controller, ok := c.adapter.(CallController)
	if !ok {
		return MutationResult{}, ErrCapabilityUnavailable
	}
	result, err := controller.SendDTMF(ctx, current.provider.Ref, request.CommandID, request.Digits)
	if err != nil {
		return MutationResult{}, err
	}
	if result.CommandID != request.CommandID || result.Current == nil ||
		result.Current.Validate() != nil || !result.Current.Ref.SameDialog(current.provider.Ref) {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	if result.Outcome == CommandUnknown {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	if result.Current.State != ProviderCallActive || result.Current.Ref.Revision < current.provider.Ref.Revision {
		return MutationResult{}, ErrCommandOutcomeUnknown
	}
	call, err := c.Snapshot(request.Call)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{CommandID: request.CommandID, Outcome: result.Outcome, Call: call}, nil
}

func validDTMFDigits(value string) bool {
	if len(value) == 0 || len(value) > 32 || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' &&
			!(char >= 'A' && char <= 'D') && !(char >= 'a' && char <= 'd') {
			return false
		}
	}
	return true
}
