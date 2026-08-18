"use strict";

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const {
  IncomingCallPushController,
  eligibleSession,
  endpointDigest,
  serializeSubscription,
  validateConfig,
} = require("./incoming-call-push.js");

const vapidPublicKey = Buffer.concat([Buffer.from([0x04]), Buffer.alloc(64, 0x31)]).toString("base64url");
const subscriptionID = "A".repeat(32);
const endpoint = "https://push.example.test/subscriptions/browser-a";

function publicVoiceSession(overrides = {}) {
  return {
    identity: "allowed@example.test",
    sms_only: false,
    transport: "cloudflare-access",
    public_voice: true,
    push_enabled: true,
    push_api_version: 1,
    notification_mode: "web-push",
    background_calls: false,
    ...overrides,
  };
}

function makeSubscription(overrides = {}) {
  let unsubscribeCalls = 0;
  const value = {
    endpoint,
    expirationTime: null,
    keys: { p256dh: "cDI1NmRo", auth: "YXV0aA" },
    ...overrides,
  };
  return {
    endpoint: value.endpoint,
    toJSON() { return value; },
    async unsubscribe() { unsubscribeCalls += 1; return true; },
    unsubscribeCalls() { return unsubscribeCalls; },
  };
}

function makeHarness(options = {}) {
  const requests = [];
  const renders = [];
  const messages = [];
  const subscription = options.subscription || null;
  let browserSubscription = subscription;
  let permissionRequests = 0;
  let subscribeCalls = 0;
  const registration = {
    pushManager: {
      async getSubscription() { return browserSubscription; },
      async subscribe(settings) {
        subscribeCalls += 1;
        assert.equal(settings.userVisibleOnly, true);
        assert.equal(settings.applicationServerKey.length, 65);
        browserSubscription = options.createdSubscription || makeSubscription();
        return browserSubscription;
      },
    },
  };
  const NotificationAPI = {
    permission: options.permission || "default",
    async requestPermission() {
      permissionRequests += 1;
      NotificationAPI.permission = options.permissionResult || "granted";
      return NotificationAPI.permission;
    },
  };
  const environment = {
    Notification: NotificationAPI,
    ...(options.pushManagerAvailable === false ? {} : { PushManager: function PushManager() {} }),
    crypto: crypto.webcrypto,
    TextEncoder,
    atob: (value) => Buffer.from(value, "base64").toString("binary"),
    btoa: (value) => Buffer.from(value, "binary").toString("base64"),
    navigator: {
      userAgent: options.userAgent || "Mozilla/5.0 (Linux; Android 15)",
      maxTouchPoints: options.maxTouchPoints || 0,
      standalone: options.standalone === true,
      serviceWorker: {
        async getRegistration(scope) {
          assert.equal(scope, "/remote/");
          return registration;
        },
      },
    },
    matchMedia(query) {
      assert.equal(query, "(display-mode: standalone)");
      return { matches: options.standalone === true };
    },
  };
  const api = async (url, requestOptions = {}) => {
    requests.push({ url, options: requestOptions });
    if (url === "/api/remote/v1/push/config") {
      return {
        version: 1,
        enabled: true,
        vapid_public_key: vapidPublicKey,
        ...(options.configSubscriptionID ? { subscription_id: options.configSubscriptionID } : {}),
      };
    }
    if (url === "/api/remote/v1/push/subscriptions" && requestOptions.method === "POST") {
      if (options.registrationError) throw new Error("registration_failed");
      return { version: 1, subscription_id: subscriptionID };
    }
    if (url === `/api/remote/v1/push/subscriptions/${subscriptionID}` && requestOptions.method === "DELETE") {
      return { version: 1 };
    }
    throw new Error(`unexpected request ${requestOptions.method || "GET"} ${url}`);
  };
  const controller = new IncomingCallPushController({
    environment,
    api,
    render: (state) => renders.push(state),
    onMessage: (message) => messages.push(message),
  });
  return {
    controller,
    environment,
    messages,
    permissionRequests: () => permissionRequests,
    renders,
    requests,
    subscribeCalls: () => subscribeCalls,
    subscription: () => browserSubscription,
  };
}

