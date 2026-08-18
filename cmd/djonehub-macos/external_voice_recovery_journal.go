package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	externalVoiceRecoveryAdapterAsterisk = "asterisk"
	externalVoiceRecoveryIDBytes         = 32

	externalVoiceRecoveryOwnerKeyDomain   = "external-voice-recovery-owner-key-v1\x00"
	externalVoiceRecoveryRequestKeyDomain = "external-voice-recovery-request-key-v1\x00"
	externalVoiceRecoveryCommandKeyDomain = "external-voice-recovery-command-key-v1\x00"
	externalVoiceRecoveryOwnerDomain      = "external-voice-recovery-owner-v1\x00"
	externalVoiceRecoveryRequestDomain    = "external-voice-recovery-request-v1\x00"
	externalVoiceRecoveryCommandDomain    = "external-voice-recovery-command-v1\x00"
)

// externalVoiceMutationJournal converts the provider-neutral coordinator
// journal contract into the private, fsynced single-slot recovery store. It
// keeps only domain-separated HMAC keys in memory; the store owns the root.
type externalVoiceMutationJournal struct {
	mu sync.Mutex

	store      *externalVoiceRecoveryStore
	gatewayID  string
	idReader   io.Reader
	ownerKey   [sha256.Size]byte
	requestKey [sha256.Size]byte
	commandKey [sha256.Size]byte
	closed     bool
}

func (*externalVoiceMutationJournal) String() string {
	return "externalVoiceMutationJournal{redacted}"
}

func (*externalVoiceMutationJournal) GoString() string {
	return "externalVoiceMutationJournal{redacted}"
}

func newExternalVoiceMutationJournal(
	store *externalVoiceRecoveryStore,
	gatewayID string,
	idReader io.Reader,
) (*externalVoiceMutationJournal, error) {
	gatewayID = strings.TrimSpace(gatewayID)
	if store == nil || gatewayID == "" || gatewayID != strings.TrimSpace(gatewayID) {
		return nil, errExternalVoiceInvalid
	}
	if idReader == nil {
		idReader = rand.Reader
	}
	root, err := store.ProviderSecret()
	if err != nil {
		return nil, errExternalVoiceUnavailable
	}
	journal := &externalVoiceMutationJournal{
		store: store, gatewayID: gatewayID, idReader: idReader,
	}
	journal.ownerKey = deriveExternalVoiceRecoveryKey(root, externalVoiceRecoveryOwnerKeyDomain)
	journal.requestKey = deriveExternalVoiceRecoveryKey(root, externalVoiceRecoveryRequestKeyDomain)
	journal.commandKey = deriveExternalVoiceRecoveryKey(root, externalVoiceRecoveryCommandKeyDomain)
	zeroExternalVoiceRecoverySecret(&root)
	return journal, nil
}

