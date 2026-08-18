const $ = (selector) => document.querySelector(selector);
let snapshot = null;
let session = null;
let selectedPeer = "";
let previousCallID = "";
let previousLegacyMessageKey = "";
let polling = false;
let callPolling = false;
let refreshAbortController = null;
let remoteMediaClient = null;
let preparedMediaCallID = "";
let rescueMediaReceipt = null;
let externalVoiceClient = null;
let externalVoiceStatus = null;
let externalVoiceStatusError = "";
let externalVoiceUnknownKind = "";
let externalVoiceIdentity = "";
let externalVoiceOperationToken = null;
let externalVoiceSnapshotInFlight = null;
let previousExternalCallID = "";
let viewEpoch = 0;
let incomingMediaPreparation = null;
let autoPrepareAttemptedCallID = "";
let directAnswerInFlight = false;
let directAnswerStartedAt = 0;
let directAnswerProgressTimer = null;
let directAnsweredCallID = "";
let directHangupInFlight = false;
let directDTMFBuffer = "";
let directDTMFTimer = null;
let directDTMFSending = false;
let directDTMFCallID = "";
let callMuted = false;
let callKeypadVisible = false;
let callRecorder = null;
let recordingStore = null;
let recordings = [];
let phoneRecordings = [];
let macRecordings = [];
let recordingsLoadedFromMac = false;
let recordingURLs = [];
let recordingStartInFlight = false;
let recordingSuppressedCallID = "";
let recordingLastError = "";
let lastObservedCall = null;
let callEndedTimer = null;
let callMeterFrame = null;
let callMeterLastPaint = 0;
const remoteSMSState = new globalThis.MacCellularRemoteSMSState();
const sessionPresentation = globalThis.MacCellularRemoteSessionPresentation;
const publicVoiceUI = globalThis.MacCellularPublicVoiceUI;
const smsSyncPageLimit = 100;
const smsSyncMaximumPagesPerRefresh = 20;
const lineLabelStorageKey = "maccellular.phone.line-label.v1";

function requestID() {
  if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
}

function hasActionFor(candidate, action) {
  return Boolean((candidate?.actions || []).some((item) => item === action || item === "*"));
}

function hasExplicitActionFor(candidate, action) {
  return Boolean((candidate?.actions || []).some((item) => item === action));
}

function hasAction(action) {
  return hasActionFor(session, action);
}

function can(action) {
  return Boolean(session?.remote_control && hasAction(action));
}

function smsOnlySession(candidate = session) {
  return sessionPresentation.smsOnly(candidate);
}

function selectTab(tab) {
  document.querySelectorAll("[data-tab]").forEach((item) => item.classList.toggle("active", item.dataset.tab === tab));
  document.querySelectorAll(".panel").forEach((panel) => panel.classList.toggle("active", panel.id === tab));
  window.scrollTo({ top: 0, behavior: "smooth" });
}

function savedLineLabel() {
  try { return localStorage.getItem(lineLabelStorageKey)?.trim() || "家庭主卡"; } catch { return "家庭主卡"; }
}

function renderProductOverview() {
  const history = snapshot?.calls?.history || [];
  const missed = history.filter((item) => item.missed).length;
  const messages = currentSMSItems();
  const conversations = groupMessages(messages);
  $("#line-label").textContent = savedLineLabel();
  $("#home-call-count").textContent = String(history.length);
  $("#home-missed").textContent = missed ? `${missed} 个未接` : "无未接";
  $("#home-message-count").textContent = String(messages.length);
  $("#home-conversation-count").textContent = `${conversations.length} 个会话`;
  $("#home-recording-count").textContent = String(recordings.length);
  $("#home-updated").textContent = snapshot ? `更新于 ${formatTime(new Date().toISOString())}` : "正在获取状态";
}

function ensureRecordingServices() {
  if (!callRecorder && typeof globalThis.MacCellularCallRecorder === "function") {
    callRecorder = new globalThis.MacCellularCallRecorder({ onState: () => renderCall() });
  }
  if (!recordingStore && typeof globalThis.MacCellularRecordingStore === "function" && globalThis.indexedDB) {
    recordingStore = new globalThis.MacCellularRecordingStore();
  }
  return callRecorder;
}

function mergeRecordingSources() {
  const merged = new Map();
  for (const item of macRecordings) merged.set(item.id, { ...item, mac_saved: true });
  for (const item of phoneRecordings) {
    const mac = merged.get(item.id);
    merged.set(item.id, { ...mac, ...item, audio_path: mac?.audio_path || "", mac_saved: Boolean(mac || item.mac_saved) });
  }
  const primaryByCall = new Map();
  for (const item of merged.values()) {
    const key = item.call_id || item.id;
    const current = primaryByCall.get(key);
    if (!current || (item.duration_seconds || 0) > (current.duration_seconds || 0)) {
      primaryByCall.set(key, item);
    }
  }
  recordings = [...primaryByCall.values()].sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at));
}

async function reloadPhoneRecordings() {
  if (!recordingStore) phoneRecordings = [];
  else try { phoneRecordings = await recordingStore.list(); } catch { phoneRecordings = []; }
  mergeRecordingSources();
}

async function loadRecordings({ includeMac = Boolean(session) } = {}) {
  ensureRecordingServices();
  await reloadPhoneRecordings();
  if (includeMac) {
    try {
      const response = await api("../api/remote/v1/recordings");
      macRecordings = Array.isArray(response?.items) ? response.items : [];
      recordingsLoadedFromMac = true;
    } catch {
      recordingsLoadedFromMac = false;
    }
    mergeRecordingSources();
  }
  renderRecordings();
}

function renderRecordings() {
  for (const url of recordingURLs) URL.revokeObjectURL(url);
  recordingURLs = [];
  const list = $("#recording-list");
  if (!list) return;
  $("#home-recording-count").textContent = String(recordings.length);
  list.replaceChildren();
  if (!recordings.length) {
    list.className = "empty";
    list.textContent = "暂无录音";
    return;
  }
  list.className = "recording-list";
  for (const item of recordings) {
    const row = document.createElement("article");
    row.className = "recording-row";
    const detail = document.createElement("div");
    const title = document.createElement("strong");
    title.textContent = formatRecordingTitle(item);
    const meta = document.createElement("small");
    const macState = item.mac_saved ? "手机 + Mac" : "仅手机 · Mac 待同步";
    meta.textContent = `${item.direction === "incoming" ? "来电" : "呼出"} · ${formatTime(item.started_at)} · ${item.duration_seconds || 0} 秒 · ${macState}`;
    detail.append(title, meta);
    const localBlob = item.blob instanceof Blob && item.blob.size > 0;
    const url = item.audio_path || (localBlob ? URL.createObjectURL(item.blob) : "");
    if (!item.audio_path && url) recordingURLs.push(url);
    const save = document.createElement("a");
    save.className = "recording-save";
    save.href = url || "#";
    const recordingPeer = safeNumber(item.number).replace(/[^+0-9A-Za-z_-]/g, "-");
    save.download = `MacCellular 通话录音_${String(item.started_at || "").replace(/[:.]/g, "-")}_${recordingPeer}.${item.extension || "m4a"}`;
    save.textContent = "保存";
    const actions = document.createElement("div");
    actions.className = "recording-actions";
    if (url) actions.append(save);
    if (!item.mac_saved) {
      const sync = document.createElement("button");
      sync.className = "recording-sync";
      sync.type = "button";
      sync.textContent = "同步 Mac";
      sync.addEventListener("click", async () => {
        sync.disabled = true;
        try {
          await uploadRecordingToMac(item);
          item.mac_saved = true;
          item.mac_error = "";
          await recordingStore.save(item);
          await reloadPhoneRecordings();
          renderRecordings();
          toast("Mac 备份已完成");
        } catch {
          sync.disabled = false;
          toast("Mac 暂时无法接收，手机录音仍然保留");
        }
      });
      actions.append(sync);
    }
    if (localBlob) {
      const remove = document.createElement("button");
      remove.className = "recording-delete";
      remove.type = "button";
      remove.textContent = "删手机副本";
      remove.addEventListener("click", async () => {
        if (!window.confirm("删除这段手机录音？Mac 备份不会删除。")) return;
        remove.disabled = true;
        try {
          await recordingStore.remove(item.id);
          await reloadPhoneRecordings();
          renderRecordings();
          toast("手机副本已删除，Mac 录音仍可播放");
        } catch {
          remove.disabled = false;
          toast("录音删除失败，请稍后重试");
        }
      });
      actions.append(remove);
    }
    const player = document.createElement("audio");
    player.controls = true;
    player.preload = "metadata";
    player.playsInline = true;
    if (url) player.src = url;
    const playbackState = document.createElement("small");
    playbackState.className = "recording-playback-state";
    playbackState.textContent = item.audio_path ? "点击播放，录音由家中 Mac 加载" : "点击播放手机副本";
    player.addEventListener("playing", () => { playbackState.textContent = "正在播放"; });
    player.addEventListener("pause", () => {
      if (!player.ended && player.currentTime > 0) playbackState.textContent = "已暂停";
    });
    player.addEventListener("error", () => {
      playbackState.textContent = item.audio_path
        ? "加载失败，请点击页面刷新后重试"
        : "手机副本无法解码，正在等待 Mac 副本";
    });
    row.append(detail, actions, player, playbackState);
    list.append(row);
  }
}

async function uploadRecordingToMac(recording, csrfToken = session?.csrf_token || "") {
  const metadata = {
    version: 1,
    id: recording.id,
    call_id: recording.call_id,
    number: recording.number || "",
    direction: recording.direction,
    started_at: recording.started_at,
    ended_at: recording.ended_at,
    duration_seconds: recording.duration_seconds,
    mime_type: recording.mime_type,
    extension: recording.extension,
  };
  const form = new FormData();
  form.append("metadata", new Blob([JSON.stringify(metadata)], { type: "application/json" }));
  form.append("audio", recording.blob, `call.${recording.extension}`);
  return api("../api/remote/v1/recordings", {
    method: "POST", body: form, ephemeral: true, csrfToken, timeoutMilliseconds: 120_000,
  });
}

async function stopCallRecording() {
  if (!callRecorder?.active()) return null;
  const csrfToken = session?.csrf_token || "";
  const recording = await callRecorder.stop();
  if (recording && recordingStore) {
    recording.mac_saved = false;
    recording.mac_error = "";
    await recordingStore.save(recording);
    await reloadPhoneRecordings();
    renderRecordings();
    try {
      await uploadRecordingToMac(recording, csrfToken);
      recording.mac_saved = true;
      await recordingStore.save(recording);
      await loadRecordings({ includeMac: true });
    } catch {
      recording.mac_error = "pending";
      await recordingStore.save(recording);
      await reloadPhoneRecordings();
      renderRecordings();
      toast("录音已保存在手机；Mac 备份稍后可重试");
    }
  }
  return recording;
}

