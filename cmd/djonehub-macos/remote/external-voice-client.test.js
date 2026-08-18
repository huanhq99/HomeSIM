"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const {
  MacCellularExternalVoiceClient,
  API,
  pcmuOnlySDP,
  validatePCMUOnlySDP,
} = require("./external-voice-client.js");

const publicCallID = "A".repeat(43);
const mediaLeaseID = `pml_${"B".repeat(43)}`;
const csrf = "synthetic-csrf-token";
const idempotencyKey = "voice-answer-command-0001";

function browserOfferSDP(payloads = "111 0 126") {
  return [
    "v=0",
    "o=- 1 2 IN IP4 127.0.0.1",
    "s=-",
    "t=0 0",
    `m=audio 9 UDP/TLS/RTP/SAVPF ${payloads}`,
    "c=IN IP4 0.0.0.0",
    "a=ice-ufrag:syntheticUfrag",
    "a=ice-pwd:syntheticPasswordValue",
    "a=fingerprint:sha-256 00:11:22:33",
    "a=setup:actpass",
    "a=mid:0",
    "a=sendrecv",
    "a=rtcp-mux",
    "a=rtpmap:111 opus/48000/2",
    "a=fmtp:111 minptime=10;useinbandfec=1",
    "a=rtcp-fb:111 transport-cc",
    "a=rtpmap:0 PCMU/8000",
    "a=rtpmap:126 telephone-event/8000",
    "a=fmtp:126 0-16",
    "a=candidate:1 1 udp 1 100.64.0.1 41000 typ host",
    "a=end-of-candidates",
    "",
  ].join("\r\n");
}

function pcmuAnswerSDP() {
  return [
    "v=0",
    "o=- 3 4 IN IP4 127.0.0.1",
    "s=-",
    "t=0 0",
    "m=audio 9 UDP/TLS/RTP/SAVPF 0",
    "c=IN IP4 0.0.0.0",
    "a=ice-ufrag:answerUfrag",
    "a=ice-pwd:answerPasswordValue",
    "a=fingerprint:sha-256 44:55:66:77",
    "a=setup:active",
    "a=mid:0",
    "a=sendrecv",
    "a=rtcp-mux",
    "a=rtpmap:0 PCMU/8000",
    "a=candidate:2 1 udp 1 100.64.0.2 41001 typ host",
    "a=end-of-candidates",
    "",
  ].join("\r\n");
}

function relayBrowserOfferSDP() {
  return browserOfferSDP().replace(
    "a=candidate:1 1 udp 1 100.64.0.1 41000 typ host",
    "a=candidate:1 1 udp 1 203.0.113.7 41000 typ relay raddr 0.0.0.0 rport 0",
  );
}

function incomingCall(revision = 7) {
  return {
    call: { public_call_id: publicCallID, generation: 3 },
    revision,
    phase: "incoming_ringing",
    controller_owned: false,
    reconcile_required: false,
  };
}

function mediaSnapshot(overrides = {}) {
  return {
    lease_id: mediaLeaseID,
    codec: { name: "PCMU", clock_rate: 8000, channels: 1 },
    prepared: true,
    activated: false,
    gateway_to_client_fresh: false,
    client_to_gateway_fresh: false,
    bidirectional_fresh: false,
    gateway_to_client_frames: 0,
    client_to_gateway_frames: 0,
    gateway_to_client_baseline: 0,
    client_to_gateway_baseline: 0,
    activation_failed: false,
    ...overrides,
  };
}

function preparedCall(overrides = {}) {
  return {
    ...incomingCall(),
    phase: "media_ready",
    media: mediaSnapshot(),
    ...overrides,
  };
}

function preparingCall(overrides = {}) {
  return {
    ...incomingCall(),
    phase: "media_preparing",
    media: mediaSnapshot({ prepared: false }),
    ...overrides,
  };
}

function activeCall(overrides = {}) {
  return {
    ...preparedCall({ revision: 8 }),
    phase: "active_unverified",
    controller_owned: true,
    media: mediaSnapshot({ activated: true, activation_epoch: 1 }),
    ...overrides,
  };
}

