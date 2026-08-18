(function (root, factory) {
  "use strict";
  var application = factory();
  if (typeof module === "object" && module !== null && module.exports) {
    module.exports = application;
    return;
  }
  if (root && root.document) {
    application.start(root.document, root);
  }
}(typeof globalThis === "object" ? globalThis : this, function () {
  "use strict";

  var SNAPSHOT_PATH = "/api/public/v1/state/snapshot";
  var PROTOCOL_VERSION = 1;
  var MAX_WIRE_COUNTER = 9007199254740991;
  var MAX_RESPONSE_BYTES = 16384;
  var MAX_JSON_DEPTH = 8;
  var SUCCESS_POLL_MS = 5000;
  var MAX_BACKOFF_MS = 60000;
  var FETCH_TIMEOUT_MS = 8000;

  var SERVICE_LABELS = Object.freeze({
    starting: "启动中",
    ready: "运行正常",
    degraded: "服务降级",
    recovery_required: "需要恢复"
  });
  var CALL_LABELS = Object.freeze({
    unavailable: "不可用",
    idle: "空闲",
    ringing: "响铃中",
    connecting: "连接中",
    active: "通话中",
    recovery_required: "需要恢复"
  });
  var SMS_LABELS = Object.freeze({
    unavailable: "不可用",
    ready: "可用",
    degraded: "服务降级"
  });

  function failSchema() {
    throw new Error("invalid public snapshot schema");
  }

  function hasOwn(value, key) {
    return Object.prototype.hasOwnProperty.call(value, key);
  }

  function isPlainObject(value) {
    return value !== null && typeof value === "object" &&
      !Array.isArray(value) && Object.getPrototypeOf(value) === Object.prototype;
  }

  function hasExactKeys(value, keys) {
    if (!isPlainObject(value)) {
      return false;
    }
    var actual = Object.keys(value);
    if (actual.length !== keys.length) {
      return false;
    }
    for (var index = 0; index < keys.length; index += 1) {
      if (!hasOwn(value, keys[index])) {
        return false;
      }
    }
    return true;
  }

  function isCounter(value, positive) {
    return typeof value === "number" && Number.isSafeInteger(value) &&
      value >= (positive ? 1 : 0) && value <= MAX_WIRE_COUNTER;
  }

  // JSON.parse accepts duplicate object keys. This bounded grammar walk rejects
  // them before parsing, including keys that are equal only after JSON escapes
  // are decoded.
  function rejectDuplicateJSONKeys(text) {
    var position = 0;

    function skipWhitespace() {
      while (position < text.length) {
        var code = text.charCodeAt(position);
        if (code !== 0x20 && code !== 0x09 && code !== 0x0a && code !== 0x0d) {
          break;
        }
        position += 1;
      }
    }

    function parseString() {
      if (text.charAt(position) !== "\"") {
        failSchema();
      }
      var start = position;
      position += 1;
      while (position < text.length) {
        var code = text.charCodeAt(position);
        if (code < 0x20) {
          failSchema();
        }
        if (code === 0x22) {
          position += 1;
          try {
            return JSON.parse(text.slice(start, position));
          } catch (_error) {
            failSchema();
          }
        }
        if (code === 0x5c) {
          position += 1;
          if (position >= text.length) {
            failSchema();
          }
          var escape = text.charAt(position);
          if (escape === "u") {
            for (var digit = 1; digit <= 4; digit += 1) {
              var hex = text.charAt(position + digit);
              if (!/^[0-9a-fA-F]$/.test(hex)) {
                failSchema();
              }
            }
            position += 5;
            continue;
          }
          if ('"\\/bfnrt'.indexOf(escape) === -1) {
            failSchema();
          }
          position += 1;
          continue;
        }
        position += 1;
      }
      failSchema();
    }

    function parseNumber() {
      if (text.charAt(position) === "-") {
        position += 1;
      }
      if (text.charAt(position) === "0") {
        position += 1;
      } else {
        if (!/^[1-9]$/.test(text.charAt(position))) {
          failSchema();
        }
        while (/^[0-9]$/.test(text.charAt(position))) {
          position += 1;
        }
      }
      if (text.charAt(position) === ".") {
        position += 1;
        if (!/^[0-9]$/.test(text.charAt(position))) {
          failSchema();
        }
        while (/^[0-9]$/.test(text.charAt(position))) {
          position += 1;
        }
      }
      var exponent = text.charAt(position);
      if (exponent === "e" || exponent === "E") {
        position += 1;
        var sign = text.charAt(position);
        if (sign === "+" || sign === "-") {
          position += 1;
        }
        if (!/^[0-9]$/.test(text.charAt(position))) {
          failSchema();
        }
        while (/^[0-9]$/.test(text.charAt(position))) {
          position += 1;
        }
      }
    }

    function consumeLiteral(literal) {
      if (text.slice(position, position + literal.length) !== literal) {
        failSchema();
      }
      position += literal.length;
    }

    function parseValue(depth) {
      if (depth > MAX_JSON_DEPTH) {
        failSchema();
      }
      skipWhitespace();
      var token = text.charAt(position);
      if (token === "{") {
        parseObject(depth + 1);
      } else if (token === "[") {
        parseArray(depth + 1);
      } else if (token === "\"") {
        parseString();
      } else if (token === "t") {
        consumeLiteral("true");
      } else if (token === "f") {
        consumeLiteral("false");
      } else if (token === "n") {
        consumeLiteral("null");
      } else if (token === "-" || /^[0-9]$/.test(token)) {
        parseNumber();
      } else {
        failSchema();
      }
    }

    function parseObject(depth) {
      position += 1;
      skipWhitespace();
      var keys = new Set();
      if (text.charAt(position) === "}") {
        position += 1;
        return;
      }
      while (position < text.length) {
        skipWhitespace();
        var key = parseString();
        if (keys.has(key)) {
          failSchema();
        }
        keys.add(key);
        skipWhitespace();
        if (text.charAt(position) !== ":") {
          failSchema();
        }
        position += 1;
        parseValue(depth);
        skipWhitespace();
        var delimiter = text.charAt(position);
        if (delimiter === "}") {
          position += 1;
          return;
        }
        if (delimiter !== ",") {
          failSchema();
        }
        position += 1;
      }
      failSchema();
    }

    function parseArray(depth) {
      position += 1;
      skipWhitespace();
      if (text.charAt(position) === "]") {
        position += 1;
        return;
      }
      while (position < text.length) {
        parseValue(depth);
        skipWhitespace();
        var delimiter = text.charAt(position);
        if (delimiter === "]") {
          position += 1;
          return;
        }
        if (delimiter !== ",") {
          failSchema();
        }
        position += 1;
      }
      failSchema();
    }

    parseValue(0);
    skipWhitespace();
    if (position !== text.length) {
      failSchema();
    }
  }

  function parseSnapshotText(text) {
    if (typeof text !== "string" || text.length === 0 || text.length > MAX_RESPONSE_BYTES) {
      failSchema();
    }
    rejectDuplicateJSONKeys(text);
    var value;
    try {
      value = JSON.parse(text);
    } catch (_error) {
      failSchema();
    }
    if (!hasExactKeys(value, ["version", "gateway_online", "received_at", "snapshot"]) ||
        value.version !== PROTOCOL_VERSION || typeof value.gateway_online !== "boolean" ||
        !isCounter(value.received_at, false)) {
      failSchema();
    }
    if (value.snapshot === null) {
      if (value.gateway_online || value.received_at !== 0) {
        failSchema();
      }
      return Object.freeze({
        version: value.version,
        gateway_online: false,
        received_at: 0,
        snapshot: null
      });
    }
    if (!hasExactKeys(value.snapshot, [
      "observed_at", "revision", "event_high_water", "service", "call", "sms"
    ]) || !isCounter(value.snapshot.observed_at, true) ||
        !isCounter(value.snapshot.revision, true) ||
        !isCounter(value.snapshot.event_high_water, false) ||
        !hasOwn(SERVICE_LABELS, value.snapshot.service) ||
        !hasOwn(CALL_LABELS, value.snapshot.call) ||
        !hasOwn(SMS_LABELS, value.snapshot.sms) || value.received_at === 0) {
      failSchema();
    }
    var snapshot = Object.freeze({
      observed_at: value.snapshot.observed_at,
      revision: value.snapshot.revision,
      event_high_water: value.snapshot.event_high_water,
      service: value.snapshot.service,
      call: value.snapshot.call,
      sms: value.snapshot.sms
    });
    return Object.freeze({
      version: value.version,
      gateway_online: value.gateway_online,
      received_at: value.received_at,
      snapshot: snapshot
    });
  }

  function relativeAge(unixSeconds, nowMilliseconds) {
    var safeNow = typeof nowMilliseconds === "number" && Number.isFinite(nowMilliseconds) ?
      nowMilliseconds : Date.now();
    var age = Math.max(0, Math.floor(safeNow / 1000) - unixSeconds);
    if (age < 10) {
      return "刚刚";
    }
    if (age < 60) {
      return String(age) + " 秒前";
    }
    if (age < 3600) {
      return String(Math.floor(age / 60)) + " 分钟前";
    }
    if (age < 86400) {
      return String(Math.floor(age / 3600)) + " 小时前";
    }
    return String(Math.floor(age / 86400)) + " 天前";
  }

  function toViewModel(state, nowMilliseconds) {
    if (!state || state.snapshot === null) {
      return Object.freeze({
        tone: "loading",
        title: "尚未连接",
        badge: "等待状态",
        detail: "已通过身份验证，但尚未收到家中网关的状态。",
        received: "—",
        revision: "—",
        service: "—",
        call: "—",
        sms: "—"
      });
    }
    var online = state.gateway_online;
    return Object.freeze({
      tone: online ? "online" : "stale",
      title: online ? "网关在线" : "网关已离线",
      badge: online ? "在线" : "过期状态",
      detail: online ?
        "已收到网关的最新粗粒度状态。" :
        "当前显示的是最后一次状态，已明确标记为过期。",
      received: relativeAge(state.received_at, nowMilliseconds),
      revision: "第 " + String(state.snapshot.revision) + " 版",
      service: SERVICE_LABELS[state.snapshot.service],
      call: CALL_LABELS[state.snapshot.call],
      sms: SMS_LABELS[state.snapshot.sms]
    });
  }

  function nextPollDelay(failureCount, randomUnit) {
    if (!Number.isSafeInteger(failureCount) || failureCount < 0) {
      throw new Error("invalid polling failure count");
    }
    if (failureCount === 0) {
      return SUCCESS_POLL_MS;
    }
    var unit = randomUnit === undefined ? 0.5 : randomUnit;
    if (typeof unit !== "number" || !Number.isFinite(unit) || unit < 0 || unit > 1) {
      throw new Error("invalid polling jitter");
    }
    var exponent = Math.min(failureCount - 1, 3);
    var ceiling = Math.min(MAX_BACKOFF_MS, 10000 * Math.pow(2, exponent));
    return Math.round(Math.max(SUCCESS_POLL_MS, ceiling * (0.8 + 0.2 * unit)));
  }

  function classifyHTTPResponse(status, responseType) {
    if (!Number.isSafeInteger(status) || status < 0 || status > 599 ||
        typeof responseType !== "string") {
      throw new Error("invalid HTTP response metadata");
    }
    if (responseType === "opaqueredirect" || status === 401) {
      return "unauthorized";
    }
    if (status === 503) {
      return "unavailable";
    }
    if (status === 200) {
      return "ok";
    }
    return "invalid";
  }

  function start(documentObject, windowObject) {
    var elements = {
      title: documentObject.getElementById("connection-title"),
      badge: documentObject.getElementById("status-badge"),
      badgeText: documentObject.getElementById("status-badge-text"),
      detail: documentObject.getElementById("status-detail"),
      received: documentObject.getElementById("received-at"),
      revision: documentObject.getElementById("revision"),
      service: documentObject.getElementById("service-state"),
      call: documentObject.getElementById("call-state"),
      sms: documentObject.getElementById("sms-state")
    };
    var required = Object.keys(elements);
    for (var index = 0; index < required.length; index += 1) {
      if (!elements[required[index]]) {
        return;
      }
    }

    var timer = null;
    var activeRequest = null;
    var generation = 0;
    var failures = 0;

    function render(model) {
      elements.title.textContent = model.title;
      elements.badge.setAttribute("data-tone", model.tone);
      elements.badgeText.textContent = model.badge;
      elements.detail.textContent = model.detail;
      elements.received.textContent = model.received;
      elements.revision.textContent = model.revision;
      elements.service.textContent = model.service;
      elements.call.textContent = model.call;
      elements.sms.textContent = model.sms;
    }

    function renderEmpty(tone, title, badge, detail) {
      render({
        tone: tone,
        title: title,
        badge: badge,
        detail: detail,
        received: "—",
        revision: "—",
        service: "—",
        call: "—",
        sms: "—"
      });
    }

    function cancelWork() {
      if (timer !== null) {
        windowObject.clearTimeout(timer);
        timer = null;
      }
      if (activeRequest !== null) {
        activeRequest.abort();
        activeRequest = null;
      }
    }

    function clearHiddenState() {
      generation += 1;
      failures = 0;
      cancelWork();
      renderEmpty(
        "loading",
        "状态已隐藏",
        "已清除",
        "页面位于后台，已清除本次读取的状态。"
      );
    }

    function renderFailure(kind) {
      if (kind === "unauthorized") {
        renderEmpty(
          "unauthorized",
          "需要重新验证",
          "未授权",
          "访问身份已失效，请重新打开本页完成验证。"
        );
        return;
      }
      if (kind === "unavailable") {
        renderEmpty(
          "unavailable",
          "验证服务暂不可用",
          "稍后重试",
          "身份验证服务暂时不可用，页面将自动限速重试。"
        );
        return;
      }
      if (kind === "invalid") {
        renderEmpty(
          "invalid",
          "状态数据无效",
          "已拒绝",
          "收到的响应不符合严格协议，未显示任何旧状态。"
        );
        return;
      }
      renderEmpty(
        "unavailable",
        "暂时无法连接",
        "重试中",
        "当前无法读取网关状态，页面将自动限速重试。"
      );
    }

    function requestFailure(kind) {
      var error = new Error("public snapshot request failed");
      error.kind = kind;
      return error;
    }

    async function readBoundedText(response) {
      var contentLength = response.headers.get("Content-Length");
      if (contentLength !== null &&
          (!/^(0|[1-9][0-9]*)$/.test(contentLength) || Number(contentLength) > MAX_RESPONSE_BYTES)) {
        throw requestFailure("invalid");
      }
      if (!response.body || typeof response.body.getReader !== "function") {
        throw requestFailure("invalid");
      }
      var reader = response.body.getReader();
      var decoder = new windowObject.TextDecoder("utf-8", {fatal: true});
      var received = 0;
      var text = "";
      while (true) {
        var result = await reader.read();
        if (result.done) {
          break;
        }
        received += result.value.byteLength;
        if (received > MAX_RESPONSE_BYTES) {
          await reader.cancel();
          throw requestFailure("invalid");
        }
        try {
          text += decoder.decode(result.value, {stream: true});
        } catch (_error) {
          await reader.cancel();
          throw requestFailure("invalid");
        }
      }
      try {
        text += decoder.decode();
      } catch (_error) {
        throw requestFailure("invalid");
      }
      return text;
    }

    function responseIsExact(response) {
      if (!response.url) {
        return false;
      }
      var parsed;
      try {
        parsed = new windowObject.URL(response.url, windowObject.location.href);
      } catch (_error) {
        return false;
      }
      return parsed.origin === windowObject.location.origin &&
        parsed.pathname === SNAPSHOT_PATH && parsed.search === "" && parsed.hash === "";
    }

    async function readSnapshot(controller) {
      var response;
      try {
        response = await windowObject.fetch(SNAPSHOT_PATH, {
          method: "GET",
          headers: {Accept: "application/json"},
          cache: "no-store",
          credentials: "same-origin",
          redirect: "manual",
          referrerPolicy: "no-referrer",
          signal: controller.signal
        });
      } catch (error) {
        if (error && error.name === "AbortError") {
          throw error;
        }
        throw requestFailure("network");
      }
      var classification = classifyHTTPResponse(response.status, response.type);
      if (classification !== "ok") {
        throw requestFailure(classification);
      }
      if (!responseIsExact(response) ||
          response.headers.get("Content-Type") !== "application/json") {
        throw requestFailure("invalid");
      }
      return parseSnapshotText(await readBoundedText(response));
    }

    function schedule(runGeneration) {
      var delay = nextPollDelay(failures, Math.random());
      timer = windowObject.setTimeout(function () {
        timer = null;
        refresh(runGeneration);
      }, delay);
    }

    async function refresh(runGeneration) {
      if (runGeneration !== generation || documentObject.visibilityState === "hidden") {
        return;
      }
      var controller = new windowObject.AbortController();
      activeRequest = controller;
      var timeout = windowObject.setTimeout(function () {
        controller.abort();
      }, FETCH_TIMEOUT_MS);
      try {
        var state = await readSnapshot(controller);
        if (runGeneration !== generation || documentObject.visibilityState === "hidden") {
          return;
        }
        failures = 0;
        render(toViewModel(state, Date.now()));
      } catch (error) {
        if (runGeneration !== generation || documentObject.visibilityState === "hidden") {
          return;
        }
        failures = Math.min(8, failures + 1);
        renderFailure(error && error.kind ? error.kind : "network");
      } finally {
        windowObject.clearTimeout(timeout);
        // Fetch resolves as soon as response headers arrive. Always abort the
        // request incarnation so a rejected status, URL, MIME type, or size
        // cannot keep streaming an unread body after this refresh completes.
        controller.abort();
        if (activeRequest === controller) {
          activeRequest = null;
        }
      }
      if (runGeneration === generation && documentObject.visibilityState !== "hidden") {
        schedule(runGeneration);
      }
    }

    function startVisiblePolling() {
      generation += 1;
      failures = 0;
      cancelWork();
      renderEmpty(
        "loading",
        "正在刷新",
        "连接中",
        "正在读取家中网关的最新状态。"
      );
      refresh(generation);
    }

    documentObject.addEventListener("visibilitychange", function () {
      if (documentObject.visibilityState === "hidden") {
        clearHiddenState();
      } else {
        startVisiblePolling();
      }
    });
    windowObject.addEventListener("pagehide", clearHiddenState);
    windowObject.addEventListener("pageshow", function () {
      if (documentObject.visibilityState !== "hidden") {
        startVisiblePolling();
      }
    });

    if (documentObject.visibilityState === "hidden") {
      clearHiddenState();
    } else {
      startVisiblePolling();
    }
  }

  return Object.freeze({
    parseSnapshotText: parseSnapshotText,
    toViewModel: toViewModel,
    nextPollDelay: nextPollDelay,
    classifyHTTPResponse: classifyHTTPResponse,
    start: start,
    constants: Object.freeze({
      snapshotPath: SNAPSHOT_PATH,
      protocolVersion: PROTOCOL_VERSION,
      maxResponseBytes: MAX_RESPONSE_BYTES,
      successPollMilliseconds: SUCCESS_POLL_MS,
      maxBackoffMilliseconds: MAX_BACKOFF_MS,
      fetchTimeoutMilliseconds: FETCH_TIMEOUT_MS
    })
  });
}));
