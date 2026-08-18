"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

class FakeRecorder extends EventTarget {
  static isTypeSupported(type) { return type === "audio/mp4"; }
  constructor(_stream, options) { super(); this.mimeType = options.mimeType; this.state = "inactive"; }
  start(timeslice) { this.timeslice = timeslice; this.state = "recording"; }
  stop() {
    this.state = "inactive";
    this.dispatchEvent(Object.assign(new Event("dataavailable"), { data: new Blob(["voice"]) }));
    this.dispatchEvent(new Event("stop"));
  }
}

function stream() { return { getAudioTracks: () => [{}] }; }
function context() {
  return {
    createMediaStreamDestination: () => ({ stream: {} }),
    createMediaStreamSource: () => ({ connect() {} }),
    resume: async () => {},
    close: async () => {},
  };
}

globalThis.MediaRecorder = FakeRecorder;
globalThis.AudioContext = function AudioContext() { return context(); };
require("./call-recorder.js");

test("manual recorder captures both streams and returns call metadata", async () => {
  let now = new Date("2026-08-16T12:00:00Z");
  const recorder = new globalThis.MacCellularCallRecorder({ MediaRecorderClass: FakeRecorder, audioContextFactory: context, now: () => now });
  await recorder.start({ localStream: stream(), remoteStream: stream(), metadata: { number: "10086", direction: "outgoing" } });
  assert.equal(recorder.active(), true);
  assert.equal(recorder.recorder.timeslice, undefined);
  now = new Date("2026-08-16T12:00:09Z");
  const result = await recorder.stop();
  assert.match(result.id, /^[0-9a-f-]{36}$/i);
  assert.equal(result.number, "10086");
  assert.equal(result.duration_seconds, 9);
  assert.equal(result.extension, "m4a");
  assert.equal(result.blob.size > 0, true);
});

test("WebM recording may retain periodic chunks", async () => {
  class FakeWebMRecorder extends FakeRecorder {
    static isTypeSupported(type) { return type === "audio/webm;codecs=opus"; }
  }
  const recorder = new globalThis.MacCellularCallRecorder({ MediaRecorderClass: FakeWebMRecorder, audioContextFactory: context });
  await recorder.start({ localStream: stream(), remoteStream: stream() });
  assert.equal(recorder.recorder.timeslice, 1000);
  await recorder.stop();
});

test("recorder refuses to start before both call directions exist", async () => {
  const recorder = new globalThis.MacCellularCallRecorder({ MediaRecorderClass: FakeRecorder, audioContextFactory: context });
  await assert.rejects(() => recorder.start({ localStream: stream(), remoteStream: null }), /双向通话音频尚未就绪/);
});
