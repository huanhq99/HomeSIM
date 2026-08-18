"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const {
  MacCellularRemoteMediaClient,
  validateRelayCredentials,
  relayOnlySDP,
} = require("./media-client.js");

function credentials(expiresAt = new Date(Date.now() + 5 * 60_000).toISOString()) {
  return {
    version: 1,
    urls: [
      "turn:8.8.8.8:3478?transport=udp",
      "turn:8.8.8.8:3478?transport=tcp",
      "turns:turn.example.com:443?transport=tcp",
    ],
    username: "synthetic-user",
    credential: "synthetic-secret",
    expires_at: expiresAt,
  };
}

class FakeEventTarget {
  constructor() { this.listeners = new Map(); }
  addEventListener(name, callback) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name).add(callback);
  }
  removeEventListener(name, callback) { this.listeners.get(name)?.delete(callback); }
  dispatch(name, event = {}) {
    for (const callback of [...(this.listeners.get(name) || [])]) callback(event);
  }
}

class FakeTrack {
  constructor() { this.kind = "audio"; this.enabled = true; this.stopped = 0; }
  stop() { this.stopped += 1; }
}

class FakeStream {
  constructor(track = new FakeTrack()) { this.track = track; }
  getAudioTracks() { return [this.track]; }
  getTracks() { return [this.track]; }
}

class FakePeer extends FakeEventTarget {
  constructor(configuration) {
    super();
    this.configuration = configuration;
    this.connectionState = "new";
    this.iceGatheringState = "complete";
    this.localDescription = null;
    this.closed = 0;
  }
  addTransceiver() {}
  async createOffer() { return { type: "offer", sdp: "v=0\r\na=candidate:1 1 udp 1 100.64.0.2 55000 typ host\r\n" }; }
  async setLocalDescription(description) { this.localDescription = description; }
  async setRemoteDescription() {
    this.connectionState = "connected";
    setTimeout(() => {
      this.dispatch("track", { track: new FakeTrack(), streams: [new FakeStream()] });
    }, 0);
  }
  close() { this.closed += 1; this.connectionState = "closed"; }
}

test("Cloudflare direct media fetches ephemeral TURN and is relay-only", async () => {
  const requests = [];
  const client = new MacCellularRemoteMediaClient({
    transport: () => "cloudflare-access",
    fetchImpl: async (path, options) => {
      requests.push({ path, options });
      return { ok: true, json: async () => credentials() };
    },
  });
  const config = await client.peerConfiguration();
  assert.equal(config.iceTransportPolicy, "relay");
  assert.equal(config.iceServers.length, 1);
  assert.deepEqual([...config.iceServers[0].urls], ["turns:turn.example.com:443?transport=tcp"]);
  assert.equal(config.iceServers[0].credentialType, "password");
  assert.deepEqual(requests, [{
    path: "/api/remote/v2/voice/media/ice-credentials",
    options: {
      method: "GET", credentials: "same-origin", cache: "no-store",
      headers: { Accept: "application/json" },
    },
  }]);
  assert.equal(Object.isFrozen(config.iceServers[0]), true);
  assert.equal(Object.isFrozen(config.iceServers[0].urls), true);
});

test("legacy Tailscale direct media retains host-only ICE configuration", async () => {
  let fetched = false;
  const client = new MacCellularRemoteMediaClient({
    transport: "tailscale-serve",
    fetchImpl: async () => { fetched = true; },
  });
  const config = await client.peerConfiguration();
  assert.deepEqual(config.iceServers, []);
  assert.equal("iceTransportPolicy" in config, false);
  assert.equal(fetched, false);
});

test("outgoing validation reports only fixed diagnostic stages", async () => {
  const client = new MacCellularRemoteMediaClient();
  await assert.rejects(client.prepareOutgoing(0, "10086", () => {}), /invalid call generation/);
  await assert.rejects(client.prepareOutgoing(1, "not-a-number", () => {}), /invalid dial number/);
  await assert.rejects(client.prepareOutgoing(1, "10086", null), /offer exchange unavailable/);
});