async function maybeStartAutomaticRecording(call) {
  const recorder = ensureRecordingServices();
  if (!call || call.state !== "active" || externalVoiceSelected() || !recorder?.supported?.() ||
      recorder.active() || recordingStartInFlight || recordingSuppressedCallID === call.id) return;
  const streams = remoteMediaClient?.recordableStreams?.();
  if (!streams?.localStream || !streams?.remoteStream) return;
  recordingStartInFlight = true;
  recordingLastError = "";
  try {
    await recorder.start({ ...streams, metadata: {
      number: call.number || "未知号码", direction: call.direction || "incoming", call_id: call.id,
    }});
    toast("通话已自动录音，手机和 Mac 各保存一份");
  } catch {
    recordingLastError = "录音启动失败";
  } finally {
    recordingStartInFlight = false;
    renderCall();
  }
}

function showCallEnded(call) {
  const banner = $("#call-ended-banner");
  if (!banner) return;
  const seconds = elapsedSeconds(call?.started_at);
  $("#call-ended-detail").textContent = `${safeNumber(call?.number)}${seconds ? ` · ${formatDuration(seconds)}` : ""}`;
  banner.hidden = false;
  if (callEndedTimer) clearTimeout(callEndedTimer);
  callEndedTimer = setTimeout(() => { banner.hidden = true; }, 3200);
  navigator.vibrate?.(60);
}

function observeCallLifecycle(nextCall) {
  const previous = lastObservedCall;
  lastObservedCall = nextCall ? { ...nextCall } : null;
  if (previous && (!nextCall || nextCall.id !== previous.id)) {
		directHangupInFlight = false;
    // CLCC can briefly publish voice-idle and then replay the just-ended call
    // before settling. Keep that exact call ID suppressed so the automatic
    // recorder cannot create a one- or two-second tail recording.
    recordingSuppressedCallID = previous.id;
    void stopCallRecording().catch(() => {});
    callMuted = false;
    callKeypadVisible = false;
    showCallEnded(previous);
    recordingLastError = "";
  }
  if (nextCall?.state === "active") void maybeStartAutomaticRecording(nextCall);
}

function applySessionPresentation(candidate) {
  const smsOnly = smsOnlySession(candidate);
  const callsTab = document.querySelector('[data-tab="calls"]');
  const recordingsTab = document.querySelector('[data-tab="recordings"]');
  const callsPanel = $("#calls");
  const recordingsPanel = $("#recordings");
  const wasSMSOnly = callsTab.hidden || callsPanel.hidden;
  callsTab.hidden = smsOnly;
  callsPanel.hidden = smsOnly;
  recordingsTab.hidden = smsOnly;
  recordingsPanel.hidden = smsOnly;
  $(".tabs").classList.toggle("sms-only", smsOnly);
  const activeTab = document.querySelector("[data-tab].active")?.dataset.tab || "home";
  if (smsOnly) {
    if (["calls", "recordings"].includes(activeTab)) selectTab("home");
    preparedMediaCallID = "";
    rescueMediaReceipt = null;
    externalVoiceClient?.close("sms-only-session");
    remoteMediaClient?.close("sms-only-session");
  } else if (wasSMSOnly && ["calls", "recordings"].includes(activeTab)) {
    selectTab("home");
  }
}

function externalVoiceSelected(candidate = session) {
  return !smsOnlySession(candidate) && candidate?.external_voice === true;
}

function externalVoiceMode(candidate = session) {
  return externalVoiceSelected(candidate) && candidate?.external_voice_api_version === 2;
}

function canExternalVoice(action, candidate = session) {
  return Boolean(externalVoiceMode(candidate) && candidate?.remote_control &&
    hasExplicitActionFor(candidate, "calls.read") && hasExplicitActionFor(candidate, action));
}

function externalVoiceRecoveryLocked() {
  return Boolean(externalVoiceStatus?.recovery_required ||
    externalVoiceStatus?.health === "manual_recovery_required");
}

function ensureExternalVoiceClient() {
  if (smsOnlySession()) return null;
  if (!externalVoiceClient && typeof globalThis.MacCellularExternalVoiceClient === "function") {
    externalVoiceClient = new globalThis.MacCellularExternalVoiceClient({
      audioElement: $("#remote-audio"),
      csrfToken: () => session?.csrf_token || "",
      onState: (state) => {
        if (!["outcome_unknown", "reconciling"].includes(state.phase)) {
          externalVoiceUnknownKind = "";
        }
        renderCall();
      },
    });
  }
  return externalVoiceClient;
}

function requestExternalVoiceSnapshot(client) {
  if (externalVoiceSnapshotInFlight?.client === client) return externalVoiceSnapshotInFlight.promise;
  const record = { client, promise: null };
  record.promise = client.refresh().finally(() => {
    if (externalVoiceSnapshotInFlight === record) externalVoiceSnapshotInFlight = null;
  });
  externalVoiceSnapshotInFlight = record;
  return record.promise;
}

async function waitForExternalVoiceSnapshot() {
  const pending = externalVoiceSnapshotInFlight?.promise;
  if (!pending) return;
  try { await pending; } catch {}
}

function beginExternalVoiceOperation() {
  if (externalVoiceOperationToken) return null;
  const token = {};
  externalVoiceOperationToken = token;
  try { renderCall(); } catch {}
  return token;
}

function finishExternalVoiceOperation(token) {
  if (externalVoiceOperationToken === token) externalVoiceOperationToken = null;
}

async function api(path, options = {}) {
  const {
    ephemeral = false,
    idempotencyKey = "",
    csrfToken = "",
    timeoutMilliseconds = 0,
    ...requestOptions
  } = options;
  const headers = { ...(options.headers || {}) };
  const isFormData = typeof FormData !== "undefined" && options.body instanceof FormData;
  if (options.body !== undefined && !isFormData) headers["Content-Type"] = "application/json";
  if (options.method && options.method !== "GET") {
    if (!ephemeral) headers["Idempotency-Key"] ||= idempotencyKey || requestID();
    headers["X-MacCellular-CSRF"] = csrfToken || session?.csrf_token || "";
  }
  const method = requestOptions.method || "GET";
  const effectiveTimeout = timeoutMilliseconds > 0 ? timeoutMilliseconds : (method === "GET" ? 15_000 : 0);
  const callerSignal = requestOptions.signal;
  const controller = effectiveTimeout > 0 ? new AbortController() : null;
  let timedOut = false;
  const abortFromCaller = () => controller?.abort();
  if (controller && callerSignal) {
    if (callerSignal.aborted) controller.abort();
    else callerSignal.addEventListener("abort", abortFromCaller, { once: true });
  }
  const timeout = controller ? setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, effectiveTimeout) : null;
  try {
    const response = await fetch(path, {
      ...requestOptions,
      headers,
      cache: "no-store",
      signal: controller?.signal || callerSignal,
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body.error || `HTTP ${response.status}`);
      error.httpStatus = response.status;
      error.code = body.code || "";
      throw error;
    }
    return body;
  } catch (error) {
    if (timedOut) {
      const timeoutError = new Error("蜂窝网络请求超时");
      timeoutError.code = "request_timeout";
      throw timeoutError;
    }
    throw error;
  } finally {
    if (timeout) clearTimeout(timeout);
    callerSignal?.removeEventListener?.("abort", abortFromCaller);
  }
}

const incomingCallPush = new globalThis.MacCellularIncomingCallPush.IncomingCallPushController({
  api,
  render(state) {
    $("#incoming-call-push").hidden = !state.visible;
    $("#incoming-call-push-toggle").disabled = state.busy;
    $("#incoming-call-push-toggle").textContent = state.label;
    $("#incoming-call-push-status").textContent = state.message;
    // Keep the legacy foreground-notification control untouched for SMS-only
    // and Tailscale sessions. Public voice uses the dedicated push control.
    $("#notify").hidden = state.visible;
  },
  onMessage: toast,
});

async function mutationFingerprint(path, body) {
  const bytes = new TextEncoder().encode(`${path}\0${JSON.stringify(body || {})}`);
  const hash = await crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(hash)].map((value) => value.toString(16).padStart(2, "0")).join("");
}

function readPendingMutations() {
  try { return JSON.parse(localStorage.getItem("maccellular.pending-mutations.v1") || "{}"); } catch { return {}; }
}

function writePendingMutations(value) {
  localStorage.setItem("maccellular.pending-mutations.v1", JSON.stringify(value));
}

async function pendingMutation(path, body) {
  const fingerprint = await mutationFingerprint(path, body);
  const pending = readPendingMutations();
  if (!pending[fingerprint]) {
    pending[fingerprint] = requestID();
    writePendingMutations(pending);
  }
  return { fingerprint, key: pending[fingerprint] };
}

function clearPendingMutation(fingerprint) {
  const pending = readPendingMutations();
  delete pending[fingerprint];
  writePendingMutations(pending);
}

function toast(message) {
  const element = $("#toast");
  element.textContent = message;
  element.classList.add("show");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => element.classList.remove("show"), 2600);
}

function safeNumber(raw) {
  return String(raw || "未知号码");
}

function formatTime(raw) {
  if (!raw) return "";
  const value = new Date(raw);
  return Number.isNaN(value.getTime()) ? "" : value.toLocaleString("zh-CN", { hour12: false });
}

function formatRecordingTitle(item) {
  const value = new Date(item?.started_at || "");
  const time = Number.isNaN(value.getTime())
    ? "通话录音"
    : value.toLocaleString("zh-CN", {
      month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false,
    });
  return `${time} · ${safeNumber(item?.number)}`;
}

function elapsedSeconds(startedAt, endedAt = Date.now()) {
  const start = Date.parse(startedAt || "");
  const end = typeof endedAt === "number" ? endedAt : Date.parse(endedAt || "");
  return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, Math.round((end - start) / 1000)) : 0;
}

function formatDuration(seconds) {
  const total = Math.max(0, Number.isFinite(seconds) ? Math.round(seconds) : 0);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const remainder = total % 60;
  return hours
    ? `${hours}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}`
    : `${minutes}:${String(remainder).padStart(2, "0")}`;
}

function stateLabel(state) {
  return ({ incoming: "来电", waiting: "等待中", active: "通话中", dialing: "正在拨号", alerting: "正在振铃", held: "已保持" })[state] || state || "通话";
}