test("push control is eligible only for an exact public voice push-v1 session", () => {
  assert.equal(eligibleSession(publicVoiceSession()), true);
  assert.equal(eligibleSession(publicVoiceSession({ transport: " Cloudflare-Access " })), true);
  for (const override of [
    { sms_only: true },
    { transport: "tailscale-serve" },
    { public_voice: false },
    { public_voice: "true" },
    { push_enabled: false },
    { push_api_version: 2 },
    { notification_mode: "foreground-only" },
    { background_calls: true },
  ]) {
    assert.equal(eligibleSession(publicVoiceSession(override)), false, JSON.stringify(override));
  }
});

test("session discovery is read-only and restores only an opaque id via endpoint digest", async () => {
  const subscription = makeSubscription();
  const harness = makeHarness({ subscription, configSubscriptionID: subscriptionID, permission: "granted" });
  await harness.controller.applySession(publicVoiceSession());

  assert.equal(harness.permissionRequests(), 0);
  assert.equal(harness.subscribeCalls(), 0);
  assert.equal(harness.requests.length, 1);
  assert.equal(harness.requests[0].url, "/api/remote/v1/push/config");
  assert.equal(
    harness.requests[0].options.headers["X-MacCellular-Push-Endpoint-SHA256"],
    crypto.createHash("sha256").update(endpoint, "utf8").digest("base64url"),
  );
  assert.equal(harness.renders.at(-1).visible, true);
  assert.equal(harness.renders.at(-1).enabled, true);
  assert.equal(harness.renders.at(-1).label, "关闭来电提醒");
  assert.equal(harness.controller.enabled(), true);
});

test("only a user toggle requests permission, subscribes, and posts the narrow v1 body", async () => {
  const harness = makeHarness();
  await harness.controller.applySession(publicVoiceSession());
  assert.equal(harness.permissionRequests(), 0);
  assert.equal(harness.subscribeCalls(), 0);

  assert.equal(await harness.controller.toggle(), true);
  assert.equal(harness.permissionRequests(), 1);
  assert.equal(harness.subscribeCalls(), 1);
  assert.deepEqual(harness.requests.map((request) => request.url), [
    "/api/remote/v1/push/config",
    "/api/remote/v1/push/subscriptions",
  ]);
  const registration = harness.requests[1];
  assert.equal(registration.options.method, "POST");
  assert.equal(registration.options.ephemeral, true);
  assert.deepEqual(JSON.parse(registration.options.body), {
    version: 1,
    subscription: {
      endpoint,
      expirationTime: null,
      keys: { p256dh: "cDI1NmRo", auth: "YXV0aA" },
    },
  });
  assert.equal(harness.renders.at(-1).enabled, true);
  assert.equal(harness.controller.enabled(), true);
  assert.equal(harness.messages.at(-1), "来电提醒已开启");
});

test("disable deletes the opaque id with a JSON body before browser unsubscribe", async () => {
  const subscription = makeSubscription();
  const harness = makeHarness({ subscription, configSubscriptionID: subscriptionID, permission: "granted" });
  await harness.controller.applySession(publicVoiceSession());
  assert.equal(await harness.controller.toggle(), true);

  const deletion = harness.requests.at(-1);
  assert.equal(deletion.url, `/api/remote/v1/push/subscriptions/${subscriptionID}`);
  assert.equal(deletion.options.method, "DELETE");
  assert.equal(deletion.options.body, "{}");
  assert.equal(deletion.options.ephemeral, true);
  assert.equal(subscription.unsubscribeCalls(), 1);
  assert.equal(harness.renders.at(-1).enabled, false);
  assert.equal(harness.controller.enabled(), false);
  assert.equal(harness.messages.at(-1), "来电提醒已关闭");
});

test("iOS requires a home-screen install before permission or subscribe is attempted", async () => {
  const harness = makeHarness({
    userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)",
    standalone: false,
    pushManagerAvailable: false,
  });
  await harness.controller.applySession(publicVoiceSession());
  assert.match(harness.renders.at(-1).message, /Safari.*主屏幕/);
  assert.equal(await harness.controller.toggle(), false);
  assert.equal(harness.permissionRequests(), 0);
  assert.equal(harness.subscribeCalls(), 0);
  assert.match(harness.messages.at(-1), /Safari.*主屏幕/);
});

