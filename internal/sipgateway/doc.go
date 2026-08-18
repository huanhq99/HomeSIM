// Package sipgateway defines the call-control and media boundary for an
// external cellular-to-SIP gateway.
//
// The package contains no network transport or device-specific implementation.
// A concrete adapter translates its signaling and media protocol into the
// small, revision-bound contract defined here. Coordinator keeps public call
// identities separate from provider handles and fails closed around ambiguous
// command outcomes.
package sipgateway
