(function installIncomingCallPush(root) {
  "use strict";

  const configPath = "/api/remote/v1/push/config";
  const subscriptionsPath = "/api/remote/v1/push/subscriptions";
  const schemaVersion = 1;
  const endpointDigestHeader = "X-MacCellular-Push-Endpoint-SHA256";
  const opaqueIDPattern = /^[A-Za-z0-9_-]{32}$/;
  const base64URLPattern = /^[A-Za-z0-9_-]+$/;

  function eligibleSession(session) {
    const transport = typeof session?.transport === "string"
      ? session.transport.trim().toLowerCase()
      : "";
    return Boolean(session?.sms_only === false && transport === "cloudflare-access" &&
      session?.public_voice === true && session?.push_enabled === true &&
      session?.push_api_version === schemaVersion && session?.notification_mode === "web-push" &&
      session?.background_calls === false);
  }

  function appleMobileBrowser(environment = root) {
    const navigator = environment?.navigator;
    const userAgent = typeof navigator?.userAgent === "string" ? navigator.userAgent : "";
    return /(?:iPhone|iPad|iPod)/.test(userAgent) ||
      (/Macintosh/.test(userAgent) && Number(navigator?.maxTouchPoints || 0) > 1);
  }

  function installedWebApp(environment = root) {
    const navigator = environment?.navigator;
    if (navigator?.standalone === true) return true;
    try {
      return environment?.matchMedia?.("(display-mode: standalone)")?.matches === true;
    } catch {
      return false;
    }
  }

  function decodeBase64URL(value, environment = root) {
    if (typeof value !== "string" || !base64URLPattern.test(value)) {
      throw new Error("invalid_base64url");
    }
    const padding = "=".repeat((4 - (value.length % 4)) % 4);
    const encoded = value.replace(/-/g, "+").replace(/_/g, "/") + padding;
    const atobImpl = environment?.atob || root.atob;
    if (typeof atobImpl !== "function") throw new Error("base64_unavailable");
    let binary;
    try {
      binary = atobImpl.call(environment, encoded);
    } catch {
      throw new Error("invalid_base64url");
    }
    return Uint8Array.from(binary, (character) => character.charCodeAt(0));
  }

  function vapidApplicationServerKey(value, environment = root) {
    const key = decodeBase64URL(value, environment);
    if (key.length !== 65 || key[0] !== 0x04) throw new Error("invalid_vapid_public_key");
    return key;
  }

  function encodeBase64URL(bytes, environment = root) {
    const btoaImpl = environment?.btoa || root.btoa;
    if (typeof btoaImpl !== "function") throw new Error("base64_unavailable");
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoaImpl.call(environment, binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  }

  async function endpointDigest(subscription, environment = root) {
    const endpoint = subscription?.endpoint;
    const subtle = environment?.crypto?.subtle;
    const TextEncoderAPI = environment?.TextEncoder || root.TextEncoder;
    if (typeof endpoint !== "string" || endpoint.length === 0 || endpoint.length > 2048 ||
        !subtle || typeof subtle.digest !== "function" || typeof TextEncoderAPI !== "function") {
      throw new Error("push_endpoint_unavailable");
    }
    const digest = await subtle.digest("SHA-256", new TextEncoderAPI().encode(endpoint));
    return encodeBase64URL(new Uint8Array(digest), environment);
  }

  function validOpaqueID(value) {
    return typeof value === "string" && opaqueIDPattern.test(value);
  }

  function validateConfig(value, environment = root) {
    if (!value || value.version !== schemaVersion || value.enabled !== true ||
        typeof value.vapid_public_key !== "string") {
      throw new Error("invalid_push_config");
    }
    const applicationServerKey = vapidApplicationServerKey(value.vapid_public_key, environment);
    if (value.subscription_id !== undefined && !validOpaqueID(value.subscription_id)) {
      throw new Error("invalid_push_subscription_id");
    }
    return Object.freeze({
      applicationServerKey,
      subscriptionID: value.subscription_id || "",
    });
  }

  function validateRegistration(value) {
    if (!value || value.version !== schemaVersion || !validOpaqueID(value.subscription_id)) {
      throw new Error("invalid_push_registration");
    }
    return value.subscription_id;
  }

  function serializeSubscription(subscription) {
    const value = subscription?.toJSON?.();
    const endpoint = value?.endpoint;
    const expirationTime = value?.expirationTime;
    const p256dh = value?.keys?.p256dh;
    const auth = value?.keys?.auth;
    if (typeof endpoint !== "string" || endpoint.length === 0 || endpoint.length > 2048 ||
        (expirationTime !== null && expirationTime !== undefined && !Number.isFinite(expirationTime)) ||
        typeof p256dh !== "string" || !base64URLPattern.test(p256dh) ||
        typeof auth !== "string" || !base64URLPattern.test(auth)) {
      throw new Error("invalid_browser_push_subscription");
    }
    return Object.freeze({
      endpoint,
      expirationTime: expirationTime ?? null,
      keys: Object.freeze({ p256dh, auth }),
    });
  }

  function supportAvailable(environment = root) {
    return Boolean(environment?.Notification && environment?.navigator?.serviceWorker &&
      environment?.PushManager);
  }

  function messageForError(error) {
    const code = error?.message || "";
    if (code === "ios_install_required") {
      return "请先用 Safari 打开本页，点“分享”→“添加到主屏幕”，再从主屏幕打开并开启提醒。";
    }
    if (code === "notification_denied") {
      return "通知权限已关闭，请到系统设置中允许“MacCellular”通知后再试。";
    }
    if (code === "push_unsupported") {
      return "当前浏览器不支持后台来电提醒，请升级系统或改用支持 Web Push 的浏览器。";
    }
    return "来电提醒暂时无法配置，请检查网络后重试。";
  }

  class IncomingCallPushController {
    constructor(options = {}) {
      this.environment = options.environment || root;
      this.api = options.api;
      this.render = typeof options.render === "function" ? options.render : () => {};
      this.onMessage = typeof options.onMessage === "function" ? options.onMessage : () => {};
      this.session = null;
      this.sessionKey = "";
      this.registration = null;
      this.subscription = null;
      this.subscriptionID = "";
      this.applicationServerKey = null;
      this.loading = null;
      this.busy = false;
      this.state = "hidden";
      this.generation = 0;
      this.renderState();
    }

    renderState(detail = "") {
      const visible = eligibleSession(this.session);
      const enabled = visible && this.state === "enabled";
      const copy = {
        hidden: "",
        loading: "正在核对这台设备的来电提醒…",
        off: "收到来电时会显示通知；接听和麦克风仍需回到本页面。",
        enabled: "后台来电提醒已开启；接听和麦克风仍需回到本页面。",
        ios_install_required: "iPhone/iPad 请用 Safari 点“分享”→“添加到主屏幕”，再开启提醒。",
        denied: "通知权限已关闭，请到系统设置中允许“MacCellular”通知。",
        unsupported: "当前浏览器不支持后台来电提醒。",
        error: "来电提醒暂时不可用，可点按钮重试。",
      };
      this.render(Object.freeze({
        visible,
        enabled,
        busy: this.busy || this.state === "loading",
        label: enabled ? "关闭来电提醒" : "开启来电提醒",
        message: detail || copy[this.state] || copy.error,
      }));
    }

    enabled() {
      return Boolean(eligibleSession(this.session) && this.state === "enabled" &&
        this.subscription && validOpaqueID(this.subscriptionID));
    }

    clear() {
      this.generation += 1;
      this.session = null;
      this.sessionKey = "";
      this.registration = null;
      this.subscription = null;
      this.subscriptionID = "";
      this.applicationServerKey = null;
      this.loading = null;
      this.busy = false;
      this.state = "hidden";
      this.renderState();
    }

    applySession(session) {
      if (!eligibleSession(session)) {
        this.clear();
        return Promise.resolve(false);
      }
      const nextKey = JSON.stringify([
        session.identity || "",
        session.transport,
        session.public_voice,
        session.push_enabled,
        session.push_api_version,
        session.notification_mode,
        session.background_calls,
      ]);
      this.session = session;
      if (nextKey === this.sessionKey && this.loading) return this.loading;
      if (nextKey === this.sessionKey && this.state !== "hidden") {
        this.renderState();
        return Promise.resolve(true);
      }
      this.sessionKey = nextKey;
      this.generation += 1;
      this.registration = null;
      this.subscription = null;
      this.subscriptionID = "";
      this.applicationServerKey = null;
      this.state = "loading";
      this.renderState();
      const loading = this.load().finally(() => {
        if (this.loading === loading) this.loading = null;
      });
      this.loading = loading;
      return loading;
    }

    async serviceWorkerRegistration() {
      const serviceWorker = this.environment?.navigator?.serviceWorker;
      if (!serviceWorker || typeof serviceWorker.getRegistration !== "function") {
        throw new Error("push_unsupported");
      }
      const registration = await serviceWorker.getRegistration("/remote/");
      if (!registration?.pushManager || typeof registration.pushManager.getSubscription !== "function") {
        throw new Error("push_unsupported");
      }
      return registration;
    }

    requireCurrent(generation) {
      if (generation !== this.generation || !eligibleSession(this.session)) {
        throw new Error("push_session_changed");
      }
    }

    async load() {
      const key = this.sessionKey;
      const generation = this.generation;
      try {
        if (appleMobileBrowser(this.environment) && !installedWebApp(this.environment)) {
          throw new Error("ios_install_required");
        }
        if (!supportAvailable(this.environment)) throw new Error("push_unsupported");
        const registration = await this.serviceWorkerRegistration();
        this.requireCurrent(generation);
        const subscription = await registration.pushManager.getSubscription();
        this.requireCurrent(generation);
        const headers = {};
        if (subscription) headers[endpointDigestHeader] = await endpointDigest(subscription, this.environment);
        this.requireCurrent(generation);
        const config = validateConfig(await this.api(configPath, { headers }), this.environment);
        if (generation !== this.generation || key !== this.sessionKey || !eligibleSession(this.session)) return false;
        this.registration = registration;
        this.subscription = subscription || null;
        this.applicationServerKey = config.applicationServerKey;
        this.subscriptionID = subscription ? config.subscriptionID : "";
        if (subscription && this.subscriptionID) {
          this.state = "enabled";
        } else if (this.environment.Notification.permission === "denied") {
          this.state = "denied";
        } else if (appleMobileBrowser(this.environment) && !installedWebApp(this.environment)) {
          this.state = "ios_install_required";
        } else {
          this.state = "off";
        }
        this.renderState();
        return true;
      } catch (error) {
        if (generation !== this.generation || key !== this.sessionKey || !eligibleSession(this.session)) return false;
        if (error?.message === "ios_install_required") this.state = "ios_install_required";
        else if (error?.message === "push_unsupported") this.state = "unsupported";
        else this.state = "error";
        this.renderState(messageForError(error));
        return false;
      }
    }

    async enable(generation) {
      this.requireCurrent(generation);
      if (appleMobileBrowser(this.environment) && !installedWebApp(this.environment)) {
        throw new Error("ios_install_required");
      }
      if (!supportAvailable(this.environment)) throw new Error("push_unsupported");
      const permission = await this.environment.Notification.requestPermission();
      this.requireCurrent(generation);
      if (permission !== "granted") throw new Error("notification_denied");
      if (!this.registration || !this.applicationServerKey) await this.load();
      this.requireCurrent(generation);
      if (!this.registration || !this.applicationServerKey) throw new Error("push_config_unavailable");
      let subscription = await this.registration.pushManager.getSubscription();
      this.requireCurrent(generation);
      if (!subscription) {
        subscription = await this.registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: this.applicationServerKey,
        });
        this.requireCurrent(generation);
      }
      try {
        const response = await this.api(subscriptionsPath, {
          method: "POST",
          body: JSON.stringify({ version: schemaVersion, subscription: serializeSubscription(subscription) }),
          ephemeral: true,
        });
        this.requireCurrent(generation);
        this.subscriptionID = validateRegistration(response);
        this.subscription = subscription;
        this.state = "enabled";
      } catch (error) {
        // Keep an ambiguous newly-created subscription inside PushManager.
        // A retry can hash and upsert the same endpoint, or recover its opaque
        // id when the server committed the first POST but the response was lost.
        if (generation === this.generation && eligibleSession(this.session)) {
          this.subscription = subscription;
        }
        throw error;
      }
    }

    async disable(generation) {
      this.requireCurrent(generation);
      if (!this.subscription || !validOpaqueID(this.subscriptionID)) {
        throw new Error("push_subscription_unavailable");
      }
      const subscription = this.subscription;
      const subscriptionID = this.subscriptionID;
      await this.api(`${subscriptionsPath}/${encodeURIComponent(subscriptionID)}`, {
        method: "DELETE",
        body: "{}",
        ephemeral: true,
      });
      try { await subscription.unsubscribe(); } catch {}
      this.requireCurrent(generation);
      this.subscription = null;
      this.subscriptionID = "";
      this.state = "off";
    }

    async toggle() {
      if (!eligibleSession(this.session) || this.busy) return false;
      const generation = this.generation;
      this.busy = true;
      this.renderState();
      try {
        if (this.state === "enabled") await this.disable(generation);
        else await this.enable(generation);
        this.requireCurrent(generation);
        this.renderState();
        this.onMessage(this.state === "enabled" ? "来电提醒已开启" : "来电提醒已关闭");
        return true;
      } catch (error) {
        if (generation !== this.generation || !eligibleSession(this.session)) return false;
        if (error?.message === "notification_denied") this.state = "denied";
        else if (error?.message === "ios_install_required") this.state = "ios_install_required";
        else if (error?.message === "push_unsupported") this.state = "unsupported";
        else this.state = "error";
        const message = messageForError(error);
        this.renderState(message);
        this.onMessage(message);
        return false;
      } finally {
        if (generation === this.generation) {
          this.busy = false;
          this.renderState();
        }
      }
    }
  }

  const exported = Object.freeze({
    IncomingCallPushController,
    appleMobileBrowser,
    eligibleSession,
    endpointDigest,
    installedWebApp,
    serializeSubscription,
    validateConfig,
  });
  root.MacCellularIncomingCallPush = exported;
  if (typeof module === "object" && module && module.exports) module.exports = exported;
})(globalThis);
