package publicedge

import "encoding/json"

const redactedValue = "[redacted]"

type redactedFrame struct {
	Version     uint64    `json:"version"`
	Type        FrameType `json:"type"`
	GatewayID   string    `json:"gateway_id"`
	DeviceID    string    `json:"device_id,omitempty"`
	Principal   string    `json:"principal,omitempty"`
	Action      string    `json:"action"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	BodySHA256  string    `json:"body_sha256"`
	OperationID string    `json:"operation_id,omitempty"`
	ExpiresAt   int64     `json:"expires_at"`
	BootID      string    `json:"boot_id"`
	LeaseEpoch  uint64    `json:"lease_epoch"`
	Sequence    uint64    `json:"sequence"`
	Payload     string    `json:"payload"`
	Signature   string    `json:"signature,omitempty"`
}

func (frame Frame) redacted() redactedFrame {
	return redactedFrame{
		Version: frame.Version, Type: frame.Type,
		GatewayID: redactIfSet(frame.GatewayID), DeviceID: redactIfSet(frame.DeviceID),
		Principal: redactIfSet(frame.Principal), Action: frame.Action, Method: frame.Method,
		Path: frame.Path, BodySHA256: redactIfSet(frame.BodySHA256),
		OperationID: redactIfSet(frame.OperationID), ExpiresAt: frame.ExpiresAt,
		BootID: redactIfSet(frame.BootID), LeaseEpoch: frame.LeaseEpoch, Sequence: frame.Sequence,
		Payload: redactIfSet(string(frame.Payload)), Signature: redactIfSet(frame.Signature),
	}
}

// MarshalJSON is safe for structured logs. It never emits identity values,
// operation IDs, body hashes, payloads, challenges, or signatures.
func (frame Frame) MarshalJSON() ([]byte, error) { return json.Marshal(frame.redacted()) }

func (frame Frame) String() string {
	data, err := json.Marshal(frame.redacted())
	if err != nil {
		return "publicedge.Frame{redacted}"
	}
	return string(data)
}

func (frame Frame) GoString() string { return frame.String() }

type redactedFrameMeta struct {
	GatewayID   string `json:"gateway_id"`
	DeviceID    string `json:"device_id,omitempty"`
	Principal   string `json:"principal,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
	BootID      string `json:"boot_id"`
	LeaseEpoch  uint64 `json:"lease_epoch"`
	Sequence    uint64 `json:"sequence"`
}

func (meta FrameMeta) redacted() redactedFrameMeta {
	return redactedFrameMeta{
		GatewayID: redactIfSet(meta.GatewayID), DeviceID: redactIfSet(meta.DeviceID),
		Principal: redactIfSet(meta.Principal), OperationID: redactIfSet(meta.OperationID),
		ExpiresAt: meta.ExpiresAt, BootID: redactIfSet(meta.BootID),
		LeaseEpoch: meta.LeaseEpoch, Sequence: meta.Sequence,
	}
}

func (meta FrameMeta) MarshalJSON() ([]byte, error) { return json.Marshal(meta.redacted()) }

func (meta FrameMeta) String() string {
	data, err := json.Marshal(meta.redacted())
	if err != nil {
		return "publicedge.FrameMeta{redacted}"
	}
	return string(data)
}

func (meta FrameMeta) GoString() string { return meta.String() }

type redactedLease struct {
	GatewayID      string `json:"gateway_id"`
	BootID         string `json:"boot_id"`
	LeaseEpoch     uint64 `json:"lease_epoch"`
	ChannelBinding string `json:"channel_binding"`
}

func (lease Lease) redacted() redactedLease {
	return redactedLease{
		GatewayID: redactIfSet(lease.GatewayID), BootID: redactIfSet(lease.BootID),
		LeaseEpoch: lease.LeaseEpoch, ChannelBinding: redactIfSet(lease.ChannelBinding),
	}
}

func (lease Lease) MarshalJSON() ([]byte, error) { return json.Marshal(lease.redacted()) }

func (lease Lease) String() string {
	data, err := json.Marshal(lease.redacted())
	if err != nil {
		return "publicedge.Lease{redacted}"
	}
	return string(data)
}

func (lease Lease) GoString() string { return lease.String() }

func redactIfSet(value string) string {
	if value == "" {
		return ""
	}
	return redactedValue
}

type redactedRegistryConfig struct {
	GatewayKeyCount   int         `json:"gateway_key_count"`
	DeviceKeyCount    int         `json:"device_key_count"`
	PrincipalKeyCount int         `json:"principal_key_count"`
	ChallengeTTL      interface{} `json:"challenge_ttl"`
	MessageTTL        interface{} `json:"message_ttl"`
}

func (config RegistryConfig) redacted() redactedRegistryConfig {
	return redactedRegistryConfig{
		GatewayKeyCount: len(config.GatewayKeys), DeviceKeyCount: len(config.DeviceKeys),
		PrincipalKeyCount: len(config.PrincipalKeys), ChallengeTTL: config.ChallengeTTL,
		MessageTTL: config.MessageTTL,
	}
}

func (config RegistryConfig) MarshalJSON() ([]byte, error) { return json.Marshal(config.redacted()) }

func (config RegistryConfig) String() string {
	data, err := json.Marshal(config.redacted())
	if err != nil {
		return "publicedge.RegistryConfig{redacted}"
	}
	return string(data)
}

func (config RegistryConfig) GoString() string { return config.String() }

type redactedRegistry struct {
	GatewayCount   int `json:"gateway_count"`
	DeviceCount    int `json:"device_count"`
	PrincipalCount int `json:"principal_count"`
}

func (registry *Registry) redacted() redactedRegistry {
	if registry == nil {
		return redactedRegistry{}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return redactedRegistry{
		GatewayCount: len(registry.gateways), DeviceCount: len(registry.deviceKeys),
		PrincipalCount: len(registry.principalKeys),
	}
}

func (registry *Registry) MarshalJSON() ([]byte, error) { return json.Marshal(registry.redacted()) }

func (registry *Registry) String() string {
	data, err := json.Marshal(registry.redacted())
	if err != nil {
		return "publicedge.Registry{redacted}"
	}
	return string(data)
}

func (registry *Registry) GoString() string { return registry.String() }
