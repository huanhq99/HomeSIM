"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

const root = __dirname;
const app = readFileSync(join(root, "app.js"), "utf8");
const page = readFileSync(join(root, "index.html"), "utf8");
const style = readFileSync(join(root, "style.css"), "utf8");

test("call card exposes a three-stage visual journey and two real audio meters", () => {
  for (const marker of [
    'data-call-step="line"', 'data-call-step="media"', 'data-call-step="live"',
    'id="remote-audio-wave"', 'id="local-audio-wave"',
  ]) assert.match(page, new RegExp(marker));
  assert.match(app, /remoteMediaClient\?\.audioLevels\?\.\(\)/);
  assert.match(app, /levels\.remote > \.025/);
  assert.match(app, /callMuted \? 0 : levels\.local/);
});

test("direct call states select ringing, connecting, live and ending visuals", () => {
  assert.match(app, /tone: "ringing"/);
  assert.match(app, /tone: "connecting"/);
  assert.match(app, /tone: "live"/);
  assert.match(app, /tone: "ending"/);
  assert.match(app, /双向通话已连接/);
  assert.match(app, /通话已接通，正在连接音频/);
});

test("visual motion respects reduced-motion preference", () => {
  assert.match(style, /prefers-reduced-motion: reduce/);
  assert.match(style, /animation: none !important/);
});