test("denied permission gives an actionable Chinese error and does not subscribe", async () => {
  const harness = makeHarness({ permissionResult: "denied" });
  await harness.controller.applySession(publicVoiceSession());
  assert.equal(await harness.controller.toggle(), false);
  assert.equal(harness.permissionRequests(), 1);
  assert.equal(harness.subscribeCalls(), 0);
  assert.match(harness.messages.at(-1), /系统设置/);
});

test("hiding the page while permission is open prevents subscribe and registration", async () => {
  const harness = makeHarness();
  await harness.controller.applySession(publicVoiceSession());
  let resolvePermission;
  harness.environment.Notification.requestPermission = () => new Promise((resolve) => {
    resolvePermission = resolve;
  });

  const pending = harness.controller.toggle();
  await Promise.resolve();
  harness.controller.clear();
  resolvePermission("granted");
  assert.equal(await pending, false);
  assert.equal(harness.subscribeCalls(), 0);
  assert.deepEqual(harness.requests.map((request) => request.url), ["/api/remote/v1/push/config"]);
  assert.equal(harness.renders.at(-1).visible, false);
  assert.deepEqual(harness.messages, []);
});

test("a session loss while POST is in flight cannot adopt the returned opaque id", async () => {
  const harness = makeHarness();
  await harness.controller.applySession(publicVoiceSession());
  const baseAPI = harness.controller.api;
  let resolveRegistration;
  let registrationStarted;
  const started = new Promise((resolve) => { registrationStarted = resolve; });
  harness.controller.api = (url, options) => {
    if (url === "/api/remote/v1/push/subscriptions") {
      registrationStarted();
      return new Promise((resolve) => { resolveRegistration = resolve; });
    }
    return baseAPI(url, options);
  };

  const pending = harness.controller.toggle();
  await started;
  harness.controller.clear();
  resolveRegistration({ version: 1, subscription_id: subscriptionID });
  assert.equal(await pending, false);
  assert.equal(harness.controller.subscription, null);
  assert.equal(harness.controller.subscriptionID, "");
  assert.equal(harness.renders.at(-1).visible, false);
  assert.deepEqual(harness.messages, []);
});

test("an ambiguous registration keeps the browser subscription for hash-based recovery", async () => {
  const created = makeSubscription();
  const harness = makeHarness({ createdSubscription: created, registrationError: true });
  await harness.controller.applySession(publicVoiceSession());
  assert.equal(await harness.controller.toggle(), false);
  assert.equal(created.unsubscribeCalls(), 0);
  assert.equal(harness.subscription(), created);
  assert.equal(harness.renders.at(-1).enabled, false);
});

test("subscription serialization and VAPID config validation are narrow", async () => {
  assert.deepEqual(serializeSubscription(makeSubscription()), {
    endpoint,
    expirationTime: null,
    keys: { p256dh: "cDI1NmRo", auth: "YXV0aA" },
  });
  assert.equal(validateConfig({ version: 1, enabled: true, vapid_public_key: vapidPublicKey }, {
    atob: (value) => Buffer.from(value, "base64").toString("binary"),
  }).applicationServerKey.length, 65);
  await assert.rejects(
    endpointDigest({ endpoint: "" }, { crypto: crypto.webcrypto, TextEncoder, btoa }),
    /push_endpoint_unavailable/,
  );
  assert.throws(
    () => serializeSubscription(makeSubscription({ endpoint: `https://push.example/${"a".repeat(2049)}` })),
    /invalid_browser_push_subscription/,
  );
  assert.throws(
    () => validateConfig({
      version: 1,
      enabled: true,
      vapid_public_key: vapidPublicKey,
      subscription_id: "A".repeat(31),
    }, { atob: (value) => Buffer.from(value, "base64").toString("binary") }),
    /invalid_push_subscription_id/,
  );
});