function ensureCallAudioBars() {
  for (const selector of ["#remote-audio-wave", "#local-audio-wave"]) {
    const wave = $(selector);
    if (!wave || wave.children.length) continue;
    if (selector === "#remote-audio-wave") wave.classList.add("remote");
    for (let index = 0; index < 14; index += 1) wave.append(document.createElement("i"));
  }
}

function paintAudioWave(selector, level) {
  const bars = [...$(selector).children];
  const normalized = Math.max(0, Math.min(1, Number(level) || 0));
  bars.forEach((bar, index) => {
    const center = (bars.length - 1) / 2;
    const shape = 1 - Math.abs(index - center) / (center + 1);
    const height = Math.max(.12, Math.min(1, normalized * (1.15 + shape * .75)));
    bar.style.transform = `scaleY(${height.toFixed(2)})`;
    bar.style.opacity = String(Math.max(.24, Math.min(1, normalized * 1.6)));
  });
}

function stopCallAudioMeter() {
  if (callMeterFrame !== null) cancelAnimationFrame(callMeterFrame);
  callMeterFrame = null;
  callMeterLastPaint = 0;
  if ($("#remote-audio-wave")?.children.length) paintAudioWave("#remote-audio-wave", 0);
  if ($("#local-audio-wave")?.children.length) paintAudioWave("#local-audio-wave", 0);
}

function startCallAudioMeter() {
  ensureCallAudioBars();
  if (callMeterFrame !== null) return;
  const paint = (timestamp) => {
    const meter = $("#call-audio-meter");
    if (!meter || meter.hidden || document.hidden) {
      stopCallAudioMeter();
      return;
    }
    if (timestamp - callMeterLastPaint >= 80) {
      callMeterLastPaint = timestamp;
      const levels = remoteMediaClient?.audioLevels?.() || { local: 0, remote: 0 };
      const local = callMuted ? 0 : levels.local;
      paintAudioWave("#remote-audio-wave", levels.remote);
      paintAudioWave("#local-audio-wave", local);
      $("#remote-audio-label").textContent = levels.remote > .025 ? "有声音" : "安静";
      $("#local-audio-label").textContent = callMuted ? "已静音" : (local > .025 ? "正在说话" : "安静");
    }
    callMeterFrame = requestAnimationFrame(paint);
  };
  callMeterFrame = requestAnimationFrame(paint);
}

function renderCallJourney({ stage = 0, tone = "ringing", title = "等待通话", detail = "", meter = false } = {}) {
  const journey = $("#call-journey");
  if (!journey) return;
  journey.dataset.tone = tone;
  $("#call-visual-title").textContent = title;
  $("#call-visual-detail").textContent = detail;
  $("#call-visual-icon").textContent = ({ ringing: "☎", connecting: "↻", live: "♫", ending: "■" })[tone] || "•";
  const steps = [...journey.querySelectorAll("[data-call-step]")];
  steps.forEach((step, index) => {
    step.classList.toggle("done", index < stage || stage >= steps.length);
    step.classList.toggle("current", index === stage && stage < steps.length);
  });
  $("#call-audio-meter").hidden = !meter;
  if (meter) startCallAudioMeter(); else stopCallAudioMeter();
}

function groupMessages(items) {
  const groups = new Map();
  for (const item of items || []) {
    const peer = safeNumber(globalThis.MacCellularRemoteSMSMessagePeer(item));
    if (!groups.has(peer)) groups.set(peer, []);
    groups.get(peer).push(item);
  }
  return [...groups.entries()].map(([peer, messages]) => ({ peer, messages, last: messages[0] }));
}

function remoteSMSSyncActive(candidate = session) {
  return candidate?.sms_sync === true && hasActionFor(candidate, "sms.read");
}

function currentSMSItems() {
  return remoteSMSSyncActive() ? remoteSMSState.items() : (snapshot?.sms?.items || []);
}

function renderGateway() {
  const gateway = snapshot?.gateway || {};
  $("#gateway-dot").classList.toggle("online", Boolean(gateway.connected));
  $("#gateway-state").textContent = gateway.connected ? "家中 Mac 网关在线" : "模块暂未连接";
  let voice;
  if (externalVoiceSelected()) {
    const labels = {
      connected: "外置语音已连接",
      starting: "外置语音正在启动",
      reconnecting: "外置语音正在重连",
      manual_recovery_required: "外置语音需人工恢复（控制已锁定）",
      closed: "外置语音已关闭",
      unavailable: "外置语音状态不可用",
      permission_locked: "外置语音权限不足（控制已锁定）",
      unsupported_api: "外置语音 API 不兼容（控制已锁定）",
    };
    voice = labels[externalVoiceStatus?.health] || "外置语音等待状态";
  } else {
    voice = gateway.voice_ready ? "语音路由已就绪" : `语音路由：${gateway.voice_phase || "未初始化"}`;
  }
  const service = smsOnlySession() ? "短信模式" : voice;
  $("#gateway-detail").textContent = `${service} · ${sessionPresentation.transportLabel(session)}`;
  $("#line-transport").textContent = sessionPresentation.transportLabel(session).replace(" 公网", "");
  $("#line-voice").textContent = smsOnlySession() ? "未启用" : (gateway.voice_ready || externalVoiceStatus?.health === "connected" ? "已就绪" : "准备中");
  $("#line-sms").textContent = remoteSMSSyncActive() || hasAction("sms.read") ? "已连接" : "未启用";
  $("#identity").textContent = session?.identity || "--";
  const externalDialEnabled = externalVoiceMode() && externalVoiceStatus?.health === "connected" &&
    externalVoiceStatus?.dial_enabled === true &&
    (!externalVoiceStatus?.call || externalVoiceStatus.call.phase === "ended") &&
    canExternalVoice("calls.control") && canExternalVoice("calls.media");
  const directDialEnabled = !externalVoiceMode() && !smsOnlySession() && !snapshot?.calls?.active &&
    Number.isSafeInteger(snapshot?.calls?.call_generation) && snapshot.calls.call_generation > 0 &&
    session?.media_offer === true && can("calls.control") && can("calls.media");
  const directDTMFEnabled = !externalVoiceMode() && snapshot?.calls?.active?.state === "active" &&
    can("calls.control");
  const externalDTMFEnabled = externalVoiceMode() && externalVoiceStatus?.dtmf_enabled === true &&
    ["active_unverified", "active_transport_verified"].includes(externalVoiceStatus?.call?.phase);
  const dialEnabled = externalDialEnabled || directDialEnabled;
  $("#dial-form").classList.toggle("in-call", directDTMFEnabled || externalDTMFEnabled);
  $("#dial-form").querySelector('button[type="submit"]').disabled = !dialEnabled;
  $("#dial-form").querySelector("label").textContent = directDTMFEnabled || externalDTMFEnabled
    ? "通话按键" : (externalDialEnabled ? "使用外置 SIP 网关拨出" : "使用家中模块拨出");
  $("#dial-help").textContent = directDTMFEnabled || externalDTMFEnabled
    ? "通话进行中；下方数字、*、# 会直接发送到电话服务菜单。"
    : externalVoiceMode()
    ? (externalDialEnabled
      ? "通过已连接的蜂窝语音网关拨号，媒体使用公网 TURN 中继。"
      : "外置语音尚未连接或未开放主动拨号。")
    : (session?.media_offer === true && can("calls.control") && can("calls.media")
      ? "通过家中的 DJI 模块拨号，通话音频使用公网 TURN 中继。"
      : "当前会话尚未开放网页拨号。");
  $("#new-message").querySelector('button[type="submit"]').disabled = !can("sms.send");
  $("#thread-send").disabled = !can("sms.send");
  renderProductOverview();
}

function renderExternalVoiceCall() {
  const card = $("#active-call");
  const voice = externalVoiceStatus;
  const call = voice?.call;
  const local = externalVoiceClient?.snapshot() || { phase: "idle" };
  const recoveryLocked = externalVoiceRecoveryLocked();
  const policyLocked = ["permission_locked", "unsupported_api"].includes(voice?.health);
  const callLive = Boolean(call && call.phase !== "ended");
  card.hidden = !(callLive || recoveryLocked || policyLocked);

  $("#prepare-media").textContent = "连接麦克风";
  $("#prepare-media").hidden = true;
  $("#prepare-media").disabled = true;
  $("#answer").hidden = true;
  $("#answer").disabled = true;
  $("#reject").hidden = true;
  $("#reject").disabled = true;
  $("#hangup").hidden = true;
  $("#hangup").disabled = true;
  $("#reconcile-voice").hidden = true;
  $("#reconcile-voice").disabled = true;

  if (recoveryLocked) {
    $("#call-state").textContent = "需人工恢复";
    $("#call-number").textContent = "外置语音网关状态未知";
    $("#call-duration").textContent = "所有接听、挂断与媒体操作已锁定";
    $("#media-state").textContent = "请先在 Asterisk / 外置网关核对真实通话并完成恢复，再重启语音服务。";
    renderCallJourney({ stage: 1, tone: "ending", title: "通话状态需要核对", detail: "控制已暂停，避免误操作" });
    return;
  }
  if (policyLocked) {
    $("#call-state").textContent = "外置语音已锁定";
    $("#call-number").textContent = "需要 v2 与显式通话权限";
    $("#call-duration").textContent = "旧 QPCMV 控制不会作为后备启用";
    $("#media-state").textContent = voice.health === "unsupported_api"
      ? "会话未声明 external_voice_api_version = 2，所有外置语音操作已锁定。"
      : "需要显式 calls.read；媒体、接听与挂断还分别需要 calls.media、calls.control、calls.hangup。";
    renderCallJourney({ stage: 1, tone: "ending", title: "语音服务未就绪", detail: "当前不会执行通话操作" });
    return;
  }
  if (!callLive) return;

  const controls = publicVoiceUI.foregroundCallControls({
    session,
    voice,
    local,
    unknownKind: externalVoiceUnknownKind,
    operationBusy: Boolean(externalVoiceOperationToken),
  });
  const { ringing, active, unknown } = controls;
  const authoritativeMediaReady = call.phase === "media_ready" && call.media?.prepared === true;
  const stateLabels = {
    incoming_ringing: call.direction === "outgoing" ? "外置网关正在拨号" : "外置网关来电",
    media_preparing: "正在准备媒体",
    media_ready: "音频已准备",
    answer_pending: "正在接听",
    reconciling: "正在核对",
    active_unverified: "通话中（媒体待验证）",
    active_transport_verified: "通话中",
    active_unmanaged: "外置通话由其他端管理",
    ending: "正在结束",
  };
  $("#call-state").textContent = stateLabels[call.phase] || "外置语音通话";
  $("#call-number").textContent = call.direction === "outgoing" ? "外置网关外呼" : "外置网关来电";
  $("#call-duration").textContent = voice.observed_at ? `状态更新于 ${formatTime(voice.observed_at)}` : "";
  const externalStage = ["incoming_ringing"].includes(call.phase) ? 0
    : ["media_preparing", "media_ready", "answer_pending", "reconciling"].includes(call.phase) ? 1
    : 3;
  const externalTone = call.phase === "ending" ? "ending"
    : externalStage === 0 ? "ringing" : externalStage === 1 ? "connecting" : "live";
  renderCallJourney({
    stage: externalStage,
    tone: externalTone,
    title: stateLabels[call.phase] || "外置语音通话",
    detail: externalStage === 0 ? "等待接听"
      : externalStage === 1 ? "正在建立浏览器与家中网关的音频链路"
      : call.phase === "ending" ? "正在等待网关确认通话结束" : "通话与音频链路已建立",
  });

  $("#prepare-media").hidden = !controls.prepare_visible;
  $("#prepare-media").disabled = !controls.prepare_enabled;
  $("#answer").hidden = !controls.answer_visible;
  $("#answer").disabled = !controls.answer_enabled || !authoritativeMediaReady;
  $("#reject").hidden = !controls.reject_visible;
  $("#reject").disabled = !controls.reject_enabled;
  $("#hangup").hidden = !controls.end_visible;
  $("#hangup").disabled = !controls.end_enabled;
  $("#reconcile-voice").hidden = !controls.reconcile_visible;
  $("#reconcile-voice").disabled = !controls.reconcile_enabled;

  if (unknown) {
    $("#media-state").textContent = "上一次操作结果未知；不会自动重试。请点“核对结果”进行只读收敛。";
  } else if (externalVoiceStatusError) {
    $("#media-state").textContent = "无法确认最新外置语音状态；本地音频暂保留，所有控制保持锁定。";
  } else if (local.phase === "prepared" && controls.answer_enabled) {
    $("#media-state").textContent = "麦克风与外置网关音频已连接，可手动接听。";
  } else if (local.phase === "active") {
    $("#media-state").textContent = call.media?.bidirectional_fresh
      ? "双向媒体已验证，可手动挂断。"
      : "通话已建立，正在等待双向媒体验证。";
  } else if (call.phase === "active_unmanaged") {
    $("#media-state").textContent = "此通话没有本页持有的媒体租约，远程控制保持锁定。";
  } else {
    $("#media-state").textContent = controls.prepare_enabled
      ? "点“连接麦克风”建立一次性浏览器媒体，再手动接听。"
      : "当前状态或显式权限不足，外置语音控制保持锁定。";
  }
}

