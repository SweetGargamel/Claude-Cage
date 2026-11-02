export function readLocal(key, fallback) {
  try {
    return JSON.parse(localStorage.getItem(key)) ?? fallback;
  } catch {
    return fallback;
  }
}

export function orderedTabs(tabs, saved) {
  const order = saved.order || [];
  return [...tabs].sort((a, b) => {
    const pinned =
      Number(Boolean(saved[b.id]?.pinned)) -
      Number(Boolean(saved[a.id]?.pinned));
    if (pinned) return pinned;
    const index = (id) =>
      order.includes(id) ? order.indexOf(id) : order.length;
    return index(a.id) - index(b.id);
  });
}

export const projectName = (path) =>
  path?.split(/[\\/]/).filter(Boolean).at(-1) || "新项目";

export function tabName(tab, saved, project) {
  return (
    saved[tab.id]?.title ||
    (tab.kind === "editor"
      ? `文件 · ${tab.workspace ? projectName(tab.workspace) : project}`
      : tab.title) ||
    "新标签页"
  );
}
