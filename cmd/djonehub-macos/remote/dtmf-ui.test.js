import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(new URL("./app.js", import.meta.url), "utf8");

test("direct DTMF responds locally and batches rapid keys", () => {
  assert.match(source, /display\.value = `\$\{display\.value\}\$\{digit\}`/);
  assert.match(source, /directDTMFBuffer \+= digit/);
  assert.match(source, /scheduleDirectDTMFFlush\(delay = 280\)/);
  assert.match(source, /directDTMFBuffer\.slice\(0, 20\)/);
  assert.match(source, /JSON\.stringify\(\{ call, digits \}\)/);
  assert.doesNotMatch(source, /directDTMFQueue/);
});

test("direct DTMF pending input is cleared when the call changes", () => {
  assert.match(source, /function resetDirectDTMF\(\)/);
  assert.match(source, /if \(directDTMFCallID !== callID\) \{\s*resetDirectDTMF\(\)/);
  assert.match(source, /if \(directDTMFCallID \|\| directDTMFBuffer\) resetDirectDTMF\(\)/);
});
