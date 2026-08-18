(function installRemoteSessionPresentation(root) {
  "use strict";

  function smsOnly(session) {
    return session?.sms_only === true;
  }

  function transportLabel(session) {
    const transport = typeof session?.transport === "string" ? session.transport.trim() : "";
    if (transport.toLowerCase() === "cloudflare-access") return "公网";
    return transport || "Tailscale";
  }

  const presentation = Object.freeze({ smsOnly, transportLabel });
  root.MacCellularRemoteSessionPresentation = presentation;
  if (typeof module !== "undefined" && module.exports) module.exports = presentation;
})(globalThis);
