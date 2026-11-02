export const isWindows = /Win/i.test(globalThis.navigator?.platform || "");
export const primaryModifier = (event, windows = isWindows) =>
  windows ? event.ctrlKey && !event.altKey && !event.metaKey : event.metaKey;
export const terminalModifier = (event, windows = isWindows) =>
  windows ? primaryModifier(event, true) && event.shiftKey : event.metaKey;
export function shortcut(mac, windows = isWindows) {
  return windows
    ? mac.replaceAll("⌘", "Ctrl+").replaceAll("⇧", "Shift+").replaceAll("↵", "Enter")
    : mac;
}