function renderCall() {
  if (smsOnlySession()) {
    stopCallAudioMeter();
    $("#active-call").hidden = true;
    for (const selector of ["#prepare-media", "#answer", "#reject", "#hangup", "#reconcile-voice"]) {
      $(selector).hidden = true;
      $(selector).disabled = true;
    }
    return;
  }
  const active = snapshot?.calls?.active;
  if (externalVoiceSelected()) {
    rescueMediaReceipt = null;
    preparedMediaCallID = "";
    if (remoteMediaClient && remoteMediaClient.snapshot().phase !== "idle") remoteMediaClient.close("external-voice-priority");
    renderExternalVoiceCall();
  } else {
    if (externalVoiceClient && externalVoiceClient.snapshot().phase !== "idle") externalVoiceClient.close("external-voice-disabled");
    externalVoiceStatus = null;
    externalVoiceStatusError = "";
    externalVoiceUnknownKind = "";
    if (!active || (rescueMediaReceipt && rescueMediaReceipt.call_id !== active.id)) {
      rescueMediaReceipt = null;
    }
    const localDirectMedia = remoteMediaClient?.snapshot() || { phase: "idle" };
    const outgoingPreparationCurrent = !active && localDirectMedia.purpose === "outgoing" &&
      localDirectMedia.call_generation === snapshot?.calls?.call_generation;
    if ((!active || (preparedMediaCallID && active.id !== preparedMediaCallID)) && !outgoingPreparationCurrent &&
        remoteMediaClient && localDirectMedia.phase !== "idle") {
      preparedMediaCallID = "";
      remoteMediaClient.close("call-changed");
    }
    const card = $("#active-call");
    card.hidden = !active;
    $("#prepare-media").textContent = "准备通话";
    $("#reconcile-voice").hidden = true;
    $("#reconcile-voice").disabled = true;
    if (active) {
      void remoteMediaClient?.ensurePlayback?.();
      $("#call-state").textContent = stateLabel(active.state);
      $("#call-number").textContent = safeNumber(active.number);
      $("#call-duration").textContent = active.started_at
        ? (active.state === "active" ? `通话时长 ${formatDuration(elapsedSeconds(active.started_at))}` : `开始于 ${formatTime(active.started_at)}`)
        : "";
      const ringing = active.state === "incoming" || active.state === "waiting";
      $("#answer").hidden = !ringing;
      $("#reject").hidden = !ringing;
      $("#hangup").hidden = ringing;
      $("#answer").disabled = true;
      $("#reject").disabled = true;
      $("#hangup").disabled = true;
      const remoteMedia = snapshot?.calls?.remote_media || {};
      const localMedia = remoteMediaClient?.snapshot() || { phase: "idle" };
      if (rescueMediaReceipt && localMedia.phase === "idle" && !rescueMediaReceipt.expires_at) {
        rescueMediaReceipt.expires_at = Date.now() + 30_000;
      }
      if (rescueMediaReceipt?.expires_at && Date.now() >= rescueMediaReceipt.expires_at) {
        rescueMediaReceipt = null;
      }
      const mediaPhase = localMedia.phase;
      const mayPrepare = ringing && Boolean(session?.media_offer) && can("calls.media") &&
        (mediaPhase === "idle" || preparedMediaCallID === active.id);
      const answerReady = ringing && Boolean(session?.incoming_answer) && can("calls.control") && can("calls.media") &&
        remoteMedia.call_action_ready === true && localMedia.phase === "prepared" &&
        localMedia.media_session_id && localMedia.lease_generation === remoteMedia.lease_generation &&
        preparedMediaCallID === active.id;
      const authenticatedHangupReady = active.state === "active" && Boolean(session?.rescue_hangup) &&
        can("calls.hangup") && can("calls.media");
      const inCall = active.state === "active";
      const answerConfirmed = directAnsweredCallID === active.id;
      const answerElapsed = directAnswerStartedAt ? Math.floor((Date.now() - directAnswerStartedAt) / 1000) : 0;
      const answerRemaining = Math.max(1, 12 - answerElapsed);
      $("#answer").textContent = answerConfirmed
        ? "已接听，正在建立通话…"
        : directAnswerInFlight
          ? (answerElapsed < 12 ? `正在接听（约 ${answerRemaining} 秒）` : "正在等待模块确认…")
          : "接听";
      $("#answer").disabled = answerConfirmed || directAnswerInFlight || !(answerReady || mayPrepare);
      $("#answer").hidden = !ringing || answerConfirmed || directAnswerInFlight;
      $("#reject").hidden = !ringing || answerConfirmed || directAnswerInFlight;
      $("#hangup").textContent = directHangupInFlight ? "正在挂断…" : "挂断";
      $("#hangup").disabled = directHangupInFlight || !authenticatedHangupReady;
      $("#in-call-tools").hidden = !inCall;
      $("#call-mute").classList.toggle("active", callMuted);
      $("#call-mute").querySelector("small").textContent = callMuted ? "取消静音" : "静音";
      $("#call-keypad").classList.toggle("active", callKeypadVisible);
      const recorder = ensureRecordingServices();
      $("#call-record").disabled = !inCall || externalVoiceSelected() || !recorder?.supported?.();
      $("#call-record").classList.toggle("active", Boolean(recorder?.active?.()));
      $("#call-record").querySelector("small").textContent = recorder?.active?.()
        ? "自动录音中"
        : recordingStartInFlight ? "正在启动" : recordingLastError || "自动录音";
      $("#dial-form").hidden = inCall && !callKeypadVisible;
      $("#dial-form").classList.toggle("in-call", inCall);
      $("#prepare-media").hidden = true;
      $("#prepare-media").disabled = !mayPrepare || mediaPhase !== "idle";
		if (answerConfirmed) {
			$("#media-state").textContent = "接听已确认，正在等待运营商通话状态更新";
		} else if (directAnswerInFlight) {
			$("#media-state").textContent = "正在自动准备并接听，通常约 5–12 秒，无需再次点击";
      } else if (remoteMedia.transport_prepared && remoteMedia.host_uac_prepared) {
        $("#media-state").textContent = answerReady
          ? "浏览器、Mac UAC 与来电租约已准备，可受控接听"
          : "浏览器与 Mac UAC 描述符已准备；接听仍等待同一来电租约确认";
      } else if (mediaPhase === "prepared" || remoteMedia.transport_prepared) {
        $("#media-state").textContent = "浏览器媒体已连接，正在等待 Mac 端同设备 UAC 校验";
      } else {
				$("#media-state").textContent = "点击“接听”后会自动完成通话连接";
      }
      const mediaConnected = mediaPhase === "prepared" && localMedia.remote_track === true;
      if (directHangupInFlight) {
        renderCallJourney({ stage: 3, tone: "ending", title: "正在挂断", detail: "等待蜂窝网络确认通话结束" });
      } else if (inCall && mediaConnected) {
        renderCallJourney({
          stage: 3,
          tone: "live",
          title: "双向通话已连接",
          detail: "下方波形来自实时收音；静音时会保持平稳",
          meter: true,
        });
      } else if (inCall || directAnswerInFlight || answerConfirmed || ["requesting-microphone", "offering", "prepared"].includes(mediaPhase)) {
        renderCallJourney({
          stage: 1,
          tone: "connecting",
          title: inCall ? "通话已接通，正在连接音频" : "正在建立音频链路",
          detail: "麦克风、公网中继与家中 Mac 正在自动连接",
        });
      } else {
        renderCallJourney({
          stage: 0,
          tone: "ringing",
          title: ringing ? "有新来电" : (active.state === "alerting" ? "对方正在振铃" : "正在建立蜂窝通话"),
          detail: ringing ? "点击接听后，音频会自动连接" : "等待运营商返回通话状态",
        });
      }
    } else {
      if (directDTMFCallID || directDTMFBuffer) resetDirectDTMF();
      renderCallJourney({ stage: 0, tone: "ringing", title: "等待通话", detail: "来电或拨号后会显示连接进度" });
      $("#in-call-tools").hidden = true;
      $("#dial-form").hidden = false;
    }
  }

  const history = snapshot?.calls?.history || [];
  const missedCount = history.filter((item) => item.missed).length;
  $("#missed-count").textContent = missedCount ? `${missedCount} 个未接来电` : "";
  renderProductOverview();
  const list = $("#call-history");
  list.replaceChildren();
  if (!history.length) {
    list.className = "empty";
    list.textContent = "暂无通话记录";
    return;
  }
  list.className = "history-list";
  for (const item of history.slice(0, 30)) {
    const row = document.createElement("button");
    row.type = "button";
    row.className = `history-row${item.missed ? " missed" : ""}`;
    const copy = document.createElement("div");
    const number = document.createElement("strong");
    number.textContent = safeNumber(item.number);
    const meta = document.createElement("small");
    const historyDuration = item.ended_at ? elapsedSeconds(item.started_at, item.ended_at) : 0;
    meta.textContent = [
      item.direction === "incoming" ? (item.missed ? "未接来电" : "已接来电") : "呼出",
      formatTime(item.started_at),
      historyDuration ? formatDuration(historyDuration) : "",
    ].filter(Boolean).join(" · ");
    copy.append(number, meta);
    const call = document.createElement("span");
    call.className = "secondary small";
    call.textContent = "回拨";
    const callable = Boolean(item.number && item.number !== "未知号码" && !snapshot?.calls?.active &&
      ((externalVoiceMode() && externalVoiceStatus?.dial_enabled === true && canExternalVoice("calls.control") &&
        canExternalVoice("calls.media")) || (!externalVoiceMode() && session?.media_offer === true &&
        can("calls.control") && can("calls.media"))));
    row.disabled = !callable;
    call.title = callable ? `回拨 ${safeNumber(item.number)}` : "当前不能回拨";
    row.addEventListener("click", async () => {
      if (!callable) return;
      row.disabled = true;
      $("#dial-number").value = item.number;
      try {
        await dial(item.number);
        $("#dial-number").value = "";
      } catch (error) {
        toast(globalThis.MacCellularPublicVoiceUI?.directVoiceErrorMessage?.(error) || "回拨失败，请稍后重试");
      }
    });
    row.append(copy, call);
    list.append(row);
  }
}