function jsonResponse(status, value) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async text() { return JSON.stringify(value); },
  };
}

class FakeEventTarget {
  constructor() {
    this.listeners = new Map();
  }

  addEventListener(name, callback) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name).add(callback);
  }

  removeEventListener(name, callback) {
    this.listeners.get(name)?.delete(callback);
  }

  dispatch(name, event = {}) {
    for (const callback of [...(this.listeners.get(name) || [])]) callback(event);
  }
}

class FakeTrack {
  constructor(kind = "audio") {
    this.kind = kind;
    this.enabled = true;
    this.stopped = 0;
  }

  stop() {
    this.stopped += 1;
  }
}

class FakeStream {
  constructor(audioTracks = [new FakeTrack()], videoTracks = []) {
    this.audioTracks = audioTracks;
    this.videoTracks = videoTracks;
  }

  getAudioTracks() { return this.audioTracks; }
  getVideoTracks() { return this.videoTracks; }
  getTracks() { return [...this.audioTracks, ...this.videoTracks]; }
}

class FakePeer extends FakeEventTarget {
  constructor(configuration, offerSDP = browserOfferSDP()) {
    super();
    this.configuration = configuration;
    this.offerSDP = offerSDP;
    this.transceivers = [];
    this.iceGatheringState = "complete";
    this.connectionState = "new";
    this.localDescription = null;
    this.remoteDescription = null;
    this.closed = 0;
  }

  addTransceiver(track, options) {
    const transceiver = { track, options };
    this.transceivers.push(transceiver);
    return transceiver;
  }

  getTransceivers() { return this.transceivers; }

  async createOffer() {
    return { type: "offer", sdp: this.offerSDP };
  }

  async setLocalDescription(description) {
    this.localDescription = description;
  }

  async setRemoteDescription(description) {
    this.remoteDescription = description;
    this.connectionState = "connected";
    this.dispatch("track", { track: new FakeTrack("audio"), streams: [new FakeStream()] });
    this.dispatch("connectionstatechange");
  }

  close() {
    this.closed += 1;
    this.connectionState = "closed";
    this.dispatch("connectionstatechange");
  }
}

function makeAudioElement() {
  return {
    srcObject: null,
    plays: 0,
    pauses: 0,
    async play() { this.plays += 1; },
    pause() { this.pauses += 1; },
  };
}

function makeHarness(fetchImpl, options = {}) {
  const localTrack = new FakeTrack("audio");
  const stream = new FakeStream([localTrack]);
  const peers = [];
  const pageTarget = new FakeEventTarget();
  const audioElement = makeAudioElement();
  const client = new MacCellularExternalVoiceClient({
    fetchImpl: async (url, requestOptions) => {
      if (url === API.session) {
        return jsonResponse(200, {
          transport: options.transport || "tailscale-serve",
          sms_only: false,
          external_voice: true,
          external_voice_api_version: 2,
        });
      }
      return fetchImpl(url, requestOptions);
    },
    csrfToken: () => csrf,
    randomUUID: () => "synthetic_nonce_00000001",
    mediaDevices: { async getUserMedia() { return stream; } },
    peerFactory(configuration) {
      const offerSDP = options.offerSDP ||
        (options.transport === "cloudflare-access" ? relayBrowserOfferSDP() : browserOfferSDP());
      const peer = new FakePeer(configuration, offerSDP);
      peers.push(peer);
      return peer;
    },
    mediaStreamFactory: (track) => new FakeStream([track]),
    audioElement,
    pageTarget,
    timeoutMilliseconds: 100,
  });
  return { client, peers, pageTarget, audioElement, localTrack, stream };
}

async function prepareHarness(onFetch) {
  const requests = [];
  const harness = makeHarness(async (url, options) => {
    requests.push({ url, options });
    if (url === API.offer) {
      onFetch?.(url, options);
      return jsonResponse(200, {
        version: 2,
        call: preparedCall(),
        answer_sdp: pcmuAnswerSDP(),
      });
    }
    throw new Error(`unexpected request ${url}`);
  });
  await harness.client.prepare(incomingCall());
  return { ...harness, requests };
}

