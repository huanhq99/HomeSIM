const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const {
  directVoiceErrorMessage,
  foregroundCallControls,
  incomingCallTransition,
  showForegroundNotification,
  v2VoiceSession,
} = require("./public-voice-ui.js");

test("direct voice failures produce actionable and non-sensitive messages", () => {
	assert.equal(
		directVoiceErrorMessage(new Error("remote media icegatheringstatechange timed out")),
		"公网中继候选收集超时，请检查当前网络后重试",
	);
	assert.equal(
		directVoiceErrorMessage(new Error("remote media icegatheringstatechange timed out (ICE 701)")),
		"当前浏览器无法连接公网中继（ICE 701），请切换网络或关闭 VPN/代理后重试",
	);
	assert.equal(directVoiceErrorMessage(new Error("browser produced a non-relay media offer")),
		"浏览器未能建立公网中继，请检查当前网络后重试");
	assert.equal(directVoiceErrorMessage(new Error("remote media local-offer failed")),
		"当前浏览器无法初始化音频通话，请更新浏览器或换用 Safari/Chrome 后重试");
	assert.equal(directVoiceErrorMessage(new Error("remote media relay-candidate failed")),
		"公网中继候选收集失败，请检查当前网络后重试");
	assert.equal(directVoiceErrorMessage(new Error("remote media gateway-offer failed")),
		"家中 Mac 未能建立音频中继，请稍后重试");
	assert.equal(directVoiceErrorMessage(new Error("remote media canceled (call-changed)")),
		"通话状态已变化，请刷新后重试");
	assert.equal(directVoiceErrorMessage(new Error("gateway returned a non-relay media answer")),
		"家中 Mac 未能建立公网中继，请稍后重试");
  assert.equal(
    directVoiceErrorMessage(new Error("microphone capture is unavailable")),
    "无法使用麦克风，请允许浏览器麦克风权限后重试",
  );
  assert.equal(
    directVoiceErrorMessage(new Error("public relay credentials are unavailable")),
    "公网音频凭据暂不可用，请稍后重试",
  );
  assert.equal(directVoiceErrorMessage(new Error("secret marker")), "拨号准备失败（诊断：unclassified-browser-error）");
  assert.equal(
    directVoiceErrorMessage(new Error("remote media connectionstatechange failed")),
    "音频准备失败（诊断：remote media connectionstatechange failed）",
  );
  assert.equal(
    directVoiceErrorMessage(new Error("remote media https://secret.example failed")),
    "拨号准备失败（诊断：unclassified-browser-error）",
  );
  assert.equal(directVoiceErrorMessage({ code: "invalid_contract" }), "拨号准备失败（诊断码：invalid_contract）");
  assert.equal(directVoiceErrorMessage({}), "拨号准备失败（诊断：missing-error-message）");
});

const callID = "A".repeat(43);
const leaseID = `pml_${"B".repeat(43)}`;

function session(...actions) {
  return {
    sms_only: false,
    external_voice: true,
    external_voice_api_version: 2,
    remote_control: true,
    actions,
  };
}

function voice(phase, options = {}) {
  return {
    enabled: true,
    health: "connected",
    owned_by_requester: options.owned ?? true,
    answer_enabled: options.answerEnabled ?? true,
    reject_enabled: options.rejectEnabled ?? true,
    dtmf_enabled: options.dtmfEnabled ?? true,
    end_enabled: options.endEnabled ?? true,
    call: {
      call: { public_call_id: callID, generation: 3 },
      revision: options.revision ?? 7,
      phase,
      media: options.media === false ? null : {
        lease_id: leaseID,
        prepared: true,
      },
    },
  };
}

function local(phase, options = {}) {
  return {
    phase,
    public_call_id: options.callID ?? callID,
    call_generation: options.generation ?? 3,
    revision: options.revision ?? 7,
    media_lease_id: options.leaseID ?? leaseID,
  };
}

