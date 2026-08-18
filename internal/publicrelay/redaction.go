package publicrelay

import "encoding/json"

type redactedValue struct {
	Type     string `json:"type"`
	Redacted bool   `json:"redacted"`
}

func marshalRedacted(kind string) ([]byte, error) {
	return json.Marshal(redactedValue{Type: kind, Redacted: true})
}

func redactedString(kind string) string { return "publicrelay." + kind + "{redacted}" }

func (EnvelopeMeta) MarshalJSON() ([]byte, error) { return marshalRedacted("EnvelopeMeta") }
func (EnvelopeMeta) String() string               { return redactedString("EnvelopeMeta") }
func (meta EnvelopeMeta) GoString() string        { return meta.String() }

func (Envelope) MarshalJSON() ([]byte, error) { return marshalRedacted("Envelope") }
func (Envelope) String() string               { return redactedString("Envelope") }
func (envelope Envelope) GoString() string    { return envelope.String() }

func (RootKey) MarshalJSON() ([]byte, error) { return marshalRedacted("RootKey") }
func (RootKey) String() string               { return redactedString("RootKey") }
func (root RootKey) GoString() string        { return root.String() }

func (KeySet) MarshalJSON() ([]byte, error) { return marshalRedacted("KeySet") }
func (KeySet) String() string               { return redactedString("KeySet") }
func (keys KeySet) GoString() string        { return keys.String() }

func (Commitment) MarshalJSON() ([]byte, error) { return marshalRedacted("Commitment") }
func (Commitment) String() string               { return redactedString("Commitment") }
func (commitment Commitment) GoString() string  { return commitment.String() }

func (SMSSendIntent) MarshalJSON() ([]byte, error) { return marshalRedacted("SMSSendIntent") }
func (SMSSendIntent) String() string               { return redactedString("SMSSendIntent") }
func (intent SMSSendIntent) GoString() string      { return intent.String() }

func (SMSInspectAndFenceRequest) MarshalJSON() ([]byte, error) {
	return marshalRedacted("SMSInspectAndFenceRequest")
}
func (SMSInspectAndFenceRequest) String() string {
	return redactedString("SMSInspectAndFenceRequest")
}
func (request SMSInspectAndFenceRequest) GoString() string { return request.String() }

func (SMSInspectAndFenceResult) MarshalJSON() ([]byte, error) {
	return marshalRedacted("SMSInspectAndFenceResult")
}
func (SMSInspectAndFenceResult) String() string {
	return redactedString("SMSInspectAndFenceResult")
}
func (result SMSInspectAndFenceResult) GoString() string { return result.String() }

func (Message) MarshalJSON() ([]byte, error) { return marshalRedacted("Message") }
func (Message) String() string               { return redactedString("Message") }
func (message Message) GoString() string     { return message.String() }

func (ReceiverState) MarshalJSON() ([]byte, error) { return marshalRedacted("ReceiverState") }
func (ReceiverState) String() string               { return redactedString("ReceiverState") }
func (state ReceiverState) GoString() string       { return state.String() }

func (ReceiverStateCommit) MarshalJSON() ([]byte, error) {
	return marshalRedacted("ReceiverStateCommit")
}
func (ReceiverStateCommit) String() string          { return redactedString("ReceiverStateCommit") }
func (commit ReceiverStateCommit) GoString() string { return commit.String() }

func (VerifiedMessage) MarshalJSON() ([]byte, error) { return marshalRedacted("VerifiedMessage") }
func (VerifiedMessage) String() string               { return redactedString("VerifiedMessage") }
func (verified VerifiedMessage) GoString() string    { return verified.String() }

func (VerifiedGatewayMessage) MarshalJSON() ([]byte, error) {
	return marshalRedacted("VerifiedGatewayMessage")
}
func (VerifiedGatewayMessage) String() string {
	return redactedString("VerifiedGatewayMessage")
}
func (verified VerifiedGatewayMessage) GoString() string { return verified.String() }

func (FencedAbsentExpectation) MarshalJSON() ([]byte, error) {
	return marshalRedacted("FencedAbsentExpectation")
}
func (FencedAbsentExpectation) String() string {
	return redactedString("FencedAbsentExpectation")
}
func (expectation FencedAbsentExpectation) GoString() string { return expectation.String() }

func (*Receiver) MarshalJSON() ([]byte, error) { return marshalRedacted("Receiver") }
func (*Receiver) String() string               { return redactedString("Receiver") }
func (receiver *Receiver) GoString() string    { return receiver.String() }
