import test from "node:test";
import assert from "node:assert/strict";
import { primaryModifier, terminalModifier, shortcut } from "./platform.mjs";

test("Windows shortcuts reserve Ctrl+C for terminal interruption and ignore AltGr", () => {
  const ctrl = { ctrlKey: true, altKey: false, metaKey: false, shiftKey: false };
  assert.equal(primaryModifier(ctrl, true), true);
  assert.equal(terminalModifier(ctrl, true), false);
  assert.equal(terminalModifier({ ...ctrl, shiftKey: true }, true), true);
  assert.equal(primaryModifier({ ...ctrl, altKey: true }, true), false);
  assert.equal(primaryModifier({ ...ctrl, metaKey: true }, false), true);
  assert.equal(shortcut("⌘⇧↵", true), "Ctrl+Shift+Enter");
  assert.equal(shortcut("⌘W", false), "⌘W");
});
