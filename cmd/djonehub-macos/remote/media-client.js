(function installRemoteMediaClient(root) {
  "use strict";

  // Chrome can wait through several UDP retransmissions before trying the
  // authenticated TURN/TCP fallback. Keep this bounded but aligned with the
  // external public voice client so a blackholed UDP path does not fail early.
  const defaultTimeoutMilliseconds = 28_000;

  function publicIPv4Host(host) {
    const parts = String(host || "").split(".");
    if (parts.length !== 4 || parts.some((part) => !/^(?:0|[1-9]\d{0,2})$/.test(part))) return false;
    const bytes = parts.map(Number);
    if (bytes.some((value) => value < 0 || value > 255) || bytes.join(".") !== host) return false;
    const [a, b, c] = bytes;
    return a !== 0 && a < 224 && a !== 10 && a !== 127 &&
      !(a === 100 && b >= 64 && b <= 127) && !(a === 169 && b === 254) &&
      !(a === 172 && b >= 16 && b <= 31) && !(a === 192 && b === 168) &&
      !(a === 192 && b === 0 && (c === 0 || c === 2)) &&
      !(a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) &&
      !(a === 203 && b === 0 && c === 113);
  }

  function canonicalDNSHost(host) {
    return typeof host === "string" && host.includes(".") && host.length <= 253 &&
      host.split(".").every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label));
  }

  function validateRelayCredentials(value, now = Date.now()) {
    if (!value || value.version !== 1 || !Array.isArray(value.urls) || value.urls.length !== 3 ||
        typeof value.username !== "string" || !value.username ||
        typeof value.credential !== "string" || !value.credential ||
        typeof value.expires_at !== "string") {
      throw new Error("public relay credentials are invalid");
    }
    const patterns = [
      /^turn:[A-Za-z0-9.-]+:\d+\?transport=udp$/,
      /^turn:[A-Za-z0-9.-]+:\d+\?transport=tcp$/,
      /^turns:[A-Za-z0-9.-]+:\d+\?transport=tcp$/,
    ];
    if (!value.urls.every((url, index) => typeof url === "string" && patterns[index].test(url))) {
      throw new Error("public relay URLs are invalid");
    }
    const hosts = value.urls.map((url) => url.replace(/^turns?:/, "").split(":", 1)[0].toLowerCase());
    const namedBundle = hosts[0] && hosts.every((host) => host === hosts[0]);
    const resolvedBundle = hosts[0] === hosts[1] && publicIPv4Host(hosts[0]) &&
      canonicalDNSHost(hosts[2]) && !publicIPv4Host(hosts[2]);
    if (!namedBundle && !resolvedBundle) {
      throw new Error("public relay hosts do not match");
    }
    const expires = Date.parse(value.expires_at);
    if (!Number.isFinite(expires) || expires <= now || expires > now + 10 * 60_000) {
      throw new Error("public relay credential lifetime is invalid");
    }
    // Public mobile media uses the authenticated TLS/TCP relay exclusively.
    // Some cellular networks can complete TURN/UDP ICE checks but then lose
    // or heavily disrupt RTP.  The same deployed relay has a verified 443
    // TLS data path, so do not let WebKit choose the unreliable 3478 routes.
    return Object.freeze({
      urls: Object.freeze([value.urls[2]]), username: value.username,
      credential: value.credential, credentialType: "password",
    });
  }

  function relayOnlySDP(sdp) {
    const candidates = String(sdp || "").split(/\r?\n/).filter((line) => line.startsWith("a=candidate:"));
    return candidates.length > 0 && candidates.every((line) => /(?:^|\s)typ relay(?:\s|$)/.test(line));
  }

  function validCall(call) {
    return Boolean(
      call &&
      typeof call.call_id === "string" && call.call_id.length > 0 &&
      Number.isSafeInteger(call.call_generation) && call.call_generation > 0 &&
      Number.isInteger(call.call_index) && call.call_index >= 0 &&
      call.call_direction === "incoming"
    );
  }

  function validOutgoing(callGeneration, number) {
    return Number.isSafeInteger(callGeneration) && callGeneration > 0 &&
      typeof number === "string" && number.length > 0 && number.length <= 64 &&
      /^[+*#0-9 ()-]+$/.test(number);
  }

  function waitForState(target, eventName, ready, failed, timeoutMilliseconds, timers) {
    if (ready()) return Promise.resolve();
    return new Promise((resolve, reject) => {
      let settled = false;
      const finish = (error) => {
        if (settled) return;
        settled = true;
        target.removeEventListener(eventName, changed);
        timers.clearTimeout(timeout);
        if (error) reject(error); else resolve();
      };
      const changed = () => {
        if (ready()) finish();
        else if (failed()) finish(new Error(`remote media ${eventName} failed`));
      };
      const timeout = timers.setTimeout(
        () => finish(new Error(`remote media ${eventName} timed out`)),
        timeoutMilliseconds,
      );
      target.addEventListener(eventName, changed);
      changed();
    });
  }

  class MacCellularRemoteMediaClient {
    constructor(options = {}) {
      this.peerFactory = options.peerFactory || ((configuration) => new RTCPeerConnection(configuration));
      this.mediaDevices = options.mediaDevices || root.navigator?.mediaDevices;
      this.audioElement = options.audioElement || root.document?.querySelector("#remote-audio") || null;
      this.mediaStreamFactory = options.mediaStreamFactory || ((track) => new MediaStream([track]));
      this.timers = {
        setTimeout: options.setTimeout || root.setTimeout.bind(root),
        clearTimeout: options.clearTimeout || root.clearTimeout.bind(root),
      };
      this.timeoutMilliseconds = options.timeoutMilliseconds || defaultTimeoutMilliseconds;
      this.onState = options.onState || (() => {});
      this.transport = options.transport || (() => "tailscale-serve");
      this.fetchImpl = options.fetchImpl || root.fetch?.bind(root);
      this.iceCredentialsPath = options.iceCredentialsPath || "/api/remote/v2/voice/media/ice-credentials";
      const AudioContextClass = root.AudioContext || root.webkitAudioContext;
      this.audioContextFactory = options.audioContextFactory ||
        (typeof AudioContextClass === "function" ? () => new AudioContextClass() : null);
      this.peer = null;
      this.localStream = null;
      this.peerStream = null;
      this.remoteStream = null;
      this.audioContext = null;
      this.silenceSource = null;
      this.remotePlaybackSource = null;
      this.localAnalyser = null;
      this.remoteAnalyser = null;
      this.call = null;
      this.callGeneration = 0;
      this.purpose = "";
      this.mediaSessionID = "";
      this.leaseGeneration = 0;
      this.phase = "idle";
      this.remoteTrackSeen = false;
      this.playbackPromise = null;
      this.iceErrorCodes = new Set();
      this.lastCloseReason = "";
      this.operationGeneration = 0;
    }

    async clockedMicrophoneStream(stream) {
      if (typeof this.audioContextFactory !== "function") return stream;
      const context = this.audioContextFactory();
      try {
        // A zero-valued source keeps the Web Audio graph clocking while the
        // room is silent. The real microphone remains mixed at unity gain, so
        // this changes neither its level nor its content; it only prevents a
        // browser from starving the RTP sender before the call is accepted.
        const microphone = context.createMediaStreamSource(stream);
        const destination = context.createMediaStreamDestination();
        const silence = context.createConstantSource();
        silence.offset.value = 0;
        microphone.connect(destination);
        if (typeof context.createAnalyser === "function") {
          this.localAnalyser = context.createAnalyser();
          this.localAnalyser.fftSize = 256;
          microphone.connect(this.localAnalyser);
        }
        silence.connect(destination);
        silence.start();
        await context.resume();
        if (context.state !== "running" || destination.stream.getAudioTracks().length !== 1) {
          throw new Error("audio graph did not start");
        }
        this.audioContext = context;
        this.silenceSource = silence;
        return destination.stream;
      } catch (error) {
        this.localAnalyser = null;
        try { await context.close(); } catch {}
        return stream;
      }
    }

    async peerConfiguration() {
      const transport = typeof this.transport === "function" ? this.transport() : this.transport;
      if (transport === "tailscale-serve") {
        return { iceServers: [], bundlePolicy: "max-bundle", rtcpMuxPolicy: "require" };
      }
      if (transport !== "cloudflare-access" || typeof this.fetchImpl !== "function") {
        throw new Error("remote media transport is unavailable");
      }
      const response = await this.fetchImpl(this.iceCredentialsPath, {
        method: "GET", credentials: "same-origin", cache: "no-store",
        headers: { Accept: "application/json" },
      });
      if (!response?.ok) throw new Error("public relay credentials are unavailable");
      const relay = validateRelayCredentials(await response.json());
      return {
        iceServers: [relay], iceTransportPolicy: "relay",
        bundlePolicy: "max-bundle", rtcpMuxPolicy: "require",
      };
    }

    async captureMicrophone(operation) {
      let timedOut = false;
      let timeout = null;
      const capture = Promise.resolve().then(() => this.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        },
        video: false,
      })).then((stream) => {
        if (timedOut || operation !== this.operationGeneration) {
          for (const track of stream?.getTracks?.() || []) track.stop();
          throw new Error(timedOut
            ? "microphone request timed out"
            : "remote media preparation was canceled");
        }
        return stream;
      });
      const deadline = new Promise((_, reject) => {
        timeout = this.timers.setTimeout(() => {
          timedOut = true;
          reject(new Error("microphone request timed out"));
        }, this.timeoutMilliseconds);
      });
      try {
        return await Promise.race([capture, deadline]);
      } finally {
        this.timers.clearTimeout(timeout);
      }
    }

    async microphonePermissionState() {
      const permissions = root.navigator?.permissions;
      if (!permissions?.query) return "unknown";
      try {
        const status = await permissions.query({ name: "microphone" });
        return ["granted", "prompt", "denied"].includes(status?.state) ? status.state : "unknown";
      } catch {
        return "unknown";
      }
    }

    snapshot() {
      return Object.freeze({
        phase: this.phase,
        media_session_id: this.mediaSessionID,
        lease_generation: this.leaseGeneration,
        call_generation: this.callGeneration,
        purpose: this.purpose,
        remote_track: this.remoteTrackSeen,
      });
    }

    setPhase(phase) {
      this.phase = phase;
      this.onState(this.snapshot());
    }

    ensurePlayback() {
      const audio = this.audioElement;
      if (!audio || this.phase === "idle" || !this.remoteTrackSeen || !audio.srcObject) {
        return Promise.resolve(false);
      }
      audio.autoplay = true;
      audio.playsInline = true;
      audio.muted = false;
      audio.volume = 1;
      if (audio.paused === false && audio.ended !== true) return Promise.resolve(true);
      if (this.playbackPromise) return this.playbackPromise;
      const pending = Promise.resolve().then(() => audio.play()).then(() => true, () => false).finally(() => {
        if (this.playbackPromise === pending) this.playbackPromise = null;
      });
      this.playbackPromise = pending;
      return pending;
    }

    async prepare(call, exchangeOffer) {
      if (!validCall(call) || typeof exchangeOffer !== "function") {
        throw new Error("exact call identity and offer exchange are required");
      }
      return this.prepareSource({ purpose: "incoming", call }, call.call_generation, call, exchangeOffer);
    }

    async prepareOutgoing(callGeneration, number, exchangeOffer) {
      if (!Number.isSafeInteger(callGeneration) || callGeneration <= 0) {
        throw new Error("remote media invalid call generation");
      }
      if (!validOutgoing(callGeneration, number)) throw new Error("remote media invalid dial number");
      if (typeof exchangeOffer !== "function") throw new Error("remote media offer exchange unavailable");
      return this.prepareSource({
        purpose: "outgoing", expected_call_generation: callGeneration, number,
      }, callGeneration, null, exchangeOffer);
    }

    async prepareSource(offerSource, callGeneration, call, exchangeOffer) {
      if (this.phase !== "idle") {
        throw new Error("remote media session already active");
      }
      if (!this.mediaDevices?.getUserMedia) {
        throw new Error("microphone capture is unavailable");
      }

      const operation = ++this.operationGeneration;
      this.lastCloseReason = "";
      let failureStage = "relay-credentials";
      const isCurrent = (peer = this.peer) =>
        operation === this.operationGeneration && (!peer || this.peer === peer);
      const requireCurrent = (peer = this.peer) => {
        if (!isCurrent(peer)) throw new Error("remote media preparation was canceled");
      };

      this.call = call ? Object.freeze({ ...call }) : null;
      this.callGeneration = callGeneration;
      this.purpose = offerSource.purpose;
      this.setPhase("requesting-microphone");
      try {
        const peerConfiguration = await this.peerConfiguration();
        requireCurrent(null);
        failureStage = "microphone";
        const stream = await this.captureMicrophone(operation);
        if (!isCurrent(null)) {
          for (const track of stream?.getTracks?.() || []) track.stop();
          throw new Error("remote media preparation was canceled");
        }
        this.localStream = stream;
        const peerStream = await this.clockedMicrophoneStream(stream);
        requireCurrent(null);
        this.peerStream = peerStream;
        const tracks = peerStream.getAudioTracks();
        if (tracks.length !== 1) throw new Error("exactly one microphone track is required");

        failureStage = "browser-peer";
        const peer = this.peerFactory(peerConfiguration);
        this.peer = peer;
        peer.addEventListener("icecandidate", (event) => {
          if (!isCurrent(peer) || !event.candidate) return;
          if (!/(?:^|\s)typ relay(?:\s|$)/.test(event.candidate.candidate || "")) {
            this.close("non-relay-candidate");
          }
        });
        peer.addEventListener("icecandidateerror", (event) => {
          if (!isCurrent(peer)) return;
          const code = Number(event?.errorCode);
          if (Number.isInteger(code) && code >= 300 && code <= 799) {
            this.iceErrorCodes.add(code);
          }
        });
        peer.addTransceiver(tracks[0], {
          direction: "sendrecv",
          streams: [peerStream],
        });
        peer.addEventListener("track", (event) => {
          if (!isCurrent(peer)) return;
          if (event.track?.kind !== "audio" || this.remoteTrackSeen) {
            this.close("unexpected-remote-track");
            return;
          }
          this.remoteTrackSeen = true;
          event.track.enabled = true;
          if (this.audioContext?.state === "running") {
            try {
              this.remoteStream = this.mediaStreamFactory(event.track);
              this.remotePlaybackSource = this.audioContext.createMediaStreamSource(this.remoteStream);
              if (typeof this.audioContext.createAnalyser === "function") {
                this.remoteAnalyser = this.audioContext.createAnalyser();
                this.remoteAnalyser.fftSize = 256;
                this.remotePlaybackSource.connect(this.remoteAnalyser);
                this.remoteAnalyser.connect(this.audioContext.destination);
              } else {
                this.remotePlaybackSource.connect(this.audioContext.destination);
              }
            } catch {
              this.remotePlaybackSource = null;
              this.remoteAnalyser = null;
              this.remoteStream = null;
            }
          }
          if (this.audioElement) {
            // WebKit can expose a remote stream whose membership lags the
            // track event. Bind the exact received track instead of trusting
            // event.streams[0], otherwise Safari may report a live receiver
            // while the audio element renders an empty stream.
            this.audioElement.autoplay = true;
            this.audioElement.playsInline = true;
            this.audioElement.muted = Boolean(this.remotePlaybackSource);
            this.audioElement.volume = 1;
            if (!this.remoteStream) this.remoteStream = this.mediaStreamFactory(event.track);
            this.audioElement.srcObject = this.remoteStream;
            if (!this.remotePlaybackSource) void this.ensurePlayback();
          }
          this.onState(this.snapshot());
        });
        peer.addEventListener("connectionstatechange", () => {
          if (isCurrent(peer) && ["failed", "closed"].includes(peer.connectionState)) {
            this.close("peer-failed");
          }
        });

        this.setPhase("offering");
        requireCurrent(peer);
        failureStage = "local-offer";
        const offer = await peer.createOffer();
        requireCurrent(peer);
        await peer.setLocalDescription(offer);
        requireCurrent(peer);
        failureStage = "relay-candidate";
        try {
          if (peerConfiguration.iceTransportPolicy === "relay") {
            // A blackholed UDP TURN URL can keep Chrome in "gathering" long
            // after its TCP/TLS relay candidate is usable. This client does
            // not trickle ICE, but a localDescription containing one relay
            // candidate is already a complete offer for our single bundled
            // audio transport. Continue at that point instead of waiting for
            // every failed fallback path to finish retransmitting.
            await waitForState(
              peer,
              "icecandidate",
              () => isCurrent(peer) && relayOnlySDP(peer.localDescription?.sdp),
              () => !isCurrent(peer) || ["closed", "failed"].includes(peer.connectionState) ||
                peer.iceGatheringState === "complete",
              this.timeoutMilliseconds,
              this.timers,
            );
          } else {
            await waitForState(
              peer,
              "icegatheringstatechange",
              () => isCurrent(peer) && peer.iceGatheringState === "complete",
              () => !isCurrent(peer) || ["closed", "failed"].includes(peer.connectionState),
              this.timeoutMilliseconds,
              this.timers,
            );
          }
        } catch (error) {
          const message = String(error?.message || "");
          if ((message.includes("icecandidate") || message.includes("icegatheringstatechange")) &&
              this.iceErrorCodes.size > 0) {
            const codes = [...this.iceErrorCodes].sort((left, right) => left - right).join(",");
            throw new Error(`remote media relay candidate failed (ICE ${codes})`);
          }
          throw error;
        }
        requireCurrent(peer);
        const localDescription = peer.localDescription;
        if (localDescription?.type !== "offer" || !localDescription.sdp) {
          throw new Error("browser did not produce a complete SDP offer");
        }
        if (peerConfiguration.iceTransportPolicy === "relay" && !relayOnlySDP(localDescription.sdp)) {
          throw new Error("browser produced a non-relay media offer");
        }

        failureStage = "gateway-offer";
        const answer = await exchangeOffer({
          ...offerSource,
          client_nonce: root.crypto.randomUUID(),
          sdp_offer: localDescription.sdp,
        });
        requireCurrent(peer);
        if (!answer || typeof answer.sdp_answer !== "string" || !answer.sdp_answer ||
            typeof answer.media_session_id !== "string" || !answer.media_session_id ||
            !Number.isSafeInteger(answer.lease_generation) || answer.lease_generation <= 0 ||
            answer.call_generation !== this.callGeneration) {
          throw new Error("gateway returned a mismatched media answer");
        }
        if (peerConfiguration.iceTransportPolicy === "relay" && !relayOnlySDP(answer.sdp_answer)) {
          throw new Error("gateway returned a non-relay media answer");
        }
        this.mediaSessionID = answer.media_session_id;
        this.leaseGeneration = answer.lease_generation;
        failureStage = "remote-answer";
        await peer.setRemoteDescription({ type: "answer", sdp: answer.sdp_answer });
        requireCurrent(peer);
        failureStage = "relay-connect";
        await waitForState(
          peer,
          "connectionstatechange",
          () => isCurrent(peer) && peer.connectionState === "connected",
          () => !isCurrent(peer) || ["failed", "closed"].includes(peer.connectionState),
          this.timeoutMilliseconds,
          this.timers,
        );
        requireCurrent(peer);
        await waitForState(
          peer,
          "track",
          () => isCurrent(peer) && this.remoteTrackSeen,
          () => !isCurrent(peer) || ["failed", "closed"].includes(peer.connectionState),
          this.timeoutMilliseconds,
          this.timers,
        );
        requireCurrent(peer);
        if (this.audioElement && !this.remotePlaybackSource) {
          failureStage = "audio-play";
          if (!await this.ensurePlayback()) throw new Error("remote audio playback did not start");
        }
        requireCurrent(peer);
        this.setPhase("prepared");
        return this.snapshot();
      } catch (error) {
        if (operation !== this.operationGeneration) {
          if (this.lastCloseReason === "non-relay-candidate") {
            throw new Error("browser produced a non-relay media candidate");
          }
          if (this.lastCloseReason === "peer-failed") {
            throw new Error("remote media peer failed during preparation");
          }
          if (this.lastCloseReason) {
            throw new Error(`remote media canceled (${this.lastCloseReason})`);
          }
        }
        if (operation === this.operationGeneration) this.close("prepare-failed");
        const message = typeof error?.message === "string" ? error.message : "";
        if (message.startsWith("remote media ") || message.includes("relay credentials") ||
            message.includes("microphone") || message.includes("browser produced") ||
            message.includes("gateway returned") || message.includes("media answer")) {
          throw error;
        }
        throw new Error(`remote media ${failureStage} failed`);
      }
    }

    setMuted(muted) {
      for (const track of this.localStream?.getAudioTracks?.() || []) {
        track.enabled = !muted;
      }
    }

    recordableStreams() {
      return Object.freeze({ localStream: this.localStream, remoteStream: this.remoteStream });
    }

    audioLevels() {
      const level = (analyser) => {
        if (!analyser?.getByteTimeDomainData) return 0;
        const samples = new Uint8Array(analyser.fftSize || 256);
        analyser.getByteTimeDomainData(samples);
        let energy = 0;
        for (const sample of samples) {
          const normalized = (sample - 128) / 128;
          energy += normalized * normalized;
        }
        return Math.min(1, Math.sqrt(energy / samples.length) * 5);
      };
      return Object.freeze({ local: level(this.localAnalyser), remote: level(this.remoteAnalyser) });
    }

    close(reason = "closed") {
      this.lastCloseReason = typeof reason === "string" ? reason : "closed";
      this.operationGeneration += 1;
      const peer = this.peer;
      const stream = this.localStream;
      const peerStream = this.peerStream;
      const audioContext = this.audioContext;
      const silenceSource = this.silenceSource;
      const remotePlaybackSource = this.remotePlaybackSource;
      const localAnalyser = this.localAnalyser;
      const remoteAnalyser = this.remoteAnalyser;
      this.peer = null;
      this.localStream = null;
      this.peerStream = null;
      this.remoteStream = null;
      this.audioContext = null;
      this.silenceSource = null;
      this.remotePlaybackSource = null;
      this.localAnalyser = null;
      this.remoteAnalyser = null;
      this.call = null;
      this.callGeneration = 0;
      this.purpose = "";
      this.mediaSessionID = "";
      this.leaseGeneration = 0;
      this.remoteTrackSeen = false;
      this.playbackPromise = null;
      this.iceErrorCodes.clear();
      try { peer?.close(); } catch {}
      for (const track of stream?.getTracks?.() || []) {
        try { track.stop(); } catch {}
      }
      if (peerStream && peerStream !== stream) {
        for (const track of peerStream.getTracks?.() || []) {
          try { track.stop(); } catch {}
        }
      }
      try { silenceSource?.stop(); } catch {}
      try { remotePlaybackSource?.disconnect(); } catch {}
      try { localAnalyser?.disconnect(); } catch {}
      try { remoteAnalyser?.disconnect(); } catch {}
      try { audioContext?.close(); } catch {}
      if (this.audioElement) {
        this.audioElement.pause?.();
        this.audioElement.srcObject = null;
      }
      // A closed peer has no reusable intermediate state. Publishing only the
      // terminal idle phase also prevents UI state callbacks from recursively
      // attempting to close the same already-cleared peer.
      this.setPhase("idle");
    }
  }

  root.MacCellularRemoteMediaClient = MacCellularRemoteMediaClient;
  if (typeof module !== "undefined" && module.exports) {
    module.exports = { MacCellularRemoteMediaClient, validateRelayCredentials, relayOnlySDP };
  }
})(globalThis);
