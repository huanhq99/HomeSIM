# Asterisk 22 integration evidence

The image/configuration harness is
[`scripts/tests/asterisk22-integration.sh`](../../scripts/tests/asterisk22-integration.sh).
The real software-media harness is
[`scripts/tests/public-voice-e2e.sh`](../../scripts/tests/public-voice-e2e.sh).

`--static` is hermetic: it exercises dry-run/apply, private modes, source pins,
dual ARI roles, policy/channelvars, PJSIP/RTP bounds, required modules and the
post-`Stasis` orphan continuation. It also fault-injects unsafe configurations
and requires the verifier to reject them.

`--live` is deliberately opt-in and disposable. It fails, rather than skips,
when Docker or Compose is missing; builds the pinned image; renders the final
port mapping; starts only a synthetic loopback project; and checks version,
health, the physically pruned 35-dynamic/16-built-in module closure, fixed
container networking, endpoint/dialplan and fail-closed container policy. Its
temporary root is under the repository's ignored
`local/asterisk-integration-tests/` tree so Colima can bind it. It does not
emulate a cellular gateway, place or answer a call, prove ARI/media WebSockets,
or establish bidirectional audio. Those remain separate acceptance gates with
the exact appliance.

## 2026-08-15 local evidence

- Hermetic static contract: `21/21` passed.
- Disposable loopback real-Asterisk smoke: `16/16` passed.
- Final `--provenance=false` arm64 local image ID and RepoDigest:
  `sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51`.
- The container subnet/address are fixed at `172.31.255.248/29` and
  `172.31.255.250`; PJSIP `local_net` uses the container subnet and
  `external_signaling_port` is rendered from the generated SIP port.
- The resolved deployment uses `pull_policy: never` and `restart: "no"`.
- The live harness left no container or temporary generated root behind.

That first harness is local image/configuration evidence, not a published
registry digest or production PBX acceptance. Its loopback case sends no SIP
INVITE or RTP by design.

## Real software-media gate

`public-voice-e2e.sh --static` compiles and runs the hermetic external-voice-v2
contract through a Go overlay. `MACCELLULAR_PUBLIC_VOICE_E2E=1
public-voice-e2e.sh --live` starts the pinned Asterisk image and a disposable
Linux synthetic SIP/RTP endpoint on one private Docker network, then exercises:

- a real SIP `INVITE` into Asterisk and an external-voice-v2 incoming snapshot;
- external-voice-v2 `Offer`, WebRTC answer/start, `Answer`, and `End`;
- exact, distinct PCMU sample patterns from gateway to browser and browser to
  gateway over real RTP plus the Asterisk media WebSocket;
- SIP `BYE`/`200`, an ended v2 snapshot, and zero remaining ARI channels or
  bridges.

The 2026-08-15 live run passed against Asterisk `22.10.1` image
`sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51`.
It is a real software-media gate without cellular hardware; it does not prove
the exact appliance/SIM/operator/SKU/firmware, public mobile networking/TURN,
Android/iOS lifecycle, or intelligible speech on a physical phone. Those remain
external acceptance gates.
