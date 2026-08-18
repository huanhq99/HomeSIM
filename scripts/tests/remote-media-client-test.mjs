import assert from "node:assert/strict";

const originalRandomUUID = globalThis.crypto.randomUUID;
Object.defineProperty(globalThis.crypto, "randomUUID", { value: () => "test-client-nonce", configurable: true });

const script = new URL("../../cmd/djonehub-macos/remote/media-client.js", import.meta.url);
await import(script.href);

class EventTargetStub {
  constructor() { this.listeners = new Map(); }
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name).add(listener);
  }
  removeEventListener(name, listener) { this.listeners.get(name)?.delete(listener); }
  emit(name, event = {}) { for (const listener of this.listeners.get(name) || []) listener(event); }
}

class PeerStub extends EventTargetStub {
  constructor() {
    super();
    this.iceGatheringState = "complete";
    this.connectionState = "new";
    this.transceivers = [];
    this.closed = false;
  }
  addTransceiver(track, options) { this.transceivers.push({ track, options }); }
  async createOffer() { return { type: "offer", sdp: "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\n" }; }
  async setLocalDescription(value) { this.localDescription = value; }
  async setRemoteDescription(value) {
    this.remoteDescription = value;
    this.emit("track", { track: { kind: "audio" }, streams: [{ id: "remote" }] });
    this.connectionState = "connected";
    this.emit("connectionstatechange");
  }
  close() { this.closed = true; this.connectionState = "closed"; }
}

function fixture() {
  const track = { kind: "audio", enabled: true, stopped: false, stop() { this.stopped = true; } };
  const stream = { getAudioTracks: () => [track], getTracks: () => [track] };
  const peer = new PeerStub();
  const audio = { srcObject: null, played: 0, paused: 0, async play() { this.played += 1; }, pause() { this.paused += 1; } };
  const client = new globalThis.MacCellularRemoteMediaClient({
    peerFactory: () => peer,
    mediaDevices: { async getUserMedia() { return stream; } },
    audioElement: audio,
    timeoutMilliseconds: 100,
  });
  return { client, peer, stream, track, audio };
}

const call = {
  call_id: "call-test",
  call_generation: 7,
  call_index: 1,
  call_direction: "incoming",
};

{
  let microphoneRequested = false;
  const { client } = fixture();
  client.mediaDevices = {
    async getUserMedia() {
      microphoneRequested = true;
      throw new Error("must not be reached");
    },
  };
  await assert.rejects(
    client.prepare({ ...call, call_direction: "outgoing" }, async () => ({})),
    /exact call identity/,
  );
  assert.equal(microphoneRequested, false);
}

{
  const { client, peer, stream, track, audio } = fixture();
  const snapshot = await client.prepare(call, async (offer) => {
    assert.equal(offer.call.call_generation, 7);
    assert.equal(offer.purpose, "incoming");
    assert.equal(offer.client_nonce, "test-client-nonce");
    assert.match(offer.sdp_offer, /m=audio/);
    return { sdp_answer: "v=0\r\n", media_session_id: "media-test", lease_generation: 19, call_generation: 7 };
  });
  assert.equal(snapshot.phase, "prepared");
  assert.equal(snapshot.lease_generation, 19);
  assert.equal(peer.transceivers.length, 1);
  assert.equal(peer.transceivers[0].options.direction, "sendrecv");
  assert.deepEqual(peer.transceivers[0].options.streams, [stream]);
  client.setMuted(true);
  assert.equal(track.enabled, false);
  client.close();
  assert.equal(track.stopped, true);
  assert.equal(peer.closed, true);
  assert.equal(audio.srcObject, null);
  assert.equal(client.snapshot().phase, "idle");
}

{
  const { client, peer, track } = fixture();
  await assert.rejects(
    client.prepare(call, async () => ({
      sdp_answer: "v=0\r\n",
      media_session_id: "wrong-generation",
      lease_generation: 20,
      call_generation: 8,
    })),
    /mismatched media answer/,
  );
  assert.equal(peer.closed, true);
  assert.equal(track.stopped, true);
  assert.equal(client.snapshot().phase, "idle");
}

{
  let releasePlay;
  const { client, peer, track, audio } = fixture();
  audio.play = async function play() {
    this.played += 1;
    await new Promise((resolve) => { releasePlay = resolve; });
  };
  const preparing = client.prepare(call, async () => ({
    sdp_answer: "v=0\r\n",
    media_session_id: "cancel-test",
    lease_generation: 21,
    call_generation: 7,
  }));
  while (!releasePlay) await new Promise((resolve) => setImmediate(resolve));
  client.close("page-hidden");
  releasePlay();
  await assert.rejects(preparing, /canceled/);
  assert.equal(peer.closed, true);
  assert.equal(track.stopped, true);
  assert.equal(client.snapshot().phase, "idle");
  assert.equal(client.snapshot().media_session_id, "");
}

console.log("remote media client tests passed");
Object.defineProperty(globalThis.crypto, "randomUUID", { value: originalRandomUUID, configurable: true });
