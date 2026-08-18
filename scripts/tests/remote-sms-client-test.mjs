import assert from "node:assert/strict";

const script = new URL("../../cmd/djonehub-macos/remote/sms-state.js", import.meta.url);
await import(script.href);

function message(id, eventID, direction, peer, timestamp, status = direction === "incoming" ? "received" : "pending") {
  return {
    id,
    event_id: eventID,
    direction,
    status,
    peer,
    content: `${id}-body`,
    timestamp,
    updated_at: timestamp,
  };
}

assert.equal(globalThis.MacCellularRemoteSMSMessagePeer({ peer: "synthetic-peer-a", sender: "legacy" }), "synthetic-peer-a");
assert.equal(globalThis.MacCellularRemoteSMSMessagePeer({ sender: "legacy" }), "legacy");
assert.equal(globalThis.MacCellularRemoteSMSMessagePeer({}), "未知号码");

const state = new globalThis.MacCellularRemoteSMSState();
const older = message("msg_old", "evt_old", "incoming", "peer-a", "2026-08-14T00:00:00Z");
const newer = message("msg_new", "evt_new", "incoming", "peer-b", "2026-08-14T01:00:00Z");

assert.deepEqual(state.applyPage({ messages: [newer], cursor: "snapshot-1", bootstrap: true, has_more: true }, { reset: true, notify: false }), []);
assert.equal(state.ready, false);
assert.deepEqual(state.applyPage({ messages: [older], cursor: "delta-2", bootstrap: true, has_more: false }, { notify: false }), []);
assert.equal(state.ready, true);
assert.deepEqual(state.items().map((item) => item.id), ["msg_new", "msg_old"]);

const incoming = message("msg_incoming", "evt_incoming", "incoming", "peer-a", "2026-08-14T02:00:00Z");
const outgoing = message("msg_outgoing", "evt_pending", "outgoing", "peer-c", "2026-08-14T03:00:00Z");
assert.deepEqual(
  state.applyPage({ messages: [incoming, outgoing], cursor: "delta-4", bootstrap: false, has_more: false }, { notify: true }).map((item) => item.id),
  ["msg_incoming"],
);

const submitted = { ...outgoing, event_id: "evt_submitted", status: "submitted", segments: 1, updated_at: "2026-08-14T03:01:00Z" };
assert.deepEqual(state.applyPage({ messages: [submitted], cursor: "delta-5", bootstrap: false, has_more: false }, { notify: true }), []);
assert.equal(state.items().filter((item) => item.id === outgoing.id).length, 1);
assert.equal(state.items().find((item) => item.id === outgoing.id).status, "submitted");

assert.throws(
  () => state.applyPage({ messages: [{ content: "missing stable IDs" }], cursor: "bad", has_more: false }),
  /invalid SMS sync response/,
);
assert.throws(
  () => state.applyPage({ messages: [{ content: "missing stable IDs" }], cursor: "bad", bootstrap: false, has_more: false }),
  /invalid SMS sync message/,
);
state.clear();
assert.equal(state.cursor, "");
assert.equal(state.ready, false);
assert.deepEqual(state.items(), []);

console.log("remote SMS client tests passed");
