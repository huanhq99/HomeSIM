package sipgateway

import "errors"

// MaxMutationRecoveryIDBytes bounds a journal-private recovery record key.
const MaxMutationRecoveryIDBytes = 256

var (
	// ErrMutationJournalRequired prevents mutation policy from becoming live
	// without a durable reservation boundary.
	ErrMutationJournalRequired = errors.New("sipgateway: durable mutation journal required")
	// ErrInvalidMutationJournalRecord rejects partial or malformed durable
	// mutation records before an adapter can execute them.
	ErrInvalidMutationJournalRecord = errors.New("sipgateway: invalid mutation journal record")
)

// MutationEvidence binds a durable mutation to the authenticated owner and
// immutable public request without persisting either original value.
type MutationEvidence struct {
	OwnerDigest   [32]byte
	RequestDigest [32]byte
}

func (MutationEvidence) String() string   { return "sipgateway.MutationEvidence{redacted}" }
func (MutationEvidence) GoString() string { return "sipgateway.MutationEvidence{redacted}" }

// Validate rejects the zero value so callers cannot silently omit either
// half of the durable ownership/request binding.
func (e MutationEvidence) Validate() error {
	if e.OwnerDigest == ([32]byte{}) || e.RequestDigest == ([32]byte{}) {
		return ErrInvalidMutationJournalRecord
	}
	return nil
}

// PendingMutation is the complete durable reservation written before a
// provider mutation begins. ProviderToken is adapter-private and must remain
// redacted outside the journal implementation.
type PendingMutation struct {
	Kind               CommandKind
	CommandID          string
	Call               PublicCallRef
	ExpectedRevision   uint64
	PublicMediaLeaseID string
	ProviderToken      RecoveryToken
	Evidence           MutationEvidence
}

func (PendingMutation) String() string   { return "sipgateway.PendingMutation{redacted}" }
func (PendingMutation) GoString() string { return "sipgateway.PendingMutation{redacted}" }

// Validate enforces the exact public intent shape before it crosses the
// durable journal boundary.
func (p PendingMutation) Validate() error {
	if !validCommandID(p.CommandID) || !validPublicCallRef(p.Call) ||
		p.ExpectedRevision == 0 || p.Evidence.Validate() != nil {
		return ErrInvalidMutationJournalRecord
	}
	if _, err := p.ProviderToken.MarshalBinary(); err != nil {
		return ErrInvalidMutationJournalRecord
	}
	switch p.Kind {
	case CommandAnswerIncoming, CommandEndActive:
		if !validOpaque(p.PublicMediaLeaseID, 1, 128) {
			return ErrInvalidMutationJournalRecord
		}
	default:
		return ErrInvalidMutationJournalRecord
	}
	return nil
}

// MutationResolution is durable evidence that an armed mutation no longer
// needs recovery. Unknown is terminal only when an explicit reconciliation
// has observed an active or ended provider state.
type MutationResolution struct {
	Outcome CommandOutcome
	State   ProviderCallState
}

func (MutationResolution) String() string   { return "sipgateway.MutationResolution{redacted}" }
func (MutationResolution) GoString() string { return "sipgateway.MutationResolution{redacted}" }

// Validate accepts only outcome/state pairs that can make a pending provider
// mutation safe to retire.
func (r MutationResolution) Validate() error {
	if err := (RecoverySnapshot{State: r.State}).Validate(); err != nil {
		return ErrInvalidMutationJournalRecord
	}
	switch r.Outcome {
	case CommandApplied:
		if r.State != ProviderCallActive && r.State != ProviderCallEnded {
			return ErrInvalidMutationJournalRecord
		}
	case CommandRejected:
		if r.State != ProviderCallIncoming && r.State != ProviderCallActive {
			return ErrInvalidMutationJournalRecord
		}
	case CommandUnknown:
		if r.State != ProviderCallActive && r.State != ProviderCallEnded {
			return ErrInvalidMutationJournalRecord
		}
	default:
		return ErrInvalidMutationJournalRecord
	}
	return nil
}

// MutationJournal is a synchronous durable boundary. Arm must not return a
// valid, bounded recovery ID until the complete pending record is durable.
// Resolve must not return nil until the resolution is durable. Implementations
// must preserve an armed record when Resolve returns an error.
type MutationJournal interface {
	Arm(PendingMutation) (recoveryID string, err error)
	Resolve(recoveryID string, resolution MutationResolution) error
}