// Evidence binds the stable authenticated owner and exact raw request digest
// without persisting either login or request body. Kind is included so equal
// JSON bytes on distinct mutation endpoints cannot share evidence.
func (j *externalVoiceMutationJournal) Evidence(
	kind sipgateway.CommandKind,
	identity string,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationEvidence, error) {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if j == nil || identity == "" || bodyHash == ([sha256.Size]byte{}) ||
		(kind != sipgateway.CommandAnswerIncoming && kind != sipgateway.CommandEndActive) {
		return sipgateway.MutationEvidence{}, errExternalVoiceInvalid
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.store == nil {
		return sipgateway.MutationEvidence{}, errExternalVoiceClosed
	}
	owner := hmac.New(sha256.New, j.ownerKey[:])
	_, _ = owner.Write([]byte(externalVoiceRecoveryOwnerDomain))
	_, _ = owner.Write([]byte(identity))
	request := hmac.New(sha256.New, j.requestKey[:])
	_, _ = request.Write([]byte(externalVoiceRecoveryRequestDomain))
	_, _ = request.Write([]byte(kind))
	_, _ = request.Write([]byte{0})
	_, _ = request.Write(bodyHash[:])
	var evidence sipgateway.MutationEvidence
	copy(evidence.OwnerDigest[:], owner.Sum(nil))
	copy(evidence.RequestDigest[:], request.Sum(nil))
	return evidence, evidence.Validate()
}

func (j *externalVoiceMutationJournal) Arm(pending sipgateway.PendingMutation) (string, error) {
	if j == nil || pending.Validate() != nil {
		return "", sipgateway.ErrInvalidMutationJournalRecord
	}
	providerToken, err := pending.ProviderToken.MarshalBinary()
	if err != nil {
		return "", sipgateway.ErrInvalidMutationJournalRecord
	}
	defer zeroExternalVoiceRecoveryBytes(providerToken)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.store == nil {
		return "", errExternalVoiceClosed
	}
	recoveryID, err := j.newRecoveryIDLocked()
	if err != nil {
		return "", errExternalVoiceUnavailable
	}
	kind, err := externalVoiceRecoveryKindFromCommand(pending.Kind)
	if err != nil {
		return "", err
	}
	commandMAC := hmac.New(sha256.New, j.commandKey[:])
	_, _ = commandMAC.Write([]byte(externalVoiceRecoveryCommandDomain))
	_, _ = commandMAC.Write([]byte(pending.Kind))
	_, _ = commandMAC.Write([]byte{0})
	_, _ = commandMAC.Write([]byte(pending.CommandID))
	snapshot, err := j.store.Arm(externalVoiceRecoveryArmRequest{
		RecoveryID:       recoveryID,
		Kind:             kind,
		OwnerHash:        hex.EncodeToString(pending.Evidence.OwnerDigest[:]),
		CommandKeyHash:   hex.EncodeToString(commandMAC.Sum(nil)),
		RequestHash:      hex.EncodeToString(pending.Evidence.RequestDigest[:]),
		PublicCallID:     pending.Call.PublicCallID,
		Generation:       pending.Call.Generation,
		ExpectedRevision: pending.ExpectedRevision,
		PublicMediaLease: pending.PublicMediaLeaseID,
		Adapter:          externalVoiceRecoveryAdapterAsterisk,
		GatewayID:        j.gatewayID,
		ProviderToken:    providerToken,
	})
	if err != nil {
		return "", errors.Join(sipgateway.ErrInvalidMutationJournalRecord, err)
	}
	defer zeroExternalVoiceRecoverySnapshot(&snapshot)
	if snapshot.State != externalVoiceRecoveryArmed || snapshot.RecoveryID != recoveryID {
		return "", sipgateway.ErrInvalidMutationJournalRecord
	}
	return recoveryID, nil
}

func (j *externalVoiceMutationJournal) Resolve(
	recoveryID string,
	resolution sipgateway.MutationResolution,
) error {
	if j == nil || resolution.Validate() != nil {
		return sipgateway.ErrInvalidMutationJournalRecord
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.store == nil {
		return errExternalVoiceClosed
	}
	current, err := j.store.Snapshot()
	if err != nil {
		return err
	}
	defer zeroExternalVoiceRecoverySnapshot(&current)
	if current.State != externalVoiceRecoveryArmed || current.RecoveryID != recoveryID {
		return errors.Join(sipgateway.ErrInvalidMutationJournalRecord, errExternalVoiceRecoveryStoreStale)
	}
	storeResolution, err := externalVoiceRecoveryResolutionFromMutation(current.Kind, resolution)
	if err != nil {
		return err
	}
	resolved, err := j.store.Resolve(recoveryID, storeResolution)
	if err != nil {
		return err
	}
	if resolved.State != externalVoiceRecoveryResolved || resolved.Resolution != storeResolution {
		return sipgateway.ErrInvalidMutationJournalRecord
	}
	return nil
}

func (j *externalVoiceMutationJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	j.store = nil
	zeroExternalVoiceRecoverySecret(&j.ownerKey)
	zeroExternalVoiceRecoverySecret(&j.requestKey)
	zeroExternalVoiceRecoverySecret(&j.commandKey)
	return nil
}

func (j *externalVoiceMutationJournal) newRecoveryIDLocked() (string, error) {
	buffer := make([]byte, externalVoiceRecoveryIDBytes)
	defer zeroExternalVoiceRecoveryBytes(buffer)
	if _, err := io.ReadFull(j.idReader, buffer); err != nil {
		return "", err
	}
	return "svr_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func deriveExternalVoiceRecoveryKey(
	root [externalVoiceRecoveryRootSecretBytes]byte,
	domain string,
) [sha256.Size]byte {
	mac := hmac.New(sha256.New, root[:])
	_, _ = mac.Write([]byte(domain))
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func externalVoiceRecoveryKindFromCommand(kind sipgateway.CommandKind) (externalVoiceRecoveryKind, error) {
	switch kind {
	case sipgateway.CommandAnswerIncoming:
		return externalVoiceRecoveryAnswerIncoming, nil
	case sipgateway.CommandEndActive:
		return externalVoiceRecoveryEndActive, nil
	default:
		return "", sipgateway.ErrInvalidMutationJournalRecord
	}
}

func externalVoiceRecoveryResolutionFromMutation(
	kind externalVoiceRecoveryKind,
	resolution sipgateway.MutationResolution,
) (externalVoiceRecoveryResolution, error) {
	switch kind {
	case externalVoiceRecoveryAnswerIncoming:
		switch resolution.Outcome {
		case sipgateway.CommandApplied:
			if resolution.State == sipgateway.ProviderCallActive {
				return externalVoiceRecoveryApplied, nil
			}
		case sipgateway.CommandRejected:
			if resolution.State == sipgateway.ProviderCallIncoming {
				return externalVoiceRecoveryRejected, nil
			}
		case sipgateway.CommandUnknown:
			if resolution.State == sipgateway.ProviderCallActive {
				return externalVoiceRecoveryApplied, nil
			}
			if resolution.State == sipgateway.ProviderCallEnded {
				return externalVoiceRecoveryProviderEnded, nil
			}
		}
	case externalVoiceRecoveryEndActive:
		switch resolution.Outcome {
		case sipgateway.CommandApplied:
			if resolution.State == sipgateway.ProviderCallEnded {
				return externalVoiceRecoveryApplied, nil
			}
		case sipgateway.CommandRejected:
			if resolution.State == sipgateway.ProviderCallActive {
				return externalVoiceRecoveryRejected, nil
			}
		case sipgateway.CommandUnknown:
			if resolution.State == sipgateway.ProviderCallEnded {
				return externalVoiceRecoveryProviderEnded, nil
			}
		}
	}
	return "", sipgateway.ErrInvalidMutationJournalRecord
}

func zeroExternalVoiceRecoveryBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ sipgateway.MutationJournal = (*externalVoiceMutationJournal)(nil)
