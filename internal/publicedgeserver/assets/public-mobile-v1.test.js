"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const application = require("./public-mobile-v1.js");

function snapshot(overrides) {
  const value = {
    version: 1,
    gateway_online: true,
    received_at: 1755200000,
    snapshot: {
      observed_at: 1755200000,
      revision: 7,
      event_high_water: 0,
      service: "ready",
      call: "idle",
      sms: "ready"
    }
  };
  return JSON.stringify(Object.assign(value, overrides || {}));
}

test("strict parser accepts only the version-one coarse schema", () => {
  const parsed = application.parseSnapshotText(snapshot());
  assert.deepEqual(parsed, {
    version: 1,
    gateway_online: true,
    received_at: 1755200000,
    snapshot: {
      observed_at: 1755200000,
      revision: 7,
      event_high_water: 0,
      service: "ready",
      call: "idle",
      sms: "ready"
    }
  });
  assert.equal(Object.isFrozen(parsed), true);
  assert.equal(Object.isFrozen(parsed.snapshot), true);

  const empty = application.parseSnapshotText(JSON.stringify({
    version: 1,
    gateway_online: false,
    received_at: 0,
    snapshot: null
  }));
  assert.equal(empty.snapshot, null);
});

test("strict parser rejects duplicate and escaped-duplicate keys", () => {
  assert.throws(() => application.parseSnapshotText(
    '{"version":1,"version":1,"gateway_online":false,"received_at":0,"snapshot":null}'
  ), /schema/);
  assert.throws(() => application.parseSnapshotText(
    '{"version":1,"gateway_online":false,"received_at":0,"snapshot":{' +
    '"observed_at":1,"revision":1,"event_high_water":0,"service":"ready",' +
    '"call":"idle","c\\u0061ll":"idle","sms":"ready"}}'
  ), /schema/);
});

test("strict parser rejects extensions, invalid enums, unsafe counters, and inconsistent state", () => {
  const validObject = JSON.parse(snapshot());
  const cases = [
    Object.assign({}, validObject, {extra: true}),
    Object.assign({}, validObject, {version: 2}),
    Object.assign({}, validObject, {gateway_online: "true"}),
    Object.assign({}, validObject, {received_at: Number.MAX_SAFE_INTEGER + 1}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {extra: true})}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {revision: 0})}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {revision: 1.5})}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {service: "executing"})}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {call: "dialing"})}),
    Object.assign({}, validObject, {snapshot: Object.assign({}, validObject.snapshot, {sms: "message"})}),
    {version: 1, gateway_online: true, received_at: 0, snapshot: null},
    {version: 1, gateway_online: false, received_at: 1, snapshot: null},
    {version: 1, gateway_online: false, received_at: 0, snapshot: validObject.snapshot}
  ];
  for (const candidate of cases) {
    assert.throws(() => application.parseSnapshotText(JSON.stringify(candidate)), /schema/);
  }
  assert.throws(() => application.parseSnapshotText("[]"), /schema/);
  assert.throws(() => application.parseSnapshotText("{} trailing"), /schema/);
  assert.throws(() => application.parseSnapshotText(" ".repeat(16385)), /schema/);
});

test("view model visibly separates online, stale, and never-seen states", () => {
  const online = application.parseSnapshotText(snapshot());
  assert.deepEqual(application.toViewModel(online, 1755200005000), {
    tone: "online",
    title: "网关在线",
    badge: "在线",
    detail: "已收到网关的最新粗粒度状态。",
    received: "刚刚",
    revision: "第 7 版",
    service: "运行正常",
    call: "空闲",
    sms: "可用"
  });

  const stale = application.parseSnapshotText(snapshot({gateway_online: false}));
  const staleView = application.toViewModel(stale, 1755200120000);
  assert.equal(staleView.tone, "stale");
  assert.equal(staleView.title, "网关已离线");
  assert.equal(staleView.badge, "过期状态");
  assert.equal(staleView.received, "2 分钟前");

  const empty = application.parseSnapshotText(JSON.stringify({
    version: 1,
    gateway_online: false,
    received_at: 0,
    snapshot: null
  }));
  const emptyView = application.toViewModel(empty, 1755200120000);
  assert.equal(emptyView.title, "尚未连接");
  assert.equal(emptyView.service, "—");
});

test("polling interval and failure backoff stay within fixed bounds", () => {
  assert.equal(application.nextPollDelay(0, 0), 5000);
  assert.equal(application.nextPollDelay(1, 0), 8000);
  assert.equal(application.nextPollDelay(1, 1), 10000);
  assert.equal(application.nextPollDelay(2, 1), 20000);
  assert.equal(application.nextPollDelay(3, 1), 40000);
  assert.equal(application.nextPollDelay(4, 1), 60000);
  assert.equal(application.nextPollDelay(100, 1), 60000);
  assert.throws(() => application.nextPollDelay(-1, 0), /failure count/);
  assert.throws(() => application.nextPollDelay(1, 2), /jitter/);
});

