"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

const app = readFileSync(join(__dirname, "app.js"), "utf8");

test("recordings prefer the Mac streaming URL and retain the phone fallback", () => {
  assert.match(app, /api\("\.\.\/api\/remote\/v1\/recordings"\)/);
  assert.match(app, /const url = item\.audio_path \|\| \(localBlob \? URL\.createObjectURL\(item\.blob\) : ""\)/);
  assert.match(app, /player\.playsInline = true/);
  assert.match(app, /录音由家中 Mac 加载/);
});

test("removing the phone copy leaves the Mac recording available", () => {
  assert.match(app, /if \(localBlob\) \{/);
  assert.match(app, /手机副本已删除，Mac 录音仍可播放/);
});

test("an ended call stays suppressed if the modem briefly replays the same call id", () => {
  const lifecycle = app.slice(app.indexOf("function observeCallLifecycle"), app.indexOf("function applySessionPresentation"));
  assert.match(lifecycle, /recordingSuppressedCallID = previous\.id/);
  assert.doesNotMatch(lifecycle, /recordingSuppressedCallID = ""/);
  assert.match(app, /recordingSuppressedCallID === call\.id/);
});

test("the recording list keeps the longest recording for each call id", () => {
  assert.match(app, /const primaryByCall = new Map\(\)/);
  assert.match(app, /item\.duration_seconds \|\| 0\) > \(current\.duration_seconds \|\| 0/);
});
