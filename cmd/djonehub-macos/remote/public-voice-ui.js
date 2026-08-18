(function installPublicVoiceUI(root) {
  "use strict";

  const incomingPhases = new Set([
    "incoming_ringing",
    "media_preparing",
    "media_ready",
    "answer_pending",
  ]);
  const activePhases = new Set([
    "active_unverified",
    "active_transport_verified",
  ]);

  function explicitAction(session, action) {
    return Boolean(Array.isArray(session?.actions) && session.actions.includes(action));
  }

  function v2VoiceSession(session) {
    return Boolean(session?.sms_only === false && session?.external_voice === true &&
      session?.external_voice_api_version === 2 && session?.remote_control === true &&
      explicitAction(session, "calls.read"));
  }

  function foregroundCallControls({ session, voice, local, unknownKind = "", operationBusy = false } = {}) {
    const call = voice?.call;
    const localState = local || { phase: "idle" };
    const ringing = Boolean(call && incomingPhases.has(call.phase));
    const active = Boolean(call && activePhases.has(call.phase));
    const unknown = localState.phase === "outcome_unknown";
    const connected = voice?.enabled === true && voice?.health === "connected";
    const exactCall = Boolean(call && localState.public_call_id === call.call?.public_call_id &&
      localState.call_generation === call.call?.generation && localState.revision === call.revision);
    const exactMedia = Boolean(exactCall && voice?.owned_by_requester === true && call.media &&
      localState.media_lease_id === call.media.lease_id);
    const media = v2VoiceSession(session) && explicitAction(session, "calls.media");
    const answer = media && explicitAction(session, "calls.control");
    const end = media && explicitAction(session, "calls.hangup");
    const operationIdle = operationBusy !== true;

    const prepareEnabled = operationIdle && connected && call?.phase === "incoming_ringing" &&
      voice?.answer_enabled === true && answer && localState.phase === "idle";
    const answerEnabled = operationIdle && connected && call?.phase === "media_ready" &&
      call.media?.prepared === true && voice?.answer_enabled === true && exactMedia && answer &&
      localState.phase === "prepared";
    const endEnabled = operationIdle && connected && active && voice?.end_enabled === true &&
      exactMedia && end && localState.phase === "active";
    const reconcileEnabled = operationIdle && connected && unknown && media &&
      ((unknownKind === "answer" && answer) || (unknownKind === "end" && end));

    return Object.freeze({
      ringing,
      active,
      unknown,
      exact_media: exactMedia,
		prepare_visible: ringing && localState.phase === "idle",
		prepare_enabled: prepareEnabled,
		answer_visible: ringing && !unknown,
		answer_enabled: answerEnabled,
		reject_visible: ringing && !unknown,
		reject_enabled: operationIdle && connected && ringing && voice?.reject_enabled === true &&
			v2VoiceSession(session) && explicitAction(session, "calls.control"),
		end_visible: active && !unknown,
      end_enabled: endEnabled,
      reconcile_visible: unknown,
      reconcile_enabled: reconcileEnabled,
    });
  }

  function incomingCallTransition(previousCallID, voice) {
    const call = voice?.call;
    const callID = call?.phase === "incoming_ringing" && call?.direction !== "outgoing" &&
      typeof call.call?.public_call_id === "string" && call.call.public_call_id
      ? call.call.public_call_id
      : "";
    return Object.freeze({
      next_call_id: callID,
      notify: callID !== "" && callID !== previousCallID,
    });
  }

  function directVoiceErrorMessage(error) {
    const message = typeof error?.message === "string" ? error.message : "";
    const code = typeof error?.code === "string" && /^[a-z0-9_]{1,48}$/.test(error.code)
      ? error.code
      : "";
    if (message.includes("microphone")) return "无法使用麦克风，请允许浏览器麦克风权限后重试";
    if (message.includes("relay credentials")) return "公网音频凭据暂不可用，请稍后重试";
    if (message.includes("browser-peer") || message.includes("local-offer")) return "当前浏览器无法初始化音频通话，请更新浏览器或换用 Safari/Chrome 后重试";
    if (message.includes("browser produced a non-relay")) return "浏览器未能建立公网中继，请检查当前网络后重试";
    if (message.includes("gateway returned a non-relay")) return "家中 Mac 未能建立公网中继，请稍后重试";
    if (message.includes("ICE 701")) return "当前浏览器无法连接公网中继（ICE 701），请切换网络或关闭 VPN/代理后重试";
    if (message.includes("icecandidate")) return "公网中继候选收集超时，请检查当前网络后重试";
    if (message.includes("relay-candidate")) return "公网中继候选收集失败，请检查当前网络后重试";
    if (message.includes("gateway-offer") || message.includes("remote-answer")) return "家中 Mac 未能建立音频中继，请稍后重试";
    if (message.includes("relay-connect")) return "公网音频中继连接失败，请切换网络后重试";
    if (message.includes("audio-play")) return "已建立通话媒体，但浏览器未能播放音频，请确认系统媒体音量后重试";
    if (message.includes("snapshot-unavailable")) return "页面状态刷新期间中断了音频准备，请稍后重试";
    if (message.includes("call-changed")) return "通话状态已变化，请刷新后重试";
    if (message.includes("unexpected-remote-track")) return "家中 Mac 返回了异常音轨，音频会话已安全关闭";
    if (message.includes("remote media canceled")) return "音频准备被页面状态变化中断，请刷新后重试";
    if (message.includes("icegatheringstatechange")) return "公网中继候选收集超时，请检查当前网络后重试";
    if (message.includes("media answer") || message.includes("peer")) {
      return "公网音频中继连接失败，请检查当前网络后重试";
    }
    if (message.includes("timed out")) return "公网音频连接超时，请检查当前网络后重试";
    if (message.startsWith("remote media ") && /^[A-Za-z0-9 (),-]+$/.test(message)) {
      return `音频准备失败（诊断：${message}）`;
    }
    if (code) return `拨号准备失败（诊断码：${code}）`;
    if (!message) return "拨号准备失败（诊断：missing-error-message）";
    return "拨号准备失败（诊断：unclassified-browser-error）";
  }

  async function showForegroundNotification(title, options, environment = root) {
    const NotificationAPI = environment?.Notification;
    if (typeof title !== "string" || title.length === 0 ||
        !NotificationAPI || NotificationAPI.permission !== "granted") {
      return false;
    }
    try {
      const registration = await environment.navigator?.serviceWorker?.getRegistration?.("/remote/");
      if (registration && typeof registration.showNotification === "function") {
        await registration.showNotification(title, options);
        return true;
      }
    } catch {
      // Fall through to the foreground constructor used by browsers without
      // a usable service-worker notification surface.
    }
    try {
      if (typeof NotificationAPI !== "function") return false;
      new NotificationAPI(title, options);
      return true;
    } catch {
      return false;
    }
  }

  const exported = Object.freeze({
    explicitAction,
    v2VoiceSession,
    foregroundCallControls,
    incomingCallTransition,
    directVoiceErrorMessage,
    showForegroundNotification,
  });
  root.MacCellularPublicVoiceUI = exported;
  if (typeof module === "object" && module && module.exports) module.exports = exported;
})(globalThis);