function renderMessages() {
  const items = currentSMSItems();
  const groups = groupMessages(items);
  const syncLabel = remoteSMSSyncActive()
    ? (remoteSMSState.ready ? "已增量同步" : "正在同步历史")
    : (formatTime(snapshot?.sms?.last_poll) || "尚未同步");
  $("#sms-meta").textContent = `已保存 ${items.length} 条 · ${groups.length} 个会话`;
  const latest = items[0]?.timestamp ? formatTime(items[0].timestamp) : "无记录";
  const pollTime = formatTime(snapshot?.sms?.last_poll);
  const pollFailed = Boolean(snapshot?.sms?.last_poll_error);
  $("#sms-summary").textContent = pollFailed
    ? `模块读取失败；当前显示已保存记录。最新短信：${latest}`
    : `${syncLabel}${pollTime ? ` · 模块读取于 ${pollTime}` : ""}。列表按号码合并为 ${groups.length} 个会话，最新记录 ${latest}。`;
  $("#sms-refresh").disabled = !can("sms.read");
  renderProductOverview();
  const container = $("#conversations");
  container.replaceChildren();
  if (!groups.length) {
    container.className = "empty";
    container.textContent = "暂无短信";
    return;
  }
  container.className = "conversation-list";
  for (const group of groups) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "conversation";
    const title = document.createElement("strong");
    const peer = document.createElement("span");
    peer.textContent = group.peer;
    const count = document.createElement("span");
    count.className = "conversation-count";
    count.textContent = String(group.messages.length);
    title.append(peer, count);
    const time = document.createElement("time");
    time.textContent = formatTime(group.last.timestamp);
    const preview = document.createElement("span");
    preview.textContent = group.last.content || "";
    button.append(title, time, preview);
    button.addEventListener("click", () => openThread(group.peer, group.messages));
    container.append(button);
  }
  if ($("#thread-dialog").open) {
    const selected = groups.find((group) => group.peer === selectedPeer);
    if (selected) renderThread(selected.peer, selected.messages);
    else $("#thread-dialog").close();
  }
}

function openThread(peer, messages) {
  selectedPeer = peer;
  renderThread(peer, messages);
  if (!$("#thread-dialog").open) $("#thread-dialog").showModal();
}

function renderThread(peer, messages) {
  $("#thread-peer").textContent = peer;
  const list = $("#thread-list");
  list.replaceChildren();
  for (const item of [...messages].reverse()) {
    const bubble = document.createElement("div");
    bubble.className = `bubble ${item.direction === "outgoing" ? "sent" : "received"}`;
    const content = document.createElement("span");
    content.textContent = item.content || "";
    const time = document.createElement("time");
    const status = item.direction === "outgoing" ? ({
      pending: "待提交",
      submitted: "已提交到模块",
      failed: "未发送",
      unknown: "结果待核对",
    })[item.status] : "";
    time.textContent = [formatTime(item.timestamp), status].filter(Boolean).join(" · ");
    bubble.append(content, time);
    list.append(bubble);
  }
  list.scrollTop = list.scrollHeight;
}

function notifyChanges(incomingSMS = []) {
  const notificationsAllowed = "Notification" in window && Notification.permission === "granted";
  const backgroundCallNotificationActive = incomingCallPush.enabled();
  if (smsOnlySession()) {
    previousCallID = "";
    previousExternalCallID = "";
  } else if (externalVoiceSelected()) {
    const transition = publicVoiceUI.incomingCallTransition(previousExternalCallID, externalVoiceStatus);
    previousExternalCallID = transition.next_call_id;
    if (transition.notify && notificationsAllowed && !backgroundCallNotificationActive) {
      void publicVoiceUI.showForegroundNotification("外置网关来电", {
        body: "打开 MacCellular 查看并处理",
        tag: "external-voice-incoming",
        icon: "icon.svg",
      });
    }
    previousCallID = "";
  } else {
    const active = snapshot?.calls?.active;
    if (active?.id && active.id !== previousCallID && (active.state === "incoming" || active.state === "waiting")) {
      previousCallID = active.id;
      if (notificationsAllowed && !backgroundCallNotificationActive) {
        void publicVoiceUI.showForegroundNotification("模块来电", {
          body: "打开 MacCellular 查看并处理", tag: "legacy-call-incoming", icon: "icon.svg",
        });
      }
    } else if (!active) {
      previousCallID = "";
    }
    previousExternalCallID = "";
  }
  if (remoteSMSSyncActive()) {
    const newest = incomingSMS.sort((left, right) => Date.parse(right.timestamp) - Date.parse(left.timestamp))[0];
    if (newest && notificationsAllowed) {
      void publicVoiceUI.showForegroundNotification("模块收到新短信", {
        body: "打开 MacCellular 查看内容",
        tag: `sms-${newest.event_id || newest.id}`,
        icon: "icon.svg",
      });
    }
    previousLegacyMessageKey = "";
    return;
  }
  const first = snapshot?.sms?.items?.[0];
  const key = first ? `${first.timestamp}` : "";
  if (previousLegacyMessageKey && key && key !== previousLegacyMessageKey && notificationsAllowed) {
    void publicVoiceUI.showForegroundNotification("模块收到新短信", {
      body: "打开 MacCellular 查看内容", tag: `sms-${key}`, icon: "icon.svg",
    });
  }
  previousLegacyMessageKey = key;
}

function clearSensitiveView() {
  if (callRecorder?.active()) void stopCallRecording();
  viewEpoch += 1;
  session = null;
  snapshot = null;
  selectedPeer = "";
  previousCallID = "";
  previousExternalCallID = "";
  previousLegacyMessageKey = "";
  remoteSMSState.clear();
  preparedMediaCallID = "";
  rescueMediaReceipt = null;
  externalVoiceStatus = null;
  externalVoiceStatusError = "";
  externalVoiceUnknownKind = "";
  externalVoiceIdentity = "";
  externalVoiceClient?.close("snapshot-unavailable");
  externalVoiceOperationToken = null;
  externalVoiceSnapshotInFlight = null;
  remoteMediaClient?.close("snapshot-unavailable");
  lastObservedCall = null;
  autoPrepareAttemptedCallID = "";
  resetDirectDTMF();
  incomingCallPush.clear();
  if ($("#thread-dialog").open) $("#thread-dialog").close();
  renderCall();
  renderMessages();
  $("#identity").textContent = "--";
  $("#active-call").hidden = true;
  $("#call-state").textContent = "通话";
  $("#call-number").textContent = "未知号码";
  $("#call-duration").textContent = "";
	$("#media-state").textContent = "等待来电";
  for (const selector of ["#prepare-media", "#answer", "#reject", "#hangup", "#reconcile-voice"]) {
    $(selector).hidden = true;
    $(selector).disabled = true;
  }
  $("#thread-peer").textContent = "短信";
  $("#thread-list").replaceChildren();
  $("#sms-phone").value = "";
  $("#sms-text").value = "";
  $("#thread-text").value = "";
  $("#dial-number").value = "";
  $("#new-message").querySelector('button[type="submit"]').disabled = true;
  $("#thread-send").disabled = true;
  $("#toast").textContent = "";
  $("#toast").classList.remove("show");
}

async function syncRemoteSMS(nextSession, epoch, resetAttempted = false) {
  if (!remoteSMSSyncActive(nextSession)) {
    remoteSMSState.clear();
    return [];
  }
  const incoming = [];
  for (let pageIndex = 0; pageIndex < smsSyncMaximumPagesPerRefresh; pageIndex += 1) {
    const cursor = remoteSMSState.cursor;
    const query = new URLSearchParams({ limit: String(smsSyncPageLimit) });
    if (cursor) query.set("cursor", cursor);
    let page;
    try {
      page = await api(`../api/remote/v1/sms/sync?${query}`);
    } catch (error) {
      if (!resetAttempted && (error.code === "invalid_cursor" || error.code === "reset_required")) {
        if (document.hidden || epoch !== viewEpoch) return null;
        remoteSMSState.clear();
        return syncRemoteSMS(nextSession, epoch, true);
      }
      throw error;
    }
    if (document.hidden || epoch !== viewEpoch) return null;
    const notify = remoteSMSState.ready && page.bootstrap !== true;
    incoming.push(...remoteSMSState.applyPage(page, { reset: !cursor, notify }));
    if (!page.has_more) break;
  }
  return incoming;
}

