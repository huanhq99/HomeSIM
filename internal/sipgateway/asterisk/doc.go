// Package asterisk adapts a version-pinned Asterisk ARI and chan_websocket
// deployment to the transport-neutral sipgateway contract. The live adapter
// uses Asterisk's native RESTRequest/RESTResponse protocol on its one event
// WebSocket after bootstrap; it never reconnects or falls back to HTTP for live
// inspection, mutation, media preparation, or owned-resource cleanup. The
// separate restart RecoveryInspector remains HTTP GET-only.
//
// It is deliberately limited to an incoming-call slice. The package observes
// channels explicitly handed to one Stasis application, prepares one PCMU
// media channel and mixing bridge, answers that exact incoming channel, and
// ends an exact active channel. It does not dial, reject, send DTMF, expose an
// HTTP listener, or configure a cellular gateway or Asterisk.
//
// A successful software test of this package is not evidence that a cellular
// appliance, SIM, operator, or live bidirectional call has passed acceptance.
package asterisk
