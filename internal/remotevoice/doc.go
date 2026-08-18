// Package remotevoice implements the network-facing, PCMU WebRTC media core.
//
// It deliberately has no modem, USB, QDC507, CoreAudio, or Swift integration.
// A caller owns a FramePort and is responsible for moving fixed-size PCM16
// frames between that port and its separately implemented device audio path.
// The Unix-socket broker is transport-only until a verified media lease owner
// binds it to call control; constructing it does not prove a live UAC route or
// authorize any modem or remote-API operation. The macOS host supplies separate,
// generation-bound UAC callback attestations.
package remotevoice