async function refreshExternalVoice(nextSession, epoch) {
  if (externalVoiceIdentity && externalVoiceIdentity !== nextSession?.identity) {
    externalVoiceClient?.close("external-voice-identity-changed");
    externalVoiceStatus = null;
    externalVoiceStatusError = "";
    externalVoiceUnknownKind = "";
  }
  externalVoiceIdentity = typeof nextSession?.identity === "string" ? nextSession.identity : "";
  if (!externalVoiceSelected(nextSession)) {
    externalVoiceStatus = null;
    externalVoiceStatusError = "";
    externalVoiceUnknownKind = "";
    externalVoiceClient?.close("external-voice-unavailable");
    return true;
  }
  if (!externalVoiceMode(nextSession) || !hasExplicitActionFor(nextSession, "calls.read")) {
    externalVoiceClient?.close("external-voice-policy-locked");
    externalVoiceStatusError = "policy_locked";
    externalVoiceStatus = {
      enabled: false,
      health: externalVoiceMode(nextSession) ? "permission_locked" : "unsupported_api",
      call: null,
      owned_by_requester: false,
      answer_enabled: false,
      end_enabled: false,
      recovery_required: false,
    };
    return true;
  }
  const client = ensureExternalVoiceClient();
  if (!client) {
    externalVoiceStatusError = "client_unavailable";
    externalVoiceStatus = {
      enabled: false,
      health: "unavailable",
      call: null,
      owned_by_requester: false,
      answer_enabled: false,
      end_enabled: false,
      recovery_required: false,
    };
    return true;
  }
  if (externalVoiceOperationToken) return true;
  // An unknown answer/end outcome is reconciled only by the operator button.
  // Periodic polling must not silently clear or resubmit that in-memory command.
  const localPhase = client.snapshot().phase;
  if (localPhase === "outcome_unknown") {
    try {
      const response = await api("/api/remote/v2/voice/snapshot");
      if (document.hidden || epoch !== viewEpoch) return false;
      if (response?.version === 2 && response.voice?.recovery_required === true &&
          response.voice?.health === "manual_recovery_required") {
        externalVoiceStatus = response.voice;
        externalVoiceStatusError = "";
        client.close("manual-recovery-required");
      }
    } catch {}
    return true;
  }
  if (["command_pending", "reconciling"].includes(localPhase)) return true;
  try {
    const response = await requestExternalVoiceSnapshot(client);
    if (document.hidden || epoch !== viewEpoch) return false;
    externalVoiceStatus = response.voice;
    externalVoiceStatusError = "";
  } catch (error) {
    if (document.hidden || epoch !== viewEpoch) return false;
    if (error?.code === "invalid_contract") client.close("external-voice-contract-failed");
    externalVoiceStatusError = error?.code || "snapshot_failed";
    externalVoiceStatus = {
      ...externalVoiceStatus,
      enabled: false,
      health: "unavailable",
      answer_enabled: false,
      end_enabled: false,
      recovery_required: false,
    };
  }
  return true;
}

async function refresh(options = {}) {
  const force = options.force === true;
  if (document.hidden) return;
  if (force) {
    viewEpoch += 1;
    refreshAbortController?.abort();
    refreshAbortController = null;
    polling = false;
    callPolling = false;
  }
  if (polling || callPolling) return;
  polling = true;
  const controller = new AbortController();
  refreshAbortController = controller;
  const epoch = viewEpoch;
  try {
    let nextSession = session;
    if (!nextSession) nextSession = await api("../api/remote/v1/session", { signal: controller.signal });
    const nextSnapshot = await api("../api/remote/v1/snapshot", { signal: controller.signal });
    const incomingSMS = await syncRemoteSMS(nextSession, epoch);
    if (incomingSMS === null) return;
    if (!await refreshExternalVoice(nextSession, epoch)) return;
    if (document.hidden || epoch !== viewEpoch) return;
    session = nextSession;
    if (!recordingsLoadedFromMac && nextSession?.media_offer === true) {
      recordingsLoadedFromMac = true;
      void loadRecordings({ includeMac: true });
    }
    observeCallLifecycle(nextSnapshot?.calls?.active || null);
    snapshot = nextSnapshot;
    applySessionPresentation(nextSession);
    void incomingCallPush.applySession(nextSession);
    renderGateway();
    renderCall();
    renderMessages();
    notifyChanges(incomingSMS);
    maybePrepareIncomingMedia();
  } catch (error) {
    if (document.hidden || epoch !== viewEpoch) return;
    session = null;
    clearSensitiveView();
    $("#gateway-dot").classList.remove("online");
    $("#gateway-state").textContent = "无法连接网关";
    $("#gateway-detail").textContent = error.message;
  } finally {
    if (refreshAbortController === controller) {
      refreshAbortController = null;
      polling = false;
    }
  }
}

async function refreshCallSnapshot() {
  if (document.hidden || polling || callPolling || !session || externalVoiceSelected() ||
      session?.incoming_answer !== true || session?.media_offer !== true) return;
  callPolling = true;
  const epoch = viewEpoch;
  try {
    const nextSnapshot = await api("../api/remote/v1/snapshot");
    if (document.hidden || epoch !== viewEpoch) return;
    observeCallLifecycle(nextSnapshot?.calls?.active || null);
    snapshot = nextSnapshot;
    renderCall();
    maybePrepareIncomingMedia();
  } catch {
    // The normal five-second refresh owns connection error presentation.
  } finally {
    callPolling = false;
  }
}

async function mutate(path, body, success, options = {}) {
  const epoch = viewEpoch;
  const pending = await pendingMutation(path, body);
  try {
    await api(`../api/remote/v1/${path}`, {
      method: "POST",
      body: JSON.stringify(body || {}),
      idempotencyKey: pending.key,
      timeoutMilliseconds: options.timeoutMilliseconds || 0,
    });
    clearPendingMutation(pending.fingerprint);
    if (!document.hidden && epoch === viewEpoch) {
      toast(success);
      await refresh();
    }
  } catch (error) {
    // A received HTTP response proves the server reached a terminal ledger
    // state. Only a lost network response keeps the same key for the retry.
    if (error.httpStatus && error.code !== "unknown_outcome") clearPendingMutation(pending.fingerprint);
    if (error.code === "unknown_outcome" && !document.hidden && epoch === viewEpoch) {
      await refresh();
    }
    const message = ({
      unknown_outcome: "操作结果不可确认，为避免重复执行已锁定；请先在模块端核对",
      conflict: "当前状态不允许该操作",
      modem_unavailable: "模块暂不可用",
      service_unavailable: "网关服务暂不可用",
    })[error.code] || error.message;
    if (!document.hidden && epoch === viewEpoch && !(options.suppressNetworkToast && !error.httpStatus)) {
      toast(message);
    }
    throw error;
  }
}

async function dial(number) {
  if (!number || number === "未知号码") return;
  if (externalVoiceSelected()) {
    if (externalVoiceStatus?.dial_enabled !== true || !canExternalVoice("calls.control") ||
        !canExternalVoice("calls.media") ||
        (externalVoiceStatus?.call && externalVoiceStatus.call.phase !== "ended")) {
      return toast("外置网关拨号尚未启用");
    }
    const epoch = viewEpoch;
    try {
      const result = await api("/api/remote/v2/voice/calls/dial", {
        method: "POST",
        body: JSON.stringify({ version: 2, number }),
        idempotencyKey: requestID(),
      });
      if (!result || result.version !== 2 || !result.call) throw new Error("外置网关未返回拨号状态");
      externalVoiceStatus = { ...externalVoiceStatus, call: result.call };
      const client = ensureExternalVoiceClient();
      if (!client) throw new Error("当前浏览器不支持外置语音");
      await client.prepare(result.call);
      if (!document.hidden && epoch === viewEpoch) {
        await adoptExternalVoiceSnapshot(epoch);
        toast(`正在拨打 ${number}`);
      }
    } catch (error) {
      if (!document.hidden && epoch === viewEpoch) toast(externalVoiceErrorMessage(error));
    }
    return;
  }
  const callGeneration = snapshot?.calls?.call_generation;
  if (!Number.isSafeInteger(callGeneration) || callGeneration <= 0 || snapshot?.calls?.active) {
    return toast("通话状态尚未就绪");
  }
  const client = ensureRemoteMediaClient();
  if (!client || client.snapshot().phase !== "idle") return toast("音频会话正在使用");
  try {
    await client.prepareOutgoing(callGeneration, number, (offer) => api("../api/remote/v1/media/offers", {
      method: "POST",
      body: JSON.stringify(offer),
    }));
    const media = client.snapshot();
    await mutate("calls/dial", {
      number,
      expected_call_generation: callGeneration,
      media_session_id: media.media_session_id,
      lease_generation: media.lease_generation,
    }, `正在拨打 ${number}`);
  } catch (error) {
    if (error?.code !== "unknown_outcome") client.close("outgoing-dial-failed");
    throw error;
  }
}

async function rejectExternalVoice() {
  const epoch = viewEpoch;
  const token = beginExternalVoiceOperation();
  if (!token) return toast("另一个外置语音操作正在进行");
  try {
    const call = externalVoiceStatus?.call;
    if (!call || call.phase !== "incoming_ringing" || !externalVoiceStatus?.reject_enabled ||
        !canExternalVoice("calls.control")) throw new Error("当前没有可拒接的外置来电");
    await api("/api/remote/v2/voice/calls/reject", {
      method: "POST",
      body: JSON.stringify({ version: 2, call: call.call, expected_revision: call.revision }),
      idempotencyKey: requestID(),
    });
    if (!document.hidden && epoch === viewEpoch) {
      toast("来电已拒接");
      await adoptExternalVoiceSnapshot(epoch);
    }
  } catch (error) {
    if (!document.hidden && epoch === viewEpoch) toast(externalVoiceErrorMessage(error));
  } finally {
    finishExternalVoiceOperation(token);
    if (!document.hidden && epoch === viewEpoch) renderCall();
  }
}

async function sendExternalVoiceDTMF(digits) {
  const epoch = viewEpoch;
  const token = beginExternalVoiceOperation();
  if (!token) return;
  try {
    const call = externalVoiceStatus?.call;
    const local = externalVoiceClient?.snapshot();
    if (!call || !local || !["active_unverified", "active_transport_verified"].includes(call.phase) ||
        !externalVoiceStatus?.dtmf_enabled || !local.media_lease_id || !canExternalVoice("calls.control") ||
        !canExternalVoice("calls.media")) throw new Error("当前不能发送通话按键");
    await api("/api/remote/v2/voice/calls/dtmf", {
      method: "POST",
      body: JSON.stringify({ version: 2, call: call.call, expected_revision: call.revision,
        media_lease_id: local.media_lease_id, digits }),
      idempotencyKey: requestID(),
    });
    if (!document.hidden && epoch === viewEpoch) toast(`已发送 ${digits}`);
  } catch (error) {
    if (!document.hidden && epoch === viewEpoch) toast(externalVoiceErrorMessage(error));
  } finally {
    finishExternalVoiceOperation(token);
    if (!document.hidden && epoch === viewEpoch) renderCall();
  }
}