test("v2 media offer is same-origin, PCMU-only, and uses one audio transceiver", async () => {
  const seen = [];
  const harness = makeHarness(async (url, options) => {
    seen.push({ url, options });
    if (url === API.offer) {
      assert.equal(options.method, "POST");
      assert.equal(options.credentials, "same-origin");
      assert.equal(options.cache, "no-store");
      assert.equal(options.headers["Content-Type"], "application/json");
      assert.equal(options.headers["X-MacCellular-CSRF"], csrf);
      assert.equal(options.headers["Idempotency-Key"], undefined);
      const body = JSON.parse(options.body);
      assert.deepEqual(body.call, { public_call_id: publicCallID, generation: 3 });
      assert.equal(body.version, 2);
      assert.equal(body.expected_revision, 7);
      assert.equal(body.client_nonce, "synthetic_nonce_00000001");
      assert.match(body.sdp_offer, /m=audio 9 UDP\/TLS\/RTP\/SAVPF 0\r\n/);
      assert.doesNotMatch(body.sdp_offer, /opus|telephone-event|m=video|m=application/i);
      return jsonResponse(200, {
        version: 2,
        call: preparedCall(),
        answer_sdp: pcmuAnswerSDP(),
      });
    }
    if (url === API.answer) {
      assert.equal(options.headers["Idempotency-Key"], idempotencyKey);
      return jsonResponse(200, { ok: true, code: "completed" });
    }
    if (url === API.snapshot) {
      assert.equal(options.method, "GET");
      assert.equal(options.headers["X-MacCellular-CSRF"], undefined);
      return jsonResponse(200, {
        version: 2,
        voice: {
          enabled: true,
          health: "connected",
          call: activeCall(),
          owned_by_requester: true,
          answer_enabled: true,
          end_enabled: true,
          recovery_required: false,
        },
      });
    }
    if (url === API.end) return jsonResponse(200, { ok: true, code: "completed" });
    throw new Error(`unexpected request ${url}`);
  });

  const prepared = await harness.client.prepare(incomingCall());
  assert.equal(prepared.phase, "prepared");
  assert.equal(prepared.media_lease_id, mediaLeaseID);
  assert.deepEqual(harness.peers[0].configuration.iceServers, []);
  assert.equal(harness.peers[0].transceivers.length, 1);
  assert.equal(harness.peers[0].transceivers[0].options.direction, "sendrecv");
  assert.deepEqual(harness.peers[0].transceivers[0].options.streams, [harness.stream]);
  assert.equal(harness.audioElement.plays, 1);

  const answer = harness.client.createPersistentOperation("answer", idempotencyKey);
  assert.ok(Object.isFrozen(answer));
  assert.equal(answer.path, API.answer);
  assert.deepEqual(JSON.parse(answer.body), {
    version: 2,
    call: { public_call_id: publicCallID, generation: 3 },
    expected_revision: 7,
    media_lease_id: mediaLeaseID,
  });
  await harness.client.submitPersistent(answer);
  assert.equal(harness.client.snapshot().phase, "awaiting_snapshot");
  await harness.client.refresh();
  assert.equal(harness.client.snapshot().phase, "active");

  const end = harness.client.createPersistentOperation("end", "voice-end-command-0001");
  await harness.client.submitPersistent(end);
  assert.equal(harness.client.snapshot().phase, "idle");
  assert.equal(harness.peers[0].closed, 1);
  assert.equal(harness.localTrack.stopped, 1);
  assert.deepEqual(seen.map((item) => item.url), [API.offer, API.answer, API.snapshot, API.end]);
});