test("foreground call UI walks incoming, answer, active, end, and reconcile states", () => {
  const permissions = session("calls.read", "calls.media", "calls.control", "calls.hangup");

  const incoming = foregroundCallControls({
    session: permissions,
    voice: voice("incoming_ringing", { media: false }),
    local: { phase: "idle" },
  });
  assert.deepEqual(incoming, {
    ringing: true,
    active: false,
    unknown: false,
    exact_media: false,
    prepare_visible: true,
    prepare_enabled: true,
    answer_visible: true,
    answer_enabled: false,
    reject_visible: true,
    reject_enabled: true,
    end_visible: false,
    end_enabled: false,
    reconcile_visible: false,
    reconcile_enabled: false,
  });

  const prepared = foregroundCallControls({
    session: permissions,
    voice: voice("media_ready"),
    local: local("prepared"),
  });
  assert.equal(prepared.prepare_visible, false);
  assert.equal(prepared.answer_visible, true);
  assert.equal(prepared.answer_enabled, true);
  assert.equal(prepared.end_enabled, false);

  const active = foregroundCallControls({
    session: permissions,
    voice: voice("active_transport_verified"),
    local: local("active"),
  });
  assert.equal(active.answer_visible, false);
  assert.equal(active.end_visible, true);
  assert.equal(active.end_enabled, true);

  const unknown = foregroundCallControls({
    session: permissions,
    voice: voice("answer_pending"),
    local: local("outcome_unknown"),
    unknownKind: "answer",
  });
  assert.equal(unknown.answer_visible, false);
  assert.equal(unknown.reconcile_visible, true);
  assert.equal(unknown.reconcile_enabled, true);
});

test("foreground controls require exact v2 flags, principal scopes, and media lease", () => {
  const wildcard = session("*");
  assert.equal(v2VoiceSession(wildcard), false);
  const wildcardControls = foregroundCallControls({
    session: wildcard,
    voice: voice("incoming_ringing", { media: false }),
    local: { phase: "idle" },
  });
  assert.equal(wildcardControls.prepare_enabled, false);

  const missingMedia = session("calls.read", "calls.control", "calls.hangup");
  assert.equal(foregroundCallControls({
    session: missingMedia,
    voice: voice("media_ready"),
    local: local("prepared"),
  }).answer_enabled, false);

  const wrongLease = foregroundCallControls({
    session: session("calls.read", "calls.media", "calls.control", "calls.hangup"),
    voice: voice("active_transport_verified"),
    local: local("active", { leaseID: `pml_${"C".repeat(43)}` }),
  });
  assert.equal(wrongLease.end_enabled, false);

  const smsOnly = session("calls.read", "calls.media", "calls.control");
  smsOnly.sms_only = true;
  assert.equal(foregroundCallControls({
    session: smsOnly,
    voice: voice("incoming_ringing", { media: false }),
    local: { phase: "idle" },
  }).prepare_enabled, false);
});

test("incoming foreground notification is one per exact ringing call and resets after ringing", () => {
  const first = incomingCallTransition("", voice("incoming_ringing", { media: false }));
  assert.deepEqual(first, { next_call_id: callID, notify: true });
  assert.deepEqual(
    incomingCallTransition(first.next_call_id, voice("incoming_ringing", { media: false })),
    { next_call_id: callID, notify: false },
  );
  assert.deepEqual(
    incomingCallTransition(callID, voice("media_ready")),
    { next_call_id: "", notify: false },
  );
  assert.deepEqual(
    incomingCallTransition("", voice("incoming_ringing", { media: false })),
    { next_call_id: callID, notify: true },
  );
});

test("mobile notification prefers service worker and falls back without throwing", async () => {
  const shown = [];
  function NotificationFallback(title, options) { shown.push({ surface: "constructor", title, options }); }
  NotificationFallback.permission = "granted";
  const serviceWorkerEnvironment = {
    Notification: NotificationFallback,
    navigator: { serviceWorker: { async getRegistration(scope) {
      assert.equal(scope, "/remote/");
      return { async showNotification(title, options) {
        shown.push({ surface: "service-worker", title, options });
      } };
    } } },
  };
  assert.equal(await showForegroundNotification("外置网关来电", { tag: "voice" }, serviceWorkerEnvironment), true);
  assert.deepEqual(shown, [{
    surface: "service-worker", title: "外置网关来电", options: { tag: "voice" },
  }]);

  const fallbackEnvironment = {
    Notification: NotificationFallback,
    navigator: { serviceWorker: { async getRegistration() { return null; } } },
  };
  assert.equal(await showForegroundNotification("外置网关来电", { tag: "voice-2" }, fallbackEnvironment), true);
  assert.equal(shown.at(-1).surface, "constructor");

  NotificationFallback.permission = "denied";
  assert.equal(await showForegroundNotification("外置网关来电", {}, fallbackEnvironment), false);
});

test("service-worker notification click returns to the public PWA", () => {
  const source = fs.readFileSync(path.join(__dirname, "service-worker.js"), "utf8");
  assert.match(source, /notificationclick/);
  assert.match(source, /clients\.matchAll/);
  assert.match(source, /clients\.openWindow\("\/remote\/"\)/);
});