async function sendDirectVoiceDTMF(digits) {
  const call = currentCallExpectation();
  if (!call || snapshot?.calls?.active?.state !== "active" || !can("calls.control")) {
    return toast("当前没有可发送按键的通话");
  }
  try {
    await api("../api/remote/v1/calls/dtmf", {
      method: "POST",
      body: JSON.stringify({ call, digits }),
      idempotencyKey: requestID(),
    });
  } catch (error) {
    toast(error?.message || "通话按键发送失败");
  }
}

function resetDirectDTMF() {
  if (directDTMFTimer) clearTimeout(directDTMFTimer);
  directDTMFTimer = null;
  directDTMFBuffer = "";
  directDTMFCallID = "";
  const display = $("#dial-number");
  if (display) display.value = "";
}

function scheduleDirectDTMFFlush(delay = 280) {
  if (directDTMFTimer) clearTimeout(directDTMFTimer);
  directDTMFTimer = setTimeout(() => void flushDirectVoiceDTMF(), delay);
}

async function flushDirectVoiceDTMF() {
  if (directDTMFSending || !directDTMFBuffer) return;
  if (directDTMFTimer) clearTimeout(directDTMFTimer);
  directDTMFTimer = null;
  const digits = directDTMFBuffer.slice(0, 20);
  directDTMFBuffer = directDTMFBuffer.slice(20);
  directDTMFSending = true;
  try {
    await sendDirectVoiceDTMF(digits);
  } finally {
    directDTMFSending = false;
    if (directDTMFBuffer) scheduleDirectDTMFFlush(60);
  }
}

function queueDirectVoiceDTMF(digit) {
  const callID = snapshot?.calls?.active?.id || "";
  if (!callID) return;
  if (directDTMFCallID !== callID) {
    resetDirectDTMF();
    directDTMFCallID = callID;
  }
  navigator.vibrate?.(20);
  const display = $("#dial-number");
  display.value = `${display.value}${digit}`.slice(-32);
  directDTMFBuffer += digit;
  if (directDTMFBuffer.length >= 20) void flushDirectVoiceDTMF();
  else scheduleDirectDTMFFlush();
}

function currentCallExpectation() {
  const active = snapshot?.calls?.active;
  if (!active) return null;
  return {
    call_id: active.id,
    call_generation: snapshot.calls.call_generation,
    call_index: active.index,
    call_direction: active.direction,
  };
}

function currentRemoteAnswerRequest() {
  const call = currentCallExpectation();
  const media = remoteMediaClient?.snapshot();
  if (!call || media?.phase !== "prepared" || !media.media_session_id || !media.lease_generation) return null;
  return {
    call,
    media_session_id: media.media_session_id,
    lease_generation: media.lease_generation,
  };
}

function currentRemoteHangupRequest() {
  const call = currentCallExpectation();
  return call ? { call } : null;
}

async function prepareIncomingMedia(options = {}) {
  if (incomingMediaPreparation) return incomingMediaPreparation;
  const quiet = options.quiet === true;
  const operation = (async () => {
    const call = currentCallExpectation();
    const active = snapshot?.calls?.active;
    if (!call || !active || !["incoming", "waiting"].includes(active.state)) {
      throw new Error("当前没有可绑定的来电");
    }
    const client = ensureRemoteMediaClient();
    if (!client) throw new Error("当前浏览器不支持远程音频");
    preparedMediaCallID = active.id;
    try {
      await client.prepare(call, (offer) => api("../api/remote/v1/media/offers", {
        method: "POST",
        body: JSON.stringify(offer),
      }));
      const prepared = remoteMediaClient.snapshot();
      rescueMediaReceipt = {
        call_id: active.id,
        media_session_id: prepared.media_session_id,
        lease_generation: prepared.lease_generation,
        expires_at: 0,
      };
      if (!quiet) toast("浏览器音频已连接，等待 Mac 端 UAC 校验");
      await refresh();
    } catch (error) {
      preparedMediaCallID = "";
      if (!quiet) toast(error.message || "音频连接失败");
      throw error;
    }
  })();
  incomingMediaPreparation = operation;
  try {
    return await operation;
  } finally {
    if (incomingMediaPreparation === operation) incomingMediaPreparation = null;
  }
}

async function maybePrepareIncomingMedia() {
  const active = snapshot?.calls?.active;
  const local = remoteMediaClient?.snapshot() || { phase: "idle" };
  if (document.hidden || externalVoiceSelected() || !active ||
      !["incoming", "waiting"].includes(active.state) || local.phase !== "idle" ||
      active.id === autoPrepareAttemptedCallID || !session?.incoming_answer || !session?.media_offer ||
      !can("calls.control") || !can("calls.media")) return;
  const client = ensureRemoteMediaClient();
  if (!client) return;
  // Do not surprise the user with an iOS permission sheet merely because a
  // background refresh noticed a call. When the site is already allowed we
  // still prepare immediately; otherwise the explicit Answer tap owns the
  // first permission request. Older WebKit versions without Permissions API
  // retain the existing behavior.
  if (typeof client.microphonePermissionState === "function") {
    const permission = await client.microphonePermissionState();
    if (permission === "prompt" || permission === "denied") return;
  }
  const current = snapshot?.calls?.active;
  if (document.hidden || !current || current.id !== active.id ||
      !["incoming", "waiting"].includes(current.state)) return;
  autoPrepareAttemptedCallID = active.id;
  void prepareIncomingMedia({ quiet: true }).catch(() => {});
}

function ensureRemoteMediaClient() {
  if (!remoteMediaClient && globalThis.MacCellularRemoteMediaClient) {
    remoteMediaClient = new globalThis.MacCellularRemoteMediaClient({
      audioElement: $("#remote-audio"),
      transport: () => session?.transport || "",
      onState: () => renderCall(),
    });
  }
  return remoteMediaClient;
}

function externalVoiceErrorMessage(error) {
  return ({
    unknown_outcome: "操作结果未知；不会自动重试，请点“核对结果”",
    conflict: "外置通话状态已变化，控制保持锁定",
    stale_call: "来电状态已变化，请刷新后重试",
    media_not_ready: "外置语音媒体尚未准备",
    voice_unavailable: "外置语音服务暂不可用",
    forbidden: "缺少外置语音的显式权限",
    timeout: "外置语音连接超时",
    media_failed: "端到端音频连接失败",
    canceled: "外置语音操作已取消",
  })[error?.code] || "外置语音操作失败";
}

async function adoptExternalVoiceSnapshot(epoch = viewEpoch) {
  const client = externalVoiceClient;
  if (!client) return false;
  try {
    const response = await requestExternalVoiceSnapshot(client);
    if (document.hidden || epoch !== viewEpoch) return false;
    externalVoiceStatus = response.voice;
    externalVoiceStatusError = "";
    return true;
  } catch (error) {
    if (!document.hidden && epoch === viewEpoch) {
      if (error?.code === "invalid_contract") client.close("external-voice-contract-failed");
      externalVoiceStatusError = error?.code || "snapshot_failed";
      externalVoiceStatus = {
        ...externalVoiceStatus,
        enabled: false,
        health: "unavailable",
        answer_enabled: false,
        end_enabled: false,
        recovery_required: false,
      };
    }
    return false;
  } finally {
    if (!document.hidden && epoch === viewEpoch) {
      renderGateway();
      renderCall();
    }
  }
}

async function prepareExternalIncomingMedia() {
  const epoch = viewEpoch;
  const token = beginExternalVoiceOperation();
  if (!token) throw new Error("另一个外置语音操作正在进行");
  try {
    await waitForExternalVoiceSnapshot();
    const voice = externalVoiceStatus;
    const call = voice?.call;
    if (document.hidden || epoch !== viewEpoch || !externalVoiceMode() || externalVoiceRecoveryLocked() ||
        voice?.health !== "connected" || voice?.answer_enabled !== true ||
        call?.phase !== "incoming_ringing" || !canExternalVoice("calls.media") ||
        !canExternalVoice("calls.control")) {
      throw new Error("当前没有可安全连接的外置来电");
    }
    const client = ensureExternalVoiceClient();
    if (!client) throw new Error("当前浏览器不支持外置语音");
    await client.prepare(call);
    if (document.hidden || epoch !== viewEpoch) return;
    await adoptExternalVoiceSnapshot(epoch);
    if (!document.hidden && epoch === viewEpoch) toast("麦克风与外置网关音频已连接，请手动接听");
  } finally {
    finishExternalVoiceOperation(token);
  }
}

async function submitExternalVoiceCommand(kind) {
  const epoch = viewEpoch;
  const requiredAction = kind === "answer" ? "calls.control" : "calls.hangup";
  const token = beginExternalVoiceOperation();
  if (!token) return toast("另一个外置语音操作正在进行");
  let client = null;
  try {
    await waitForExternalVoiceSnapshot();
    client = externalVoiceClient;
    if (!client || document.hidden || epoch !== viewEpoch || !externalVoiceMode() ||
        externalVoiceRecoveryLocked() || !canExternalVoice("calls.media") ||
        !canExternalVoice(requiredAction)) {
      throw new Error("外置语音控制已锁定");
    }
    const operation = client.createPersistentOperation(kind, requestID());
    await client.submitPersistent(operation);
    if (document.hidden || epoch !== viewEpoch) return;
    externalVoiceUnknownKind = "";
    toast(kind === "answer" ? "接听命令已确认" : "挂断命令已确认");
    await adoptExternalVoiceSnapshot(epoch);
  } catch (error) {
    if (document.hidden || epoch !== viewEpoch) return;
    if (client?.snapshot().phase === "outcome_unknown") externalVoiceUnknownKind = kind;
    renderCall();
    toast(externalVoiceErrorMessage(error));
  } finally {
    finishExternalVoiceOperation(token);
    if (!document.hidden && epoch === viewEpoch) renderCall();
  }
}

async function reconcileExternalVoice() {
  const epoch = viewEpoch;
  const token = beginExternalVoiceOperation();
  if (!token) return toast("另一个外置语音操作正在进行");
  try {
    await waitForExternalVoiceSnapshot();
    const client = externalVoiceClient;
    const allowed = externalVoiceUnknownKind === "answer"
      ? canExternalVoice("calls.control")
      : externalVoiceUnknownKind === "end" && canExternalVoice("calls.hangup");
    if (!client || document.hidden || epoch !== viewEpoch || !allowed ||
        !canExternalVoice("calls.media") || externalVoiceRecoveryLocked()) {
      return toast("当前结果不能由本页安全核对");
    }
    const result = await client.reconcile();
    if (document.hidden || epoch !== viewEpoch) return;
    externalVoiceStatus = { ...externalVoiceStatus, call: result.call };
    if (client.snapshot().phase === "outcome_unknown") {
      toast("结果仍未知；控制继续锁定，不会自动重试");
    } else {
      externalVoiceUnknownKind = "";
      toast("通话状态已核对");
      await adoptExternalVoiceSnapshot(epoch);
    }
  } catch (error) {
    if (!document.hidden && epoch === viewEpoch) toast(externalVoiceErrorMessage(error));
  } finally {
    finishExternalVoiceOperation(token);
    if (!document.hidden && epoch === viewEpoch) renderCall();
  }
}