test("Cloudflare external voice fetches short-lived TURN credentials and creates a relay-only peer", async () => {
  const expiresAt = new Date(Date.now() + 5 * 60_000).toISOString();
  const seen = [];
  const harness = makeHarness(async (url, options) => {
    seen.push({ url, options });
    if (url === API.iceCredentials) {
      assert.equal(options.method, "GET");
      assert.equal(options.credentials, "same-origin");
      assert.equal(options.headers["X-MacCellular-CSRF"], undefined);
      return jsonResponse(200, {
        version: 1,
        urls: [
          "turn:turn.example.com:3478?transport=udp",
          "turn:turn.example.com:3478?transport=tcp",
          "turns:turn.example.com:443?transport=tcp",
        ],
        username: "synthetic-turn-user",
        credential: "synthetic-turn-password",
        expires_at: expiresAt,
      });
    }
    if (url === API.offer) {
      return jsonResponse(200, {
        version: 2,
        call: preparedCall(),
        answer_sdp: pcmuAnswerSDP(),
      });
    }
    throw new Error(`unexpected request ${url}`);
  }, { transport: "cloudflare-access" });

  await harness.client.prepare(incomingCall());
  assert.deepEqual(seen.map((item) => item.url), [API.iceCredentials, API.offer]);
  assert.deepEqual(harness.peers[0].configuration, {
    iceServers: [{
      urls: [
        "turn:turn.example.com:3478?transport=udp",
        "turn:turn.example.com:3478?transport=tcp",
        "turns:turn.example.com:443?transport=tcp",
      ],
      username: "synthetic-turn-user",
      credential: "synthetic-turn-password",
      credentialType: "password",
    }],
    iceTransportPolicy: "relay",
    bundlePolicy: "max-bundle",
    rtcpMuxPolicy: "require",
  });
  harness.client.dispose();
});

test("Cloudflare external voice fails closed before peer creation on invalid TURN credentials", async () => {
  let offers = 0;
  const harness = makeHarness(async (url) => {
    if (url === API.iceCredentials) {
      return jsonResponse(200, {
        version: 1,
        urls: ["stun:turn.example.com:3478"],
        username: "synthetic-turn-user",
        credential: "synthetic-turn-password",
        expires_at: new Date(Date.now() + 5 * 60_000).toISOString(),
      });
    }
    offers += 1;
    throw new Error(`unexpected request ${url}`);
  }, { transport: "cloudflare-access" });

  await assert.rejects(
    harness.client.prepare(incomingCall()),
    (error) => error?.code === "invalid_contract",
  );
  assert.equal(offers, 0);
  assert.equal(harness.peers.length, 0);
  assert.equal(harness.localTrack.stopped, 1);
});

test("Cloudflare external voice rejects any non-relay candidate before sending the SDP offer", async () => {
  let offers = 0;
  const harness = makeHarness(async (url) => {
    if (url === API.iceCredentials) {
      return jsonResponse(200, {
        version: 1,
        urls: [
          "turn:turn.example.com:3478?transport=udp",
          "turn:turn.example.com:3478?transport=tcp",
          "turns:turn.example.com:443?transport=tcp",
        ],
        username: "synthetic-turn-user",
        credential: "synthetic-turn-password",
        expires_at: new Date(Date.now() + 5 * 60_000).toISOString(),
      });
    }
    offers += 1;
    throw new Error(`unexpected request ${url}`);
  }, { transport: "cloudflare-access", offerSDP: browserOfferSDP() });

  await assert.rejects(
    harness.client.prepare(incomingCall()),
    (error) => error?.code === "invalid_contract" && /relay-only/.test(error.message),
  );
  assert.equal(offers, 0);
  assert.equal(harness.peers.length, 1);
  assert.equal(harness.peers[0].closed, 1);
  assert.equal(harness.localTrack.stopped, 1);
});

test("media_preparing answer is accepted but mutation stays locked until authoritative media_ready", async () => {
  const harness = makeHarness(async (url) => {
    if (url === API.offer) {
      return jsonResponse(200, {
        version: 2,
        call: preparingCall(),
        answer_sdp: pcmuAnswerSDP(),
      });
    }
    if (url === API.snapshot) {
      return jsonResponse(200, {
        version: 2,
        voice: {
          enabled: true,
          health: "connected",
          call: preparedCall(),
          owned_by_requester: true,
          answer_enabled: true,
          end_enabled: true,
          recovery_required: false,
        },
      });
    }
    throw new Error(`unexpected request ${url}`);
  });

  const local = await harness.client.prepare(incomingCall());
  assert.equal(local.phase, "prepared");
  assert.throws(
    () => harness.client.createPersistentOperation("answer", idempotencyKey),
    (error) => error?.code === "conflict",
  );
  await harness.client.refresh();
  const operation = harness.client.createPersistentOperation("answer", idempotencyKey);
  assert.equal(operation.path, API.answer);
});

