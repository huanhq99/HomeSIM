(function installRemoteSMSState(root) {
  "use strict";

  function messagePeer(message) {
    return String(message?.peer || message?.sender || "未知号码");
  }

  function messageTime(message) {
    const value = Date.parse(message?.timestamp || "");
    return Number.isFinite(value) ? value : 0;
  }

  function compareMessages(left, right) {
    const byTimestamp = messageTime(right) - messageTime(left);
    if (byTimestamp) return byTimestamp;
    const byUpdate = Date.parse(right?.updated_at || "") - Date.parse(left?.updated_at || "");
    if (Number.isFinite(byUpdate) && byUpdate) return byUpdate;
    return String(right?.id || "").localeCompare(String(left?.id || ""));
  }

  function validSyncMessage(message) {
    return Boolean(
      message && typeof message.id === "string" && message.id.length > 0 &&
      typeof message.event_id === "string" && message.event_id.length > 0 &&
      (message.direction === "incoming" || message.direction === "outgoing") &&
      typeof message.peer === "string" && message.peer.length > 0 &&
      typeof message.content === "string" && typeof message.timestamp === "string"
    );
  }

  class MacCellularRemoteSMSState {
    constructor() { this.clear(); }

    clear() {
      this.messages = new Map();
      this.cursor = "";
      this.ready = false;
    }

    applyPage(page, { reset = false, notify = false } = {}) {
      if (!page || !Array.isArray(page.messages) || typeof page.cursor !== "string" || !page.cursor ||
          typeof page.bootstrap !== "boolean" || typeof page.has_more !== "boolean") {
        throw new Error("invalid SMS sync response");
      }
      if (reset) this.clear();
      const incoming = [];
      for (const message of page.messages) {
        if (!validSyncMessage(message)) throw new Error("invalid SMS sync message");
        const existed = this.messages.has(message.id);
        this.messages.set(message.id, message);
        if (notify && !existed && message.direction === "incoming") incoming.push(message);
      }
      this.cursor = page.cursor;
      if (!page.has_more) this.ready = true;
      return incoming;
    }

    items() {
      return [...this.messages.values()].sort(compareMessages);
    }
  }

  root.MacCellularRemoteSMSState = MacCellularRemoteSMSState;
  root.MacCellularRemoteSMSMessagePeer = messagePeer;
})(globalThis);