test("audio levels reflect real analyser samples and reset on close", () => {
  const analyser = (sample) => ({
    fftSize: 8,
    getByteTimeDomainData(target) { target.fill(sample); },
    disconnect() {},
  });
  const client = new MacCellularRemoteMediaClient();
  client.localAnalyser = analyser(153);
  client.remoteAnalyser = analyser(128);
  const levels = client.audioLevels();
  assert.ok(levels.local > .9);
  assert.equal(levels.remote, 0);
  client.close();
  assert.deepEqual(client.audioLevels(), { local: 0, remote: 0 });
});

test("a stalled microphone request is bounded and a late stream is stopped", async () => {
  let resolveCapture;
  const lateTrack = new FakeTrack();
  const client = new MacCellularRemoteMediaClient({
    transport: "tailscale-serve",
    mediaDevices: {
      getUserMedia() {
        return new Promise((resolve) => { resolveCapture = resolve; });
      },
    },
    timeoutMilliseconds: 5,
  });

  await assert.rejects(
    client.prepareOutgoing(7, "10086", async () => assert.fail("offer must not be exchanged")),
    /microphone request timed out/,
  );
  assert.equal(client.snapshot().phase, "idle");
  resolveCapture(new FakeStream(lateTrack));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(lateTrack.stopped, 1);
});

test("public relay contract and SDP reject fallback candidates", () => {
  assert.throws(() => validateRelayCredentials({ ...credentials(), urls: credentials().urls.slice(0, 2) }));
  assert.throws(() => validateRelayCredentials({ ...credentials(), expires_at: new Date(Date.now() - 1).toISOString() }));
  assert.throws(() => validateRelayCredentials({
    ...credentials(),
    urls: [
      "turn:turn.example.com:3478?transport=udp",
      "turn:other.example.com:3478?transport=tcp",
      "turns:turn.example.com:443?transport=tcp",
    ],
  }));
  for (const address of ["10.0.0.1", "100.64.0.1", "127.0.0.1", "192.0.2.1", "198.19.153.67"]) {
    assert.throws(() => validateRelayCredentials({
      ...credentials(),
      urls: [
        `turn:${address}:3478?transport=udp`,
        `turn:${address}:3478?transport=tcp`,
        "turns:turn.example.com:443?transport=tcp",
      ],
    }));
  }
  assert.equal(relayOnlySDP("v=0\r\na=candidate:1 1 udp 1 203.0.113.5 55000 typ relay\r\n"), true);
  assert.equal(relayOnlySDP("v=0\r\na=candidate:1 1 udp 1 192.0.2.2 55000 typ host\r\n"), false);
  assert.equal(relayOnlySDP("v=0\r\n"), false);
});

test("public mobile media validates the full fallback bundle but only offers TLS 443 to WebRTC", () => {
  const relay = validateRelayCredentials(credentials());
  assert.deepEqual([...relay.urls], ["turns:turn.example.com:443?transport=tcp"]);
  assert.equal(relay.urls.some((url) => url.includes(":3478")), false);
});

