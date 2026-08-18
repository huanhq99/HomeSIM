"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

const { smsOnly, transportLabel } = require("./session-presentation.js");

test("Cloudflare Access SMS sessions select the public SMS presentation", () => {
  const session = { sms_only: true, transport: "cloudflare-access" };
  assert.equal(smsOnly(session), true);
  assert.equal(transportLabel(session), "公网");
});

test("sms_only is fail-closed to the exact boolean", () => {
  assert.equal(smsOnly({ sms_only: "true" }), false);
  assert.equal(smsOnly({ sms_only: 1 }), false);
  assert.equal(smsOnly({}), false);
  assert.equal(smsOnly(null), false);
});

test("legacy Tailscale transport remains unchanged", () => {
  assert.equal(transportLabel({ transport: "Tailscale" }), "Tailscale");
  assert.equal(transportLabel({ transport: "tailscale-serve" }), "tailscale-serve");
  assert.equal(transportLabel({}), "Tailscale");
});

test("only the Cloudflare Access transport gets the 公网 label", () => {
  assert.equal(transportLabel({ transport: " Cloudflare-Access " }), "公网");
  assert.equal(transportLabel({ transport: "cloudflare-tunnel" }), "cloudflare-tunnel");
});