test("HTTP classification keeps Access unauthorized and unavailable distinct", () => {
  assert.equal(application.classifyHTTPResponse(200, "basic"), "ok");
  assert.equal(application.classifyHTTPResponse(401, "basic"), "unauthorized");
  assert.equal(application.classifyHTTPResponse(0, "opaqueredirect"), "unauthorized");
  assert.equal(application.classifyHTTPResponse(503, "basic"), "unavailable");
  assert.equal(application.classifyHTTPResponse(500, "basic"), "invalid");
  assert.throws(() => application.classifyHTTPResponse(700, "basic"), /metadata/);
});

test("moving the page to background aborts polling and clears rendered state", async () => {
  class Element {
    constructor() {
      this.textContent = "";
      this.attributes = Object.create(null);
    }
    setAttribute(name, value) {
      this.attributes[name] = value;
    }
  }

  const ids = [
    "connection-title", "status-badge", "status-badge-text", "status-detail",
    "received-at", "revision", "service-state", "call-state", "sms-state"
  ];
  const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
  const documentListeners = Object.create(null);
  const documentObject = {
    visibilityState: "visible",
    getElementById(id) {
      return elements[id] || null;
    },
    addEventListener(name, listener) {
      documentListeners[name] = listener;
    }
  };
  const windowListeners = Object.create(null);
  let requestPath;
  let requestOptions;
  const windowObject = {
    location: {
      href: "https://phone.example.com/",
      origin: "https://phone.example.com"
    },
    AbortController,
    URL,
    TextDecoder,
    TextEncoder,
    setTimeout,
    clearTimeout,
    addEventListener(name, listener) {
      windowListeners[name] = listener;
    },
    fetch(path, options) {
      requestPath = path;
      requestOptions = options;
      return new Promise((_resolve, reject) => {
        options.signal.addEventListener("abort", () => {
          const error = new Error("aborted");
          error.name = "AbortError";
          reject(error);
        }, {once: true});
      });
    }
  };

  application.start(documentObject, windowObject);
  assert.equal(requestPath, "/api/public/v1/state/snapshot");
  assert.equal(requestOptions.cache, "no-store");
  assert.equal(requestOptions.credentials, "same-origin");
  assert.equal(requestOptions.redirect, "manual");
  assert.equal(elements["connection-title"].textContent, "正在刷新");

  documentObject.visibilityState = "hidden";
  documentListeners.visibilitychange();
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(requestOptions.signal.aborted, true);
  assert.equal(elements["connection-title"].textContent, "状态已隐藏");
  assert.equal(elements["status-badge-text"].textContent, "已清除");
  assert.equal(elements["service-state"].textContent, "—");
  assert.equal(elements["call-state"].textContent, "—");
  assert.equal(elements["sms-state"].textContent, "—");
  assert.equal(typeof windowListeners.pagehide, "function");
  assert.equal(typeof windowListeners.pageshow, "function");
});

test("invalid response metadata aborts the unread response body", async () => {
  class Element {
    constructor() {
      this.textContent = "";
      this.attributes = Object.create(null);
    }
    setAttribute(name, value) {
      this.attributes[name] = value;
    }
  }

  const ids = [
    "connection-title", "status-badge", "status-badge-text", "status-detail",
    "received-at", "revision", "service-state", "call-state", "sms-state"
  ];
  const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
  const documentObject = {
    visibilityState: "visible",
    getElementById(id) {
      return elements[id] || null;
    },
    addEventListener() {}
  };
  const windowListeners = Object.create(null);
  let requestSignal;
  let bodyRead = false;
  const windowObject = {
    location: {
      href: "https://phone.example.com/",
      origin: "https://phone.example.com"
    },
    AbortController,
    URL,
    TextDecoder,
    TextEncoder,
    setTimeout,
    clearTimeout,
    addEventListener(name, listener) {
      windowListeners[name] = listener;
    },
    async fetch(_path, options) {
      requestSignal = options.signal;
      return {
        status: 500,
        type: "basic",
        url: "https://phone.example.com/api/public/v1/state/snapshot",
        headers: {get() { return null; }},
        body: {getReader() { bodyRead = true; throw new Error("must not read"); }}
      };
    }
  };

  application.start(documentObject, windowObject);
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(requestSignal.aborted, true);
  assert.equal(bodyRead, false);
  assert.equal(elements["connection-title"].textContent, "状态数据无效");
  windowListeners.pagehide();
});