test("push code keeps credentials out of page storage and cannot answer in the background", () => {
  const client = fs.readFileSync(path.join(__dirname, "incoming-call-push.js"), "utf8");
  const worker = fs.readFileSync(path.join(__dirname, "service-worker.js"), "utf8");
  assert.doesNotMatch(client, /localStorage|sessionStorage|indexedDB|document\.cookie/);
  assert.doesNotMatch(worker, /localStorage|sessionStorage|indexedDB|document\.cookie/);
  assert.doesNotMatch(worker, /getUserMedia|mediaDevices|calls\/answer|microphone|麦克风/);
  assert.match(worker, /payload\.version !== 1 \|\| payload\.type !== "incoming_call"/);
  assert.match(worker, /showNotification\("MacCellular 来电"/);
  assert.match(worker, /body: "打开 MacCellular 查看并处理"/);
  assert.doesNotMatch(worker, /payload\.(?:title|body|number|phone)/);
  assert.match(worker, /clients\.openWindow\("\/remote\/"\)/);
});

test("service worker accepts only the fixed v1 incoming-call payload and uses fixed copy", async () => {
  const source = fs.readFileSync(path.join(__dirname, "service-worker.js"), "utf8");
  const handlers = new Map();
  const shown = [];
  const context = {
    URL,
    caches: {
      async open() { return { async addAll() {}, async put() {} }; },
      async keys() { return []; },
      async delete() {},
      async match() { return null; },
    },
    clients: { async matchAll() { return []; }, async openWindow() {} },
    fetch: async () => ({ ok: false }),
    self: {
      location: { origin: "https://phone.example.com" },
      registration: { async showNotification(title, options) { shown.push({ title, options }); } },
      addEventListener(name, handler) { handlers.set(name, handler); },
    },
  };
  vm.runInNewContext(source, context, { filename: "service-worker.js" });
  async function dispatchPush(payload) {
    let pending;
    handlers.get("push")({
      data: { json: () => payload },
      waitUntil(value) { pending = value; },
    });
    await pending;
  }

  await dispatchPush({ version: 1, type: "incoming_call", phone: "+15555550123" });
  await dispatchPush({ version: 2, type: "incoming_call" });
  await dispatchPush({ version: 1, type: "incoming_call" });
  assert.deepEqual(JSON.parse(JSON.stringify(shown)), [{
    title: "MacCellular 来电",
    options: {
      body: "打开 MacCellular 查看并处理",
      tag: "maccellular-incoming-call-v1",
      icon: "icon-192.png",
      data: { version: 1 },
    },
  }]);
});

test("service worker immediately activates so an existing installed PWA can receive push", async () => {
  const source = fs.readFileSync(path.join(__dirname, "service-worker.js"), "utf8");
  const handlers = new Map();
  let skipWaitingCalls = 0;
  let claimCalls = 0;
  const context = {
    URL,
    caches: {
      async open() { return { async addAll() {} }; },
      async keys() { return ["old-cache"]; },
      async delete() { return true; },
    },
    clients: {
      async claim() { claimCalls += 1; },
      async matchAll() { return []; },
      async openWindow() {},
    },
    fetch: async () => ({ ok: false }),
    self: {
      location: { origin: "https://phone.example.com" },
      registration: { async showNotification() {} },
      async skipWaiting() { skipWaitingCalls += 1; },
      addEventListener(name, handler) { handlers.set(name, handler); },
    },
  };
  vm.runInNewContext(source, context, { filename: "service-worker.js" });

  for (const eventName of ["install", "activate"]) {
    let pending;
    handlers.get(eventName)({ waitUntil(value) { pending = value; } });
    await pending;
  }
  assert.equal(skipWaitingCalls, 1);
  assert.equal(claimCalls, 1);
  assert.doesNotMatch(source, /client\.navigate\(/);
});

test("cache and page include the same versioned push client", () => {
  const index = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  const worker = fs.readFileSync(path.join(__dirname, "service-worker.js"), "utf8");
  const app = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
  const asset = "incoming-call-push.js?v=20260815-remote1";
  assert.match(index, new RegExp(asset.replace(/[.?]/g, "\\$&")));
  assert.match(worker, new RegExp(asset.replace(/[.?]/g, "\\$&")));
  assert.match(worker, /maccellular-remote-v63/);
  assert.match(index, /app\.js\?v=20260817-remote39/);
  assert.match(worker, /app\.js\?v=20260817-remote39/);
  assert.doesNotMatch(app, /serviceWorker\.addEventListener\("controllerchange"/);
  assert.match(app, /window\.addEventListener\("pageshow"/);
  assert.match(app, /refreshAbortController\?\.abort\(\)/);
  assert.match(app, /#refresh"\)\.addEventListener\("click", \(\) => refresh\(\{ force: true \}\)\)/);
});