test("outgoing direct media binds the idle generation and dial number before ATD", async () => {
  const localTrack = new FakeTrack();
  const audio = { srcObject: null, plays: 0, muted: true, volume: 0,
    async play() { this.plays += 1; }, pause() {} };
  let exchanged;
  const client = new MacCellularRemoteMediaClient({
    transport: "tailscale-serve",
    mediaDevices: { async getUserMedia() { return new FakeStream(localTrack); } },
    peerFactory: (configuration) => new FakePeer(configuration),
    audioElement: audio,
    mediaStreamFactory: (track) => new FakeStream(track),
    timeoutMilliseconds: 1_000,
  });

  const snapshot = await client.prepareOutgoing(11, "+86 10086", async (offer) => {
    exchanged = offer;
    return {
      media_session_id: "media-session-1",
      lease_generation: 7,
      call_generation: 11,
      sdp_answer: "v=0\r\na=candidate:2 1 udp 1 100.64.0.3 55001 typ host\r\n",
    };
  });

  assert.equal(exchanged.purpose, "outgoing");
  assert.equal(exchanged.expected_call_generation, 11);
  assert.equal(exchanged.number, "+86 10086");
  assert.equal("call" in exchanged, false);
  assert.equal(snapshot.phase, "prepared");
  assert.equal(snapshot.purpose, "outgoing");
  assert.equal(snapshot.call_generation, 11);
  assert.equal(snapshot.media_session_id, "media-session-1");
  assert.equal(audio.plays, 1);
  assert.equal(audio.muted, false);
  assert.equal(audio.volume, 1);
  assert.notEqual(audio.srcObject, null);
  assert.equal(audio.srcObject.getAudioTracks().length, 1);
  audio.paused = true;
  assert.equal(await client.ensurePlayback(), true);
  assert.equal(audio.plays, 2);
  audio.paused = false;
  assert.equal(await client.ensurePlayback(), true);
  assert.equal(audio.plays, 2);

  client.close();
  assert.equal(localTrack.stopped, 1);
  assert.equal(client.snapshot().phase, "idle");
});

test("outgoing media mixes the real microphone with a zero-valued clock source", async () => {
  const microphoneTrack = new FakeTrack();
  const mixedTrack = new FakeTrack();
  const connections = [];
  const silence = {
    offset: { value: -1 }, starts: 0, stops: 0,
    connect(destination) { connections.push(["silence", destination]); },
    start() { this.starts += 1; }, stop() { this.stops += 1; },
  };
  const destination = { stream: new FakeStream(mixedTrack) };
  const speakerDestination = {};
  let sourceCount = 0;
  const context = {
    state: "suspended", closes: 0, destination: speakerDestination,
    createMediaStreamSource(stream) {
      sourceCount += 1;
      const name = sourceCount === 1 ? "microphone" : "remote-playback";
      if (sourceCount === 1) assert.equal(stream.getAudioTracks()[0], microphoneTrack);
      return {
        connect(target) { connections.push([name, target]); },
        disconnect() { connections.push([`${name}-disconnect`]); },
      };
    },
    createMediaStreamDestination() { return destination; },
    createConstantSource() { return silence; },
    async resume() { this.state = "running"; },
    async close() { this.closes += 1; },
  };
  let transceiverTrack;
  class ClockedPeer extends FakePeer {
    addTransceiver(track) { transceiverTrack = track; }
  }
  const client = new MacCellularRemoteMediaClient({
    transport: "tailscale-serve",
    mediaDevices: { async getUserMedia() { return new FakeStream(microphoneTrack); } },
    audioContextFactory: () => context,
    mediaStreamFactory: (track) => new FakeStream(track),
    peerFactory: (configuration) => new ClockedPeer(configuration),
    timeoutMilliseconds: 1_000,
  });
  await client.prepareOutgoing(21, "10086", async () => ({
    media_session_id: "media-session-clocked",
    lease_generation: 9,
    call_generation: 21,
    sdp_answer: "v=0\r\na=candidate:2 1 udp 1 100.64.0.3 55001 typ host\r\n",
  }));
  assert.equal(transceiverTrack, mixedTrack);
  assert.equal(silence.offset.value, 0);
  assert.equal(silence.starts, 1);
  assert.deepEqual(connections.slice(0, 3), [
    ["microphone", destination], ["silence", destination], ["remote-playback", speakerDestination],
  ]);
  client.close();
  assert.equal(microphoneTrack.stopped, 1);
  assert.equal(mixedTrack.stopped, 1);
  assert.equal(silence.stops, 1);
  assert.deepEqual(connections.at(-1), ["remote-playback-disconnect"]);
  assert.equal(context.closes, 1);
});