test("unknown_outcome is never retried and only explicit ephemeral reconcile can inspect it", async () => {
  let answerRequests = 0;
  let reconcileRequests = 0;
  const harness = makeHarness(async (url, options) => {
    if (url === API.offer) {
      return jsonResponse(200, { version: 2, call: preparedCall(), answer_sdp: pcmuAnswerSDP() });
    }
    if (url === API.answer) {
      answerRequests += 1;
      return jsonResponse(409, { ok: false, code: "unknown_outcome" });
    }
    if (url === API.reconcile) {
      reconcileRequests += 1;
      assert.equal(options.headers["Idempotency-Key"], undefined);
      assert.equal(options.body, operation.body);
      return jsonResponse(200, {
        version: 2,
        call: reconcileRequests === 1
          ? preparedCall({ phase: "reconciling", reconcile_required: true })
          : activeCall({ reconcile_required: false }),
      });
    }
    throw new Error(`unexpected request ${url}`);
  });
  await harness.client.prepare(incomingCall());
  const operation = harness.client.createPersistentOperation("answer", idempotencyKey);
  await assert.rejects(
    harness.client.submitPersistent(operation),
    (error) => error.code === "unknown_outcome" && error.httpStatus === 409,
  );
  assert.equal(answerRequests, 1);
  assert.equal(harness.client.snapshot().phase, "outcome_unknown");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(answerRequests, 1);
  await assert.rejects(
    harness.client.submitPersistent(operation),
    (error) => error.code === "conflict",
  );
  assert.equal(answerRequests, 1);

  await harness.client.reconcile();
  assert.equal(reconcileRequests, 1);
  assert.equal(harness.client.snapshot().phase, "outcome_unknown");
  await harness.client.reconcile();
  assert.equal(reconcileRequests, 2);
  assert.equal(answerRequests, 1);
  assert.equal(harness.client.snapshot().phase, "active");
});

test("manual_recovery_required is preserved and closes any live browser media", async () => {
  let snapshotRequests = 0;
  const harness = makeHarness(async (url) => {
    if (url === API.offer) {
      return jsonResponse(200, { version: 2, call: preparedCall(), answer_sdp: pcmuAnswerSDP() });
    }
    if (url === API.snapshot) {
      snapshotRequests += 1;
      return jsonResponse(200, {
        version: 2,
        voice: {
          enabled: true,
          health: "manual_recovery_required",
          call: null,
          owned_by_requester: false,
          answer_enabled: true,
          end_enabled: true,
          recovery_required: true,
        },
      });
    }
    throw new Error(`unexpected request ${url}`);
  });
  await harness.client.prepare(incomingCall());
  const response = await harness.client.refresh();
  assert.equal(snapshotRequests, 1);
  assert.equal(response.voice.health, "manual_recovery_required");
  assert.equal(response.voice.recovery_required, true);
  assert.equal(harness.client.snapshot().phase, "idle");
  assert.equal(harness.peers[0].closed, 1);
  assert.equal(harness.localTrack.stopped, 1);

  const invalidClient = makeHarness(async () => jsonResponse(200, {
    version: 2,
    voice: {
      enabled: false,
      health: "disabled",
      call: null,
      owned_by_requester: false,
      answer_enabled: false,
      end_enabled: false,
    },
  })).client;
  await assert.rejects(invalidClient.refresh(), (error) => error.code === "invalid_contract");
  invalidClient.dispose();
});

test("persistent operations accept only the same frozen in-memory object", async () => {
  const harness = await prepareHarness();
  const operation = harness.client.createPersistentOperation("answer", idempotencyKey);
  const clone = Object.freeze({ ...operation });
  await assert.rejects(
    harness.client.submitPersistent(clone),
    (error) => error.code === "conflict",
  );
  assert.equal(harness.requests.length, 1);
  assert.throws(() => {
    operation.body = "{}";
  }, TypeError);
});

