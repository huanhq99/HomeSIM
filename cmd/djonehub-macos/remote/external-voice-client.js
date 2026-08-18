(function installExternalVoiceClient(root) {
  "use strict";

  const api = Object.freeze({
    session: "/api/remote/v1/session",
    snapshot: "/api/remote/v2/voice/snapshot",
    iceCredentials: "/api/remote/v2/voice/media/ice-credentials",
    offer: "/api/remote/v2/voice/media/offers",
    answer: "/api/remote/v2/voice/calls/answer",
    end: "/api/remote/v2/voice/calls/end",
    reconcile: "/api/remote/v2/voice/calls/reconcile",
  });
  const schemaVersion = 2;
  const defaultTimeoutMilliseconds = 28_000;
  const publicCallIDPattern = /^[A-Za-z0-9_-]{43}$/;
  const mediaLeaseIDPattern = /^pml_[A-Za-z0-9_-]{43}$/;
  const noncePattern = /^[A-Za-z0-9_-]{16,128}$/;
  const idempotencyKeyPattern = /^[A-Za-z0-9._:-]{8,128}$/;
  const responseCodePattern = /^[a-z][a-z0-9_]{0,47}$/;
  const callPhases = new Set([
    "incoming_ringing",
    "media_preparing",
    "media_ready",
    "answer_pending",
    "reconciling",
    "active_unverified",
    "active_transport_verified",
    "active_unmanaged",
    "ending",
    "ended",
  ]);
  const activeCallPhases = new Set([
    "active_unverified",
    "active_transport_verified",
  ]);
  const localTransitions = Object.freeze({
    idle: new Set(["requesting_microphone"]),
    requesting_microphone: new Set(["creating_offer", "idle"]),
    creating_offer: new Set(["gathering_ice", "idle"]),
    gathering_ice: new Set(["exchanging_offer", "idle"]),
    exchanging_offer: new Set(["connecting", "idle"]),
    connecting: new Set(["prepared", "idle"]),
    prepared: new Set(["command_pending", "active", "outcome_unknown", "idle"]),
    active: new Set(["command_pending", "outcome_unknown", "idle"]),
    command_pending: new Set(["prepared", "active", "awaiting_snapshot", "outcome_unknown", "idle"]),
    awaiting_snapshot: new Set(["prepared", "active", "outcome_unknown", "idle"]),
    outcome_unknown: new Set(["reconciling", "prepared", "active", "idle"]),
    reconciling: new Set(["outcome_unknown", "prepared", "active", "idle"]),
  });

  class ExternalVoiceError extends Error {
    constructor(code, message, httpStatus = 0) {
      super(message);
      this.name = "ExternalVoiceError";
      this.code = code;
      this.httpStatus = httpStatus;
    }
  }

  function invalid(message) {
    return new ExternalVoiceError("invalid_contract", message);
  }

  function plainObject(value) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return false;
    const prototype = Object.getPrototypeOf(value);
    return prototype === Object.prototype || prototype === null;
  }

  function positiveSafeInteger(value) {
    return Number.isSafeInteger(value) && value > 0;
  }

  function nonnegativeSafeInteger(value) {
    return Number.isSafeInteger(value) && value >= 0;
  }

  function exactBoolean(value) {
    return value === true || value === false;
  }

  function validateCallRef(value) {
    if (!plainObject(value) || !publicCallIDPattern.test(value.public_call_id) ||
        !positiveSafeInteger(value.generation)) {
      throw invalid("gateway returned an invalid public call reference");
    }
    return Object.freeze({
      public_call_id: value.public_call_id,
      generation: value.generation,
    });
  }

  function sameCall(left, right) {
    return Boolean(left && right && left.public_call_id === right.public_call_id &&
      left.generation === right.generation);
  }

  function validatePCMU(value) {
    if (!plainObject(value) || value.name !== "PCMU" || value.clock_rate !== 8000 ||
        value.channels !== 1) {
      throw invalid("gateway did not select mono PCMU at 8 kHz");
    }
    return Object.freeze({ name: "PCMU", clock_rate: 8000, channels: 1 });
  }

  function validateMediaSnapshot(value) {
    if (!plainObject(value) || !mediaLeaseIDPattern.test(value.lease_id) ||
        !exactBoolean(value.prepared) || !exactBoolean(value.activated) ||
        !exactBoolean(value.gateway_to_client_fresh) ||
        !exactBoolean(value.client_to_gateway_fresh) ||
        !exactBoolean(value.bidirectional_fresh) ||
        !nonnegativeSafeInteger(value.gateway_to_client_frames) ||
        !nonnegativeSafeInteger(value.client_to_gateway_frames) ||
        !nonnegativeSafeInteger(value.gateway_to_client_baseline) ||
        !nonnegativeSafeInteger(value.client_to_gateway_baseline) ||
        !exactBoolean(value.activation_failed) ||
        (value.activation_epoch !== undefined && !positiveSafeInteger(value.activation_epoch))) {
      throw invalid("gateway returned an invalid media lease");
    }
    const result = {
      lease_id: value.lease_id,
      codec: validatePCMU(value.codec),
      prepared: value.prepared,
      activated: value.activated,
      gateway_to_client_fresh: value.gateway_to_client_fresh,
      client_to_gateway_fresh: value.client_to_gateway_fresh,
      bidirectional_fresh: value.bidirectional_fresh,
      gateway_to_client_frames: value.gateway_to_client_frames,
      client_to_gateway_frames: value.client_to_gateway_frames,
      gateway_to_client_baseline: value.gateway_to_client_baseline,
      client_to_gateway_baseline: value.client_to_gateway_baseline,
      activation_failed: value.activation_failed,
    };
    if (value.activation_epoch !== undefined) result.activation_epoch = value.activation_epoch;
    return Object.freeze(result);
  }

  function validateCallSnapshot(value, requireMedia = false) {
    if (!plainObject(value) || !positiveSafeInteger(value.revision) ||
        !callPhases.has(value.phase) || !exactBoolean(value.controller_owned) ||
        !exactBoolean(value.reconcile_required)) {
      throw invalid("gateway returned an invalid call snapshot");
    }
    const media = value.media === undefined || value.media === null
      ? null
      : validateMediaSnapshot(value.media);
    if (requireMedia && !media) throw invalid("gateway omitted the exact media lease");
    const result = {
      call: validateCallRef(value.call),
      revision: value.revision,
      phase: value.phase,
      controller_owned: value.controller_owned,
      reconcile_required: value.reconcile_required,
      media,
    };
    if (value.direction !== undefined) {
      if (value.direction !== "incoming" && value.direction !== "outgoing") {
        throw invalid("gateway returned an invalid call direction");
      }
      result.direction = value.direction;
    }
    return Object.freeze(result);
  }

  function validateVoiceStatus(value) {
    if (!plainObject(value) || !exactBoolean(value.enabled) || typeof value.health !== "string" ||
        !exactBoolean(value.owned_by_requester) || !exactBoolean(value.answer_enabled) ||
        !exactBoolean(value.end_enabled) || !exactBoolean(value.recovery_required) ||
        (value.dial_enabled !== undefined && !exactBoolean(value.dial_enabled)) ||
        (value.observed_at !== undefined && typeof value.observed_at !== "string")) {
      throw invalid("gateway returned an invalid external voice status");
    }
    const call = value.call === undefined || value.call === null
      ? null
      : validateCallSnapshot(value.call);
    const result = {
      enabled: value.enabled,
      health: value.health,
      call,
      owned_by_requester: value.owned_by_requester,
      answer_enabled: value.answer_enabled,
      end_enabled: value.end_enabled,
      dial_enabled: value.dial_enabled === true,
      recovery_required: value.recovery_required,
    };
    if (value.observed_at !== undefined) result.observed_at = value.observed_at;
    return Object.freeze(result);
  }

  function validateSnapshotResponse(value) {
    if (!plainObject(value) || value.version !== schemaVersion) {
      throw invalid("gateway returned an unsupported external voice snapshot");
    }
    return Object.freeze({ version: schemaVersion, voice: validateVoiceStatus(value.voice) });
  }

  function validateTransportSession(value) {
    if (!plainObject(value) || value.external_voice !== true ||
        value.external_voice_api_version !== schemaVersion || value.sms_only !== false ||
        !["cloudflare-access", "tailscale-serve"].includes(value.transport)) {
      throw invalid("gateway returned an invalid external voice transport session");
    }
    return value.transport;
  }

  function exactCredentialString(value, maximumLength = 512) {
    return typeof value === "string" && value.length > 0 && value.length <= maximumLength &&
      value === value.trim() && !/[\0\r\n]/.test(value);
  }

  function validateICECredentials(value, nowMilliseconds = Date.now()) {
    if (!plainObject(value) || value.version !== 1 || !Array.isArray(value.urls) ||
        value.urls.length !== 3 || new Set(value.urls).size !== value.urls.length ||
        !exactCredentialString(value.username) || !exactCredentialString(value.credential) ||
        !exactCredentialString(value.expires_at, 64)) {
      throw invalid("gateway returned invalid TURN credentials");
    }
    for (const url of value.urls) {
      if (!exactCredentialString(url) || !/^turns?:[^@/?#]+:\d+\?transport=(?:udp|tcp)$/.test(url)) {
        throw invalid("gateway returned an invalid TURN relay URL");
      }
    }
    if (!value.urls.some((url) => /^turn:.*\?transport=udp$/.test(url)) ||
        !value.urls.some((url) => /^turns:.*\?transport=tcp$/.test(url))) {
      throw invalid("gateway omitted a required TURN relay transport");
    }
    const expiresAt = Date.parse(value.expires_at);
    if (!Number.isFinite(expiresAt) || expiresAt <= nowMilliseconds + 30_000 ||
        expiresAt > nowMilliseconds + 11 * 60_000) {
      throw invalid("gateway returned expired or unbounded TURN credentials");
    }
    return Object.freeze({
      urls: Object.freeze([...value.urls]),
      username: value.username,
      credential: value.credential,
      credentialType: "password",
    });
  }

  function validateOfferResponse(value, expectedCall, expectedRevision) {
    if (!plainObject(value) || value.version !== schemaVersion ||
        typeof value.answer_sdp !== "string" || value.answer_sdp.length === 0) {
      throw invalid("gateway returned an invalid media answer");
    }
    const call = validateCallSnapshot(value.call, true);
    const transportPreparing = call.phase === "media_preparing" && call.media.prepared === false;
    const transportReady = call.phase === "media_ready" && call.media.prepared === true;
    if (!sameCall(call.call, expectedCall) || call.revision !== expectedRevision ||
        call.media.activated || call.media.activation_failed ||
        (!transportPreparing && !transportReady)) {
      throw invalid("gateway returned a stale or activated media lease");
    }
    return Object.freeze({ version: schemaVersion, call, answer_sdp: value.answer_sdp });
  }

  function validateReconcileResponse(value, expectedCall) {
    if (!plainObject(value) || value.version !== schemaVersion) {
      throw invalid("gateway returned an invalid reconciliation response");
    }
    const call = validateCallSnapshot(value.call);
    if (!sameCall(call.call, expectedCall)) {
      throw invalid("gateway reconciled a different public call");
    }
    return Object.freeze({ version: schemaVersion, call });
  }

  function splitSDP(value) {
    if (typeof value !== "string" || value.length === 0 || value.length > 96 * 1024 ||
        value.includes("\0")) {
      throw invalid("browser produced an invalid SDP description");
    }
    const lines = value.replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n");
    while (lines.length && lines[lines.length - 1] === "") lines.pop();
    if (!lines.length || lines.some((line) => line.length === 0)) {
      throw invalid("browser produced malformed SDP lines");
    }
    return lines;
  }

  function validateWebRTCSDP(lines) {
    const mediaLines = lines.filter((line) => line.startsWith("m="));
    if (mediaLines.length !== 1 || !mediaLines[0].startsWith("m=audio ")) {
      throw invalid("exactly one audio media section is required");
    }
    const fields = mediaLines[0].split(/ +/);
    if (fields.length < 4 || fields[2] !== "UDP/TLS/RTP/SAVPF" || !fields.slice(3).includes("0")) {
      throw invalid("the browser offer does not support WebRTC PCMU payload 0");
    }
    if (!lines.includes("a=rtcp-mux") ||
        !lines.some((line) => line.startsWith("a=ice-ufrag:")) ||
        !lines.some((line) => line.startsWith("a=ice-pwd:")) ||
        !lines.some((line) => /^a=fingerprint:sha-256 /i.test(line))) {
      throw invalid("the SDP description is missing WebRTC transport proofs");
    }
    for (const line of lines) {
      const match = /^a=rtpmap:(\d+)\s+([^/\s]+)\/(\d+)(?:\/(\d+))?$/i.exec(line);
      if (match && match[1] === "0" &&
          (match[2].toUpperCase() !== "PCMU" || Number(match[3]) !== 8000 ||
           (match[4] !== undefined && Number(match[4]) !== 1))) {
        throw invalid("payload 0 was not mono PCMU at 8 kHz");
      }
    }
  }

  function pcmuOnlySDP(value) {
    const lines = splitSDP(value);
    validateWebRTCSDP(lines);
    const filtered = [];
    for (const line of lines) {
      if (line.startsWith("m=audio ")) {
        const fields = line.split(/ +/);
        filtered.push([...fields.slice(0, 3), "0"].join(" "));
        continue;
      }
      const codecAttribute = /^a=(?:rtpmap|fmtp|rtcp-fb):(\d+|\*)\b/i.exec(line);
      if (codecAttribute && codecAttribute[1] !== "0") continue;
      filtered.push(line);
    }
    const result = `${filtered.join("\r\n")}\r\n`;
    validatePCMUOnlySDP(result);
    return result;
  }

  function validatePCMUOnlySDP(value) {
    const lines = splitSDP(value);
    validateWebRTCSDP(lines);
    const media = lines.find((line) => line.startsWith("m=audio ")).split(/ +/);
    if (media.length !== 4 || media[3] !== "0") {
      throw invalid("the negotiated media description is not PCMU-only");
    }
    for (const line of lines) {
      const codecAttribute = /^a=(?:rtpmap|fmtp|rtcp-fb):(\d+|\*)\b/i.exec(line);
      if (codecAttribute && codecAttribute[1] !== "0") {
        throw invalid("the negotiated media description contains another codec");
      }
    }
    return value;
  }

  function validateRelayOnlySDP(value) {
    const candidates = splitSDP(value).filter((line) => line.startsWith("a=candidate:"));
    if (!candidates.length || candidates.some((line) => !/\styp relay(?:\s|$)/.test(line))) {
      throw invalid("the public browser offer is not TURN relay-only");
    }
    return value;
  }

  function waitForEvent(target, eventName, ready, failed, timeoutMilliseconds, timers, signal) {
    if (ready()) return Promise.resolve();
    if (signal?.aborted) {
      return Promise.reject(new ExternalVoiceError("canceled", "external voice operation was canceled"));
    }
    return new Promise((resolve, reject) => {
      let settled = false;
      let timeout;
      const finish = (error) => {
        if (settled) return;
        settled = true;
        target.removeEventListener(eventName, changed);
        signal?.removeEventListener("abort", aborted);
        timers.clearTimeout(timeout);
        if (error) reject(error); else resolve();
      };
      const changed = () => {
        if (ready()) finish();
        else if (failed()) finish(new ExternalVoiceError("media_failed", "external voice media failed"));
      };
      const aborted = () => finish(new ExternalVoiceError("canceled", "external voice operation was canceled"));
      timeout = timers.setTimeout(
        () => finish(new ExternalVoiceError("timeout", "external voice media timed out")),
        timeoutMilliseconds,
      );
      target.addEventListener(eventName, changed);
      signal?.addEventListener("abort", aborted, { once: true });
      changed();
    });
  }

  function safeJSONParse(text) {
    if (typeof text !== "string" || text.length === 0 || text.length > 128 * 1024) return null;
    try {
      const value = JSON.parse(text);
      return plainObject(value) ? value : null;
    } catch {
      return null;
    }
  }

  function cloneCallRequest(call) {
    if (!call?.media) throw invalid("an exact media lease is required");
    return {
      version: schemaVersion,
      call: {
        public_call_id: call.call.public_call_id,
        generation: call.call.generation,
      },
      expected_revision: call.revision,
      media_lease_id: call.media.lease_id,
    };
  }

  function operationMatchesAuthoritativeCall(kind, localPhase, call) {
    if (!call?.media) return false;
    if (kind === "answer") {
      return localPhase === "prepared" && call.phase === "media_ready" &&
        call.media.prepared === true && call.media.activated === false &&
        call.media.activation_failed === false;
    }
    return kind === "end" && localPhase === "active" && activeCallPhases.has(call.phase) &&
      call.media.prepared === true && call.media.activated === true &&
      call.media.activation_failed === false;
  }

  class MacCellularExternalVoiceClient {
    constructor(options = {}) {
      this.fetchImpl = options.fetchImpl || root.fetch?.bind(root);
      this.peerFactory = options.peerFactory || ((configuration) => new RTCPeerConnection(configuration));
      this.mediaDevices = options.mediaDevices || root.navigator?.mediaDevices;
      this.audioElement = options.audioElement || null;
      this.mediaStreamFactory = options.mediaStreamFactory || ((track) => new MediaStream([track]));
      this.randomUUID = options.randomUUID || root.crypto?.randomUUID?.bind(root.crypto);
      this.csrfToken = typeof options.csrfToken === "function"
        ? options.csrfToken
        : () => options.csrfToken || "";
      this.onState = typeof options.onState === "function" ? options.onState : () => {};
      this.timeoutMilliseconds = options.timeoutMilliseconds || defaultTimeoutMilliseconds;
      this.nowMilliseconds = typeof options.nowMilliseconds === "function"
        ? options.nowMilliseconds
        : () => Date.now();
      this.timers = {
        setTimeout: options.setTimeout || root.setTimeout.bind(root),
        clearTimeout: options.clearTimeout || root.clearTimeout.bind(root),
      };
      this.pageTarget = options.pageTarget === undefined ? root : options.pageTarget;

      this.phase = "idle";
      this.peer = null;
      this.localStream = null;
      this.mediaController = null;
      this.currentCall = null;
      this.remoteTrackSeen = false;
      this.lifecycleGeneration = 1;
      this.controllers = new Set();
      this.commandInFlight = false;
      this.phaseBeforeCommand = "";
      this.unknownOperation = null;
      this.issuedOperations = new WeakSet();
      this.disposed = false;
      this.pagehideHandler = () => this.close("pagehide");
      this.pageTarget?.addEventListener?.("pagehide", this.pagehideHandler);
    }

    snapshot() {
      return Object.freeze({
        phase: this.phase,
        public_call_id: this.currentCall?.call.public_call_id || "",
        call_generation: this.currentCall?.call.generation || 0,
        revision: this.currentCall?.revision || 0,
        media_lease_id: this.currentCall?.media?.lease_id || "",
        remote_track: this.remoteTrackSeen,
        muted: Boolean(this.localStream?.getAudioTracks?.()[0]?.enabled === false),
        reconcile_required: Boolean(this.currentCall?.reconcile_required || this.unknownOperation),
      });
    }

    setPhase(next) {
      if (next === this.phase) return;
      if (!localTransitions[this.phase]?.has(next)) {
        throw invalid(`invalid external voice state transition ${this.phase} -> ${next}`);
      }
      this.phase = next;
      this.publishState();
    }

    publishState() {
      try { this.onState(this.snapshot()); } catch {}
    }

    requireUsable() {
      if (this.disposed) throw new ExternalVoiceError("closed", "external voice client is closed");
      if (typeof this.fetchImpl !== "function") {
        throw new ExternalVoiceError("unavailable", "same-origin fetch is unavailable");
      }
    }

    requireGeneration(generation, peer = this.peer) {
      if (this.disposed || generation !== this.lifecycleGeneration || (peer && peer !== this.peer)) {
        throw new ExternalVoiceError("canceled", "external voice operation was canceled");
      }
    }

    newController() {
      const controller = new AbortController();
      this.controllers.add(controller);
      return controller;
    }

    releaseController(controller) {
      this.controllers.delete(controller);
    }

    mutationHeaders(idempotencyKey = "") {
      const csrf = this.csrfToken();
      if (typeof csrf !== "string" || csrf.length === 0 || csrf !== csrf.trim() || /[\r\n]/.test(csrf)) {
        throw new ExternalVoiceError("forbidden", "an exact CSRF token is required");
      }
      const headers = {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-MacCellular-CSRF": csrf,
      };
      if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
      return headers;
    }

    async requestJSON(path, options = {}) {
      this.requireUsable();
      const controller = this.newController();
      try {
        const response = await this.fetchImpl(path, {
          method: options.method || "GET",
          headers: options.headers || { Accept: "application/json" },
          body: options.body,
          cache: "no-store",
          credentials: "same-origin",
          redirect: "error",
          signal: controller.signal,
        });
        if (!response || typeof response.ok !== "boolean" || !Number.isInteger(response.status) ||
            typeof response.text !== "function") {
          throw invalid("fetch returned an invalid response object");
        }
        const value = safeJSONParse(await response.text());
        if (!response.ok) {
          const code = value && typeof value.code === "string" && responseCodePattern.test(value.code)
            ? value.code
            : "request_failed";
          throw new ExternalVoiceError(code, `external voice request failed (${response.status})`, response.status);
        }
        if (!value) throw invalid("gateway returned invalid JSON");
        return value;
      } finally {
        this.releaseController(controller);
      }
    }

    async refresh() {
      this.requireUsable();
      const generation = this.lifecycleGeneration;
      const response = validateSnapshotResponse(await this.requestJSON(api.snapshot));
      this.requireGeneration(generation, null);
      this.adoptVoiceStatus(response.voice);
      return response;
    }

    async peerConfiguration(generation) {
      const session = await this.requestJSON(api.session);
      this.requireGeneration(generation, null);
      const transport = validateTransportSession(session);
      if (transport === "tailscale-serve") {
        return Object.freeze({
          iceServers: Object.freeze([]),
          bundlePolicy: "max-bundle",
          rtcpMuxPolicy: "require",
        });
      }
      const credentials = validateICECredentials(
        await this.requestJSON(api.iceCredentials),
        this.nowMilliseconds(),
      );
      this.requireGeneration(generation, null);
      return Object.freeze({
        iceServers: Object.freeze([credentials]),
        iceTransportPolicy: "relay",
        bundlePolicy: "max-bundle",
        rtcpMuxPolicy: "require",
      });
    }

    adoptVoiceStatus(voice) {
      if (!this.currentCall) return;
      const next = voice.call;
      const mediaTransitional = [
        "requesting_microphone", "creating_offer", "gathering_ice", "exchanging_offer",
        "connecting",
      ].includes(this.phase);
      const operationTransitional = ["command_pending", "reconciling"].includes(this.phase);
      if (!voice.enabled || voice.health !== "connected" || !next ||
          !sameCall(next.call, this.currentCall.call) || next.phase === "ended") {
        this.close("authoritative-call-changed");
        return;
      }
      if (operationTransitional) {
        if (!voice.owned_by_requester || !next.media ||
            next.media.lease_id !== this.currentCall.media?.lease_id) {
          this.close("authoritative-call-changed");
        }
        return;
      }
      if (mediaTransitional) {
        if (next.revision !== this.currentCall.revision ||
            (this.currentCall.media && (!voice.owned_by_requester || !next.media ||
             next.media.lease_id !== this.currentCall.media.lease_id))) {
          this.close("authoritative-call-changed");
        }
        return;
      }
      if (!voice.owned_by_requester || !next.media ||
          next.media.lease_id !== this.currentCall.media?.lease_id) {
        this.close("authoritative-call-changed");
        return;
      }
      this.currentCall = next;
      if (next.reconcile_required) {
        if (this.phase !== "outcome_unknown") this.setPhase("outcome_unknown");
        return;
      }
      this.unknownOperation = null;
      const nextPhase = activeCallPhases.has(next.phase) ? "active" : "prepared";
      if (["prepared", "active", "awaiting_snapshot", "outcome_unknown"].includes(this.phase)) {
        this.setPhase(nextPhase);
      }
    }

    async prepare(callSnapshot) {
      this.requireUsable();
      if (this.phase !== "idle" || this.commandInFlight) {
        throw new ExternalVoiceError("conflict", "an external voice session is already active");
      }
      if (!this.mediaDevices?.getUserMedia || typeof this.randomUUID !== "function") {
        throw new ExternalVoiceError("unavailable", "microphone or secure randomness is unavailable");
      }
      const call = validateCallSnapshot(callSnapshot);
      if (call.phase !== "incoming_ringing" || call.media) {
        throw new ExternalVoiceError("stale_call", "the exact ringing call is no longer unclaimed");
      }

      const generation = this.lifecycleGeneration;
      const mediaController = new AbortController();
      this.mediaController = mediaController;
      this.currentCall = call;
      this.setPhase("requesting_microphone");
      try {
        const stream = await this.mediaDevices.getUserMedia({
          audio: {
            channelCount: { exact: 1 },
            echoCancellation: true,
            noiseSuppression: true,
            autoGainControl: true,
          },
          video: false,
        });
        if (generation !== this.lifecycleGeneration || this.disposed) {
          for (const track of stream?.getTracks?.() || []) track.stop?.();
          throw new ExternalVoiceError("canceled", "external voice operation was canceled");
        }
        if (!stream || typeof stream.getAudioTracks !== "function" ||
            stream.getAudioTracks().length !== 1 ||
            (stream.getVideoTracks?.().length || 0) !== 0) {
          for (const track of stream?.getTracks?.() || []) track.stop?.();
          throw invalid("exactly one microphone audio track is required");
        }
        this.localStream = stream;
        this.setPhase("creating_offer");

        const peerConfiguration = await this.peerConfiguration(generation);
        this.requireGeneration(generation, null);
        const peer = this.peerFactory(peerConfiguration);
        if (!peer || typeof peer.addTransceiver !== "function" ||
            typeof peer.createOffer !== "function" || typeof peer.setLocalDescription !== "function" ||
            typeof peer.setRemoteDescription !== "function") {
          throw invalid("peer factory returned an invalid RTCPeerConnection");
        }
        this.peer = peer;
        const transceiver = peer.addTransceiver(stream.getAudioTracks()[0], {
          direction: "sendrecv",
          streams: [stream],
        });
        if (!transceiver || (peer.getTransceivers && peer.getTransceivers().length !== 1)) {
          throw invalid("exactly one audio transceiver is required");
        }
        peer.addEventListener("track", (event) => this.onRemoteTrack(generation, peer, event));
        peer.addEventListener("connectionstatechange", () => {
          if (generation === this.lifecycleGeneration && peer === this.peer &&
              ["failed", "closed"].includes(peer.connectionState)) {
            this.close("peer-failed");
          }
        });

        const offer = await peer.createOffer();
        this.requireGeneration(generation, peer);
        if (!plainObject(offer) || offer.type !== "offer") {
          throw invalid("browser did not create an SDP offer");
        }
        const restrictedOffer = Object.freeze({ type: "offer", sdp: pcmuOnlySDP(offer.sdp) });
        await peer.setLocalDescription(restrictedOffer);
        this.requireGeneration(generation, peer);
        this.setPhase("gathering_ice");
        await waitForEvent(
          peer,
          "icegatheringstatechange",
          () => peer === this.peer && peer.iceGatheringState === "complete",
          () => peer !== this.peer || ["failed", "closed"].includes(peer.connectionState),
          this.timeoutMilliseconds,
          this.timers,
          mediaController.signal,
        );
        this.requireGeneration(generation, peer);
        const localDescription = peer.localDescription;
        if (!localDescription || localDescription.type !== "offer") {
          throw invalid("browser did not retain the complete local offer");
        }
        const offerSDP = pcmuOnlySDP(localDescription.sdp);
        if (peerConfiguration.iceTransportPolicy === "relay") validateRelayOnlySDP(offerSDP);
        const clientNonce = this.randomUUID();
        if (typeof clientNonce !== "string" || !noncePattern.test(clientNonce)) {
          throw invalid("secure client nonce is invalid");
        }
        const body = JSON.stringify({
          version: schemaVersion,
          call: {
            public_call_id: call.call.public_call_id,
            generation: call.call.generation,
          },
          expected_revision: call.revision,
          client_nonce: clientNonce,
          sdp_offer: offerSDP,
        });
        this.setPhase("exchanging_offer");
        const rawAnswer = await this.requestJSON(api.offer, {
          method: "POST",
          headers: this.mutationHeaders(),
          body,
        });
        this.requireGeneration(generation, peer);
        const answer = validateOfferResponse(rawAnswer, call.call, call.revision);
        validatePCMUOnlySDP(answer.answer_sdp);
        this.currentCall = answer.call;
        await peer.setRemoteDescription({ type: "answer", sdp: answer.answer_sdp });
        this.requireGeneration(generation, peer);
        this.setPhase("connecting");
        await waitForEvent(
          peer,
          "connectionstatechange",
          () => peer === this.peer && peer.connectionState === "connected",
          () => peer !== this.peer || ["failed", "closed"].includes(peer.connectionState),
          this.timeoutMilliseconds,
          this.timers,
          mediaController.signal,
        );
        await waitForEvent(
          peer,
          "track",
          () => peer === this.peer && this.remoteTrackSeen,
          () => peer !== this.peer || ["failed", "closed"].includes(peer.connectionState),
          this.timeoutMilliseconds,
          this.timers,
          mediaController.signal,
        );
        this.requireGeneration(generation, peer);
        if (this.audioElement) await this.audioElement.play();
        this.requireGeneration(generation, peer);
        this.setPhase("prepared");
        return this.snapshot();
      } catch (error) {
        if (generation === this.lifecycleGeneration) this.close("prepare-failed");
        throw error;
      }
    }

    onRemoteTrack(generation, peer, event) {
      if (generation !== this.lifecycleGeneration || peer !== this.peer) return;
      if (this.remoteTrackSeen || event?.track?.kind !== "audio") {
        this.close("unexpected-remote-track");
        return;
      }
      this.remoteTrackSeen = true;
      if (this.audioElement) {
        this.audioElement.srcObject = event.streams?.[0] || this.mediaStreamFactory(event.track);
      }
      this.publishState();
    }

    createPersistentOperation(kind, idempotencyKey) {
      this.requireUsable();
      if (!idempotencyKeyPattern.test(idempotencyKey || "") ||
          !operationMatchesAuthoritativeCall(kind, this.phase, this.currentCall) ||
          this.commandInFlight || this.unknownOperation) {
        throw new ExternalVoiceError("conflict", "an exact in-memory call operation cannot be created");
      }
      const operation = Object.freeze({
        kind,
        path: api[kind],
        body: JSON.stringify(cloneCallRequest(this.currentCall)),
        idempotencyKey,
      });
      this.issuedOperations.add(operation);
      return operation;
    }

    validatePersistentOperation(operation) {
      if (!plainObject(operation) || !Object.isFrozen(operation) ||
          !this.issuedOperations.has(operation) || !["answer", "end"].includes(operation.kind) ||
          operation.path !== api[operation.kind] || !idempotencyKeyPattern.test(operation.idempotencyKey || "") ||
          typeof operation.body !== "string" || operation.body !== JSON.stringify(cloneCallRequest(this.currentCall)) ||
          !operationMatchesAuthoritativeCall(operation.kind, this.phase, this.currentCall)) {
        throw new ExternalVoiceError("conflict", "the persistent operation body or key changed");
      }
    }

    async submitPersistent(operation) {
      this.requireUsable();
      if (this.commandInFlight || this.unknownOperation) {
        throw new ExternalVoiceError("conflict", "a command outcome must be reconciled first");
      }
      this.validatePersistentOperation(operation);
      const headers = this.mutationHeaders(operation.idempotencyKey);
      const generation = this.lifecycleGeneration;
      this.commandInFlight = true;
      this.phaseBeforeCommand = this.phase;
      this.setPhase("command_pending");
      try {
        const value = await this.requestJSON(operation.path, {
          method: "POST",
          headers,
          body: operation.body,
        });
        this.requireGeneration(generation);
        if (value.ok !== true || !["completed", "completed_audit_unavailable"].includes(value.code)) {
          throw invalid("gateway returned an invalid persistent operation receipt");
        }
        this.commandInFlight = false;
        this.phaseBeforeCommand = "";
        if (operation.kind === "end") {
          const receipt = Object.freeze({ ok: true, code: value.code });
          this.close("end-confirmed");
          return receipt;
        }
        this.setPhase("awaiting_snapshot");
        return Object.freeze({ ok: true, code: value.code });
      } catch (error) {
        if (generation !== this.lifecycleGeneration) throw error;
        this.commandInFlight = false;
        if (error?.code === "unknown_outcome" || !error?.httpStatus) {
          this.unknownOperation = operation;
          this.phaseBeforeCommand = "";
          this.setPhase("outcome_unknown");
        } else {
          const previous = this.phaseBeforeCommand;
          this.phaseBeforeCommand = "";
          this.setPhase(previous);
        }
        throw error;
      }
    }

    async reconcile() {
      this.requireUsable();
      if (this.phase !== "outcome_unknown" || !this.unknownOperation || this.commandInFlight) {
        throw new ExternalVoiceError("conflict", "there is no exact unknown command to reconcile");
      }
      const operation = this.unknownOperation;
      const request = safeJSONParse(operation.body);
      const expectedCall = validateCallRef(request?.call);
      const headers = this.mutationHeaders();
      const generation = this.lifecycleGeneration;
      this.setPhase("reconciling");
      try {
        const raw = await this.requestJSON(api.reconcile, {
          method: "POST",
          headers,
          body: operation.body,
        });
        this.requireGeneration(generation);
        const result = validateReconcileResponse(raw, expectedCall);
        if (result.call.phase === "ended" || !result.call.media) {
          this.unknownOperation = null;
          this.close("reconciled-ended");
          return result;
        }
        this.currentCall = result.call;
        if (result.call.reconcile_required) {
          this.setPhase("outcome_unknown");
        } else {
          this.unknownOperation = null;
          this.setPhase(activeCallPhases.has(result.call.phase) ? "active" : "prepared");
        }
        return result;
      } catch (error) {
        if (generation === this.lifecycleGeneration) this.setPhase("outcome_unknown");
        throw error;
      }
    }

    setMuted(muted) {
      if (typeof muted !== "boolean") throw invalid("muted must be boolean");
      for (const track of this.localStream?.getAudioTracks?.() || []) track.enabled = !muted;
      this.publishState();
    }

    close(reason = "closed") {
      void reason;
      this.lifecycleGeneration += 1;
      this.mediaController?.abort();
      this.mediaController = null;
      for (const controller of this.controllers) controller.abort();
      this.controllers.clear();
      const peer = this.peer;
      const stream = this.localStream;
      this.peer = null;
      this.localStream = null;
      this.currentCall = null;
      this.remoteTrackSeen = false;
      this.commandInFlight = false;
      this.phaseBeforeCommand = "";
      this.unknownOperation = null;
      try { peer?.close?.(); } catch {}
      for (const track of stream?.getTracks?.() || []) {
        try { track.stop?.(); } catch {}
      }
      if (this.audioElement) {
        try { this.audioElement.pause?.(); } catch {}
        this.audioElement.srcObject = null;
      }
      if (this.phase !== "idle") {
        this.phase = "idle";
        this.publishState();
      }
    }

    dispose() {
      if (this.disposed) return;
      this.close("disposed");
      this.disposed = true;
      this.pageTarget?.removeEventListener?.("pagehide", this.pagehideHandler);
    }
  }

  const exported = Object.freeze({
    MacCellularExternalVoiceClient,
    ExternalVoiceError,
    API: api,
    pcmuOnlySDP,
    validatePCMUOnlySDP,
  });
  root.MacCellularExternalVoiceClient = MacCellularExternalVoiceClient;
  root.MacCellularExternalVoiceError = ExternalVoiceError;
  if (typeof module === "object" && module && module.exports) module.exports = exported;
})(globalThis);
