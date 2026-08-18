(function installCallRecorder(root) {
  "use strict";

  const mimeCandidates = [
    "audio/mp4;codecs=mp4a.40.2",
    "audio/mp4",
    "audio/webm;codecs=opus",
    "audio/webm",
  ];

  function preferredMime(MediaRecorderClass) {
    if (typeof MediaRecorderClass !== "function") return "";
    if (typeof MediaRecorderClass.isTypeSupported !== "function") return mimeCandidates[0];
    return mimeCandidates.find((mime) => MediaRecorderClass.isTypeSupported(mime)) || "";
  }

  function recordingExtension(mime) {
    return String(mime || "").startsWith("audio/mp4") ? "m4a" : "webm";
  }

  class MacCellularCallRecorder {
    constructor(options = {}) {
      this.MediaRecorderClass = options.MediaRecorderClass || root.MediaRecorder;
      const AudioContextClass = root.AudioContext || root.webkitAudioContext;
      this.audioContextFactory = options.audioContextFactory ||
        (typeof AudioContextClass === "function" ? () => new AudioContextClass() : null);
      this.now = options.now || (() => new Date());
      this.onState = options.onState || (() => {});
      this.recorder = null;
      this.context = null;
      this.destination = null;
      this.chunks = [];
      this.metadata = null;
      this.startedAt = null;
      this.stopPromise = null;
    }

    supported() {
      return typeof this.MediaRecorderClass === "function" &&
        typeof this.audioContextFactory === "function" && preferredMime(this.MediaRecorderClass) !== "";
    }

    active() { return Boolean(this.recorder && this.recorder.state !== "inactive"); }

    async start({ localStream, remoteStream, metadata = {} }) {
      if (this.recorder || this.stopPromise) throw new Error("通话录音已在进行");
      if (!this.supported()) throw new Error("当前浏览器不支持通话录音");
      if (!localStream?.getAudioTracks?.().length || !remoteStream?.getAudioTracks?.().length) {
        throw new Error("双向通话音频尚未就绪");
      }
      const context = this.audioContextFactory();
      const destination = context.createMediaStreamDestination();
      context.createMediaStreamSource(localStream).connect(destination);
      context.createMediaStreamSource(remoteStream).connect(destination);
      await context.resume();
      const mimeType = preferredMime(this.MediaRecorderClass);
      const recorder = new this.MediaRecorderClass(destination.stream, { mimeType });
      this.context = context;
      this.destination = destination;
      this.recorder = recorder;
      this.chunks = [];
      this.metadata = { ...metadata };
      this.startedAt = this.now();
      recorder.addEventListener("dataavailable", (event) => {
        if (event.data?.size) this.chunks.push(event.data);
      });
      // Safari/Chromium can emit fragmented MP4 timeslices whose later chunks
      // do not contain the initialization boxes required for standalone
      // playback. Keep MP4 as one complete recording; WebM chunks concatenate
      // safely and may still be emitted once per second.
      if (mimeType.startsWith("audio/mp4")) recorder.start();
      else recorder.start(1000);
      this.onState({ phase: "recording", started_at: this.startedAt.toISOString() });
    }

    async stop() {
      if (!this.recorder) return null;
      if (this.stopPromise) return this.stopPromise;
      const recorder = this.recorder;
      const context = this.context;
      const startedAt = this.startedAt;
      const metadata = this.metadata || {};
      const chunks = this.chunks;
      this.stopPromise = new Promise((resolve) => {
        recorder.addEventListener("stop", async () => {
          const endedAt = this.now();
          const mimeType = recorder.mimeType || preferredMime(this.MediaRecorderClass);
          const blob = new Blob(chunks, { type: mimeType });
          try { await context?.close?.(); } catch {}
          this.recorder = null;
          this.context = null;
          this.destination = null;
          this.chunks = [];
          this.metadata = null;
          this.startedAt = null;
          this.stopPromise = null;
          const result = {
            id: root.crypto?.randomUUID?.() || `${endedAt.getTime()}`,
            ...metadata,
            started_at: startedAt.toISOString(),
            ended_at: endedAt.toISOString(),
            duration_seconds: Math.max(0, Math.round((endedAt - startedAt) / 1000)),
            mime_type: mimeType,
            extension: recordingExtension(mimeType),
            blob,
          };
          this.onState({ phase: "saved", recording: result });
          resolve(result);
        }, { once: true });
        recorder.stop();
      });
      return this.stopPromise;
    }
  }

  class MacCellularRecordingStore {
    constructor(indexedDB = root.indexedDB) {
      this.indexedDB = indexedDB;
      this.dbPromise = null;
    }

    open() {
      if (this.dbPromise) return this.dbPromise;
      this.dbPromise = new Promise((resolve, reject) => {
        const request = this.indexedDB.open("maccellular-recordings", 1);
        request.addEventListener("upgradeneeded", () => request.result.createObjectStore("recordings", { keyPath: "id" }));
        request.addEventListener("success", () => resolve(request.result));
        request.addEventListener("error", () => reject(request.error || new Error("无法打开录音库")));
      });
      return this.dbPromise;
    }

    async save(recording) {
      const db = await this.open();
      return new Promise((resolve, reject) => {
        const transaction = db.transaction("recordings", "readwrite");
        transaction.objectStore("recordings").put(recording);
        transaction.addEventListener("complete", resolve);
        transaction.addEventListener("error", () => reject(transaction.error || new Error("录音保存失败")));
      });
    }

    async list() {
      const db = await this.open();
      return new Promise((resolve, reject) => {
        const request = db.transaction("recordings", "readonly").objectStore("recordings").getAll();
        request.addEventListener("success", () => resolve((request.result || []).sort((a, b) =>
          Date.parse(b.started_at) - Date.parse(a.started_at))));
        request.addEventListener("error", () => reject(request.error || new Error("录音读取失败")));
      });
    }

    async remove(id) {
      const db = await this.open();
      return new Promise((resolve, reject) => {
        const transaction = db.transaction("recordings", "readwrite");
        transaction.objectStore("recordings").delete(id);
        transaction.addEventListener("complete", resolve);
        transaction.addEventListener("error", () => reject(transaction.error || new Error("录音删除失败")));
      });
    }
  }

  root.MacCellularCallRecorder = MacCellularCallRecorder;
  root.MacCellularRecordingStore = MacCellularRecordingStore;
  root.MacCellularRecordingExtension = recordingExtension;
})(globalThis);