test("pagehide closes synchronously and late microphone resolution cannot create a peer", async () => {
  let resolveMicrophone;
  const stream = new FakeStream([new FakeTrack("audio")]);
  const pageTarget = new FakeEventTarget();
  let peerCreations = 0;
  let fetches = 0;
  const client = new MacCellularExternalVoiceClient({
    fetchImpl: async () => { fetches += 1; throw new Error("must not fetch"); },
    csrfToken: csrf,
    randomUUID: () => "synthetic_nonce_00000001",
    pageTarget,
    mediaDevices: {
      getUserMedia() {
        return new Promise((resolve) => { resolveMicrophone = resolve; });
      },
    },
    peerFactory() {
      peerCreations += 1;
      return new FakePeer({ iceServers: [] });
    },
  });
  const preparing = client.prepare(incomingCall());
  assert.equal(client.snapshot().phase, "requesting_microphone");
  pageTarget.dispatch("pagehide");
  assert.equal(client.snapshot().phase, "idle");
  resolveMicrophone(stream);
  await assert.rejects(preparing, (error) => error.code === "canceled");
  assert.equal(stream.getAudioTracks()[0].stopped, 1);
  assert.equal(peerCreations, 0);
  assert.equal(fetches, 0);
  client.dispose();
  assert.equal(pageTarget.listeners.get("pagehide")?.size || 0, 0);
});

test("close aborts an in-flight offer and the generation barrier drops its late response", async () => {
  let offerStarted;
  const offerReached = new Promise((resolve) => { offerStarted = resolve; });
  let resolveOffer;
  const harness = makeHarness((url, options) => {
    assert.equal(url, API.offer);
    offerStarted();
    return new Promise((resolve, reject) => {
      resolveOffer = resolve;
      options.signal.addEventListener("abort", () => {
        const error = new Error("aborted");
        error.name = "AbortError";
        reject(error);
      }, { once: true });
    });
  });
  const preparing = harness.client.prepare(incomingCall());
  await offerReached;
  harness.client.close("test-close");
  resolveOffer(jsonResponse(200, {
    version: 2,
    call: preparedCall(),
    answer_sdp: pcmuAnswerSDP(),
  }));
  await assert.rejects(preparing);
  assert.equal(harness.client.snapshot().phase, "idle");
  assert.equal(harness.client.snapshot().media_lease_id, "");
  assert.equal(harness.peers[0].closed, 1);
  assert.equal(harness.localTrack.stopped, 1);
});

test("SDP contract rejects video, data, and non-PCMU answers", () => {
  const restricted = pcmuOnlySDP(browserOfferSDP());
  assert.match(restricted, /m=audio 9 UDP\/TLS\/RTP\/SAVPF 0/);
  assert.doesNotMatch(restricted, /a=rtpmap:111|a=rtpmap:126/);
  assert.throws(
    () => pcmuOnlySDP(`${browserOfferSDP()}m=video 9 UDP/TLS/RTP/SAVPF 96\r\n`),
    /exactly one audio media section/,
  );
  assert.throws(
    () => validatePCMUOnlySDP(browserOfferSDP("111")),
    /does not support WebRTC PCMU/,
  );
});

test("client source contains no browser persistence API and does not import the legacy v1 client", () => {
  const source = fs.readFileSync(path.join(__dirname, "external-voice-client.js"), "utf8");
  assert.doesNotMatch(source, /localStorage|sessionStorage|indexedDB|\.caches\b/);
  assert.doesNotMatch(source, /remote\/media-client|\/api\/remote\/v1\/(?:media|calls)\/|MacCellularRemoteMediaClient|QPCMV|\bATA\b|\bATH\b/);
  assert.match(source, /\/api\/remote\/v2\/voice\/media\/offers/);
  assert.match(source, /iceTransportPolicy:\s*"relay"/);
  assert.match(source, /defaultTimeoutMilliseconds = 28_000/);
});
