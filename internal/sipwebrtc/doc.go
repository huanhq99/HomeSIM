// Package sipwebrtc bridges one already-prepared external SIP gateway media
// session to the repository's restricted PCMU WebRTC endpoint.
//
// It contains no SIP signaling, provider credentials, listener, or call-control
// authority. Call mutations remain owned by sipgateway.Coordinator; this bridge
// only makes its media session's Prepared and post-activation freshness proof
// depend on both the gateway and browser transports.
package sipwebrtc