document.querySelectorAll("[data-tab]").forEach((button) => button.addEventListener("click", () => {
  if (smsOnlySession() && button.dataset.tab === "calls") return;
  selectTab(button.dataset.tab);
  if (button.dataset.tab === "recordings") void loadRecordings({ includeMac: true });
}));

$("#dial-pad").addEventListener("click", (event) => {
  const button = event.target.closest("button");
  if (!button || !$("#dial-pad").contains(button)) return;
  const digit = button.querySelector("strong")?.textContent || "";
  if (!digit) return;
  if (externalVoiceSelected() && externalVoiceStatus?.call &&
      ["active_unverified", "active_transport_verified"].includes(externalVoiceStatus.call.phase)) {
    void sendExternalVoiceDTMF(digit);
    return;
  }
  if (!externalVoiceSelected() && snapshot?.calls?.active?.state === "active") {
		queueDirectVoiceDTMF(digit);
    return;
  }
  $("#dial-number").value += digit;
});
$("#dial-delete").addEventListener("click", () => {
  const input = $("#dial-number");
  input.value = input.value.slice(0, -1);
});
$("#dial-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (smsOnlySession()) return;
  const button = event.submitter;
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "正在准备…";
  try {
    await dial($("#dial-number").value.trim());
    $("#dial-number").value = "";
  } catch (error) {
    const message = globalThis.MacCellularPublicVoiceUI?.directVoiceErrorMessage?.(error) ||
      "拨号准备失败，请稍后重试";
    toast(message);
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
});
$("#answer").addEventListener("click", async () => {
  if (smsOnlySession()) return;
  if (externalVoiceSelected()) return submitExternalVoiceCommand("answer");
  if (directAnswerInFlight) return;
  directAnswerInFlight = true;
  directAnswerStartedAt = Date.now();
  if (directAnswerProgressTimer) clearInterval(directAnswerProgressTimer);
  directAnswerProgressTimer = setInterval(() => renderCall(), 1_000);
  renderCall();
  try {
    if (!currentRemoteAnswerRequest()) {
      await prepareIncomingMedia({ quiet: true });
    }
    const request = currentRemoteAnswerRequest();
    if (!request) throw new Error("音频准备尚未完成，请重试");
    await mutate("calls/answer", request, "接听成功，通话音频已连接", {
		timeoutMilliseconds: 25_000,
      suppressNetworkToast: true,
    });
		directAnsweredCallID = request.call.call_id;
  } catch (error) {
    if (!error?.httpStatus) {
      await refresh();
      if (snapshot?.calls?.active?.state === "active") {
			directAnsweredCallID = snapshot.calls.active.id;
        toast("接听已由模块确认，通话音频已连接");
      } else {
        toast("接听请求没有得到确认，请再点一次；不会重复接听");
      }
    }
  } finally {
    directAnswerInFlight = false;
		directAnswerStartedAt = 0;
		if (directAnswerProgressTimer) clearInterval(directAnswerProgressTimer);
		directAnswerProgressTimer = null;
    renderCall();
  }
});
$("#reject").addEventListener("click", () => {
  if (smsOnlySession()) return;
  if (externalVoiceSelected()) return rejectExternalVoice();
  return mutate("calls/reject", currentCallExpectation(), "已拒接");
});
$("#hangup").addEventListener("click", async () => {
  if (smsOnlySession()) return;
  if (externalVoiceSelected()) return submitExternalVoiceCommand("end");
  const request = currentRemoteHangupRequest();
  if (!request) return toast("当前通话状态正在更新，请稍后再试");
  const button = $("#hangup");
	directHangupInFlight = true;
  button.disabled = true;
  button.textContent = "正在挂断…";
  try {
    await mutate("calls/hangup", request, "通话已挂断");
    await stopCallRecording();
    rescueMediaReceipt = null;
    preparedMediaCallID = "";
    remoteMediaClient?.close("hangup-accepted");
  } catch {
    // mutate() already presents the user-facing failure state.
		directHangupInFlight = false;
  } finally {
		button.textContent = directHangupInFlight ? "正在挂断…" : "挂断";
    renderCall();
  }
});
$("#call-mute").addEventListener("click", () => {
  if (snapshot?.calls?.active?.state !== "active" || externalVoiceSelected()) return;
  callMuted = !callMuted;
  remoteMediaClient?.setMuted(callMuted);
  navigator.vibrate?.(25);
  renderCall();
});
$("#call-keypad").addEventListener("click", () => {
  if (snapshot?.calls?.active?.state !== "active") return;
  callKeypadVisible = !callKeypadVisible;
  renderCall();
  if (callKeypadVisible) $("#dial-form").scrollIntoView({ behavior: "smooth", block: "center" });
});
$("#call-record").addEventListener("click", async () => {
  const active = snapshot?.calls?.active;
  const recorder = ensureRecordingServices();
  if (!active || active.state !== "active" || externalVoiceSelected() || !recorder?.supported?.()) return;
  const button = $("#call-record");
  button.disabled = true;
  try {
    if (recorder.active()) {
      recordingSuppressedCallID = active.id;
      await stopCallRecording();
      toast("本次自动录音已停止");
    } else {
      recordingSuppressedCallID = "";
      await maybeStartAutomaticRecording(active);
    }
  } catch (error) {
    toast(error?.message || "录音操作失败");
  } finally {
    renderCall();
  }
});
$("#prepare-media").addEventListener("click", async () => {
  if (smsOnlySession()) return;
  const button = $("#prepare-media");
  button.disabled = true;
  try {
    if (externalVoiceSelected()) await prepareExternalIncomingMedia();
    else await prepareIncomingMedia();
  } catch (error) {
    if (externalVoiceSelected()) toast(error?.message || "外置语音连接失败");
  } finally {
    renderCall();
  }
});
$("#reconcile-voice").addEventListener("click", () => {
  if (!smsOnlySession()) reconcileExternalVoice();
});

$("#new-message").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  try {
    await mutate("sms/send", { phone: $("#sms-phone").value.trim(), message: $("#sms-text").value.trim() }, "短信已发送");
    $("#sms-text").value = "";
  } finally { button.disabled = false; }
});
$("#show-compose").addEventListener("click", () => {
  $("#new-message").hidden = false;
  $("#show-compose").hidden = true;
  $("#sms-phone").focus();
});
$("#close-compose").addEventListener("click", () => {
  $("#new-message").hidden = true;
  $("#show-compose").hidden = false;
  $("#sms-phone").value = "";
  $("#sms-text").value = "";
});
$("#thread-send").addEventListener("click", async () => {
  const text = $("#thread-text").value.trim();
  if (!text || !selectedPeer) return;
  const button = $("#thread-send");
  button.disabled = true;
  try {
    await mutate("sms/send", { phone: selectedPeer, message: text }, "回复已发送");
    $("#thread-text").value = "";
    const group = groupMessages(currentSMSItems()).find((item) => item.peer === selectedPeer);
    if (group) openThread(group.peer, group.messages);
  } finally { button.disabled = false; }
});
$("#sms-refresh").addEventListener("click", async () => {
  const button = $("#sms-refresh");
  button.disabled = true;
  try {
    await mutate("sms/refresh", {}, "已从模块重新读取短信");
  } finally {
    button.disabled = !can("sms.read");
  }
});

$("#notify").addEventListener("click", async () => {
  if (!("Notification" in window)) return toast("当前浏览器不支持通知");
  const permission = await Notification.requestPermission();
  $("#notify").textContent = permission === "granted" ? "通知已开启" : "通知未授权";
});
$("#incoming-call-push-toggle").addEventListener("click", () => incomingCallPush.toggle());
$("#refresh").addEventListener("click", () => refresh({ force: true }));
$("#quick-dial").addEventListener("click", () => {
  if (smsOnlySession()) return toast("当前线路只启用了短信");
  selectTab("calls");
});
$("#quick-message").addEventListener("click", () => {
  selectTab("messages");
  $("#new-message").hidden = false;
  $("#show-compose").hidden = true;
  $("#sms-phone").focus();
});
$("#home-calls").addEventListener("click", () => {
  if (smsOnlySession()) return toast("当前线路只启用了短信");
  selectTab("calls");
});
$("#home-messages").addEventListener("click", () => selectTab("messages"));
$("#home-recordings").addEventListener("click", () => {
  if (smsOnlySession()) return toast("当前线路没有通话录音");
  selectTab("recordings");
  void loadRecordings({ includeMac: true });
});
$("#edit-line").addEventListener("click", () => {
  $("#line-label-input").value = savedLineLabel();
  $("#line-editor").hidden = false;
  $("#line-label-input").focus();
});
$("#cancel-line-edit").addEventListener("click", () => { $("#line-editor").hidden = true; });
$("#line-editor").addEventListener("submit", (event) => {
  event.preventDefault();
  const label = $("#line-label-input").value.trim().slice(0, 24);
  if (!label) return toast("请输入线路名称");
  try { localStorage.setItem(lineLabelStorageKey, label); } catch { return toast("当前浏览器无法保存线路名称"); }
  $("#line-label").textContent = label;
  $("#line-editor").hidden = true;
  toast("线路名称已更新");
});
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    refreshAbortController?.abort();
    refreshAbortController = null;
    polling = false;
    callPolling = false;
    return;
  }
  void remoteMediaClient?.ensurePlayback?.();
  refresh();
});
window.addEventListener("pagehide", () => {
  refreshAbortController?.abort();
  refreshAbortController = null;
  polling = false;
  callPolling = false;
  clearSensitiveView();
  externalVoiceClient?.dispose();
  externalVoiceClient = null;
});
window.addEventListener("pageshow", () => {
  void remoteMediaClient?.ensurePlayback?.();
  if (!document.hidden && !polling && !session) refresh({ force: true });
});

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("service-worker.js", { scope: "./" }).catch(() => {});
}
refresh({ force: true });
void loadRecordings();
setInterval(() => { if (!document.hidden) refresh(); }, 5000);
setInterval(() => { if (!document.hidden) refreshCallSnapshot(); }, 1000);