test("ICE candidate errors survive the bounded gathering timeout without leaking URLs", async () => {
  class FailingGatherPeer extends FakePeer {
    constructor(configuration) {
      super(configuration);
      this.iceGatheringState = "gathering";
    }
    async setLocalDescription(description) {
      this.localDescription = description;
      this.dispatch("icecandidateerror", {
        errorCode: 701,
        url: "turn:8.8.8.8:3478?transport=tcp",
        errorText: "marker-that-must-not-be-copied",
      });
    }
  }

  const client = new MacCellularRemoteMediaClient({
    transport: "cloudflare-access",
    fetchImpl: async () => ({ ok: true, json: async () => credentials() }),
    mediaDevices: { async getUserMedia() { return new FakeStream(); } },
    peerFactory: (configuration) => new FailingGatherPeer(configuration),
    timeoutMilliseconds: 5,
  });
  await assert.rejects(
    client.prepareOutgoing(12, "10086", async () => assert.fail("offer must not be exchanged")),
    (error) => error.message.includes("relay candidate failed (ICE 701)") &&
      !error.message.includes("8.8.8.8") && !error.message.includes("marker-that-must-not-be-copied"),
  );
  assert.equal(client.snapshot().phase, "idle");
});

test("public media continues on the first relay candidate without waiting for blackholed UDP", async () => {
  class TCPRelayPeer extends FakePeer {
    constructor(configuration) {
      super(configuration);
      this.iceGatheringState = "gathering";
    }
    async setLocalDescription(description) {
      this.localDescription = description;
      setTimeout(() => {
        this.localDescription = {
          type: "offer",
          sdp: "v=0\r\na=candidate:7 1 tcp 1 8.8.8.8 49160 typ relay tcptype passive\r\n",
        };
        this.dispatch("icecandidate", {
          candidate: { candidate: "candidate:7 1 tcp 1 8.8.8.8 49160 typ relay tcptype passive" },
        });
      }, 0);
    }
  }

  const client = new MacCellularRemoteMediaClient({
    transport: "cloudflare-access",
    fetchImpl: async () => ({ ok: true, json: async () => credentials() }),
    mediaDevices: { async getUserMedia() { return new FakeStream(); } },
    peerFactory: (configuration) => new TCPRelayPeer(configuration),
    timeoutMilliseconds: 100,
  });
  const snapshot = await client.prepareOutgoing(13, "10086", async (offer) => {
    assert.match(offer.sdp_offer, /typ relay/);
    return {
      media_session_id: "media-session-tcp",
      lease_generation: 8,
      call_generation: 13,
      sdp_answer: "v=0\r\na=candidate:8 1 tcp 1 8.8.8.8 49161 typ relay tcptype passive\r\n",
    };
  });
  assert.equal(snapshot.phase, "prepared");
  assert.equal(client.peer.iceGatheringState, "gathering");
  client.close();
});

test("a browser non-relay candidate preserves its actionable failure reason", async () => {
  class NonRelayPeer extends FakePeer {
    constructor(configuration) {
      super(configuration);
      this.iceGatheringState = "gathering";
    }
    async setLocalDescription(description) {
      this.localDescription = description;
      setTimeout(() => this.dispatch("icecandidate", {
        candidate: { candidate: "candidate:9 1 udp 1 192.168.1.2 55000 typ host" },
      }), 0);
    }
  }
  const client = new MacCellularRemoteMediaClient({
    transport: "cloudflare-access",
    fetchImpl: async () => ({ ok: true, json: async () => credentials() }),
    mediaDevices: { async getUserMedia() { return new FakeStream(); } },
    peerFactory: (configuration) => new NonRelayPeer(configuration),
    timeoutMilliseconds: 100,
  });
  await assert.rejects(
    client.prepareOutgoing(14, "10086", async () => assert.fail("offer must not be exchanged")),
    /browser produced a non-relay media candidate/,
  );
});
