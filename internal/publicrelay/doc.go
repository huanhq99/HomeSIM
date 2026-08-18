// Package publicrelay defines the cryptographic and replay-safety core for the
// version 2 opaque public relay. It is deliberately independent from the
// status-only publicedge version 1 package and contains no HTTP, WebSocket,
// modem, storage, or deployment integration.
//
// The outer envelope exposes only opaque routing/session metadata, bounded
// timing metadata, padded ciphertext, and a typed P-256 proof. SMS actions,
// operation identifiers, destinations, message bodies, cursors, and results
// exist only inside the authenticated ciphertext.
package publicrelay
