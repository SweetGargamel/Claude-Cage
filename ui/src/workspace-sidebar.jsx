import { shortcut } from "./platform.mjs";
import React, { useEffect, useState } from "react";
import {
  ArrowUpRight,
  ChevronDown,
  FileCode2,
  FolderOpen,
  Globe2,
  Home,
  PanelLeftClose,
  Plus,
  Settings2,
} from "lucide-react";
import { EnvironmentShield } from "./environment-shield";
import logo from "./logo.png";
import { projectName } from "./workspace-state.mjs";
import { WorkspaceTabs } from "./workspace-tabs";

export function WorkspaceSidebar({
  workspace,
  defaultWorkspace,
  projects,
  tabs,
  saved,
  active,
  home,
  busy,
  ready,
  running,
  runtimeLabel,
  network,
  onHome,
  onOpenProject,
  onProjectSelect,
  onSelect,
  onClose,
  onRename,
  onPin,
  onReorder,
  onNewTerminal,
  onBrowser,
  onEditor,
  onSettings,
  onSafety,
  onCollapse,
  updateAction,
}) {
  const [collapsed, setCollapsed] = useState({});
  const activeWorkspace = tabs.find((tab) => tab.id === active)?.workspace;
  useEffect(() => {
    if (activeWorkspace)
      setCollapsed((previous) => ({ ...previous, [activeWorkspace]: false }));
  }, [active, activeWorkspace]);
  const healthy = ready && network.mode === "open";
  return (
    <aside className="workspace-sidebar" aria-label="项目与终端">
      <div className="sidebar-top">
        <span className="sidebar-brand">
          <img className="brand-logo" src={logo} alt="" />
          Cage
        </span>
        <button
          className="icon-button"
          onClick={onCollapse}
          aria-label="收起侧栏"
          title="收起侧栏"
        >
          <PanelLeftClose size={17} />
        </button>
      </div>
      <nav className="sidebar-navigation" aria-label="主导航">
        <button
          className={`sidebar-link ${home ? "selected" : ""}`}
          onClick={onHome}
        >
          <Home size={17} />
          工作台
        </button>
        <button
          className={`sidebar-link ${!home && tabs.some((tab) => tab.id === active && tab.kind !== "terminal") ? "selected" : ""}`}
          onClick={onBrowser}
          disabled={!ready || busy}
          aria-label="打开浏览器"
          title="打开浏览器"
        >
          <Globe2 size={17} />
          浏览器
        </button>
        <button
          className="sidebar-link"
          onClick={() => onOpenProject()}
          disabled={busy || !running}
        >
          <FolderOpen size={17} />
          打开项目<kbd>{shortcut("⌘O")}</kbd>
        </button>
        <button
          className="sidebar-link"
          onClick={() => onEditor()}
          disabled={!ready || busy}
        >
          <FileCode2 size={17} />
          项目文件
          <ArrowUpRight size={14} className="navigation-trailing" />
        </button>
      </nav>
      <div className="sidebar-scroll">
        <div className="sidebar-section-heading">
          <span>项目</span>
          <button
            className="icon-button"
            onClick={() => onOpenProject()}
            aria-label="添加项目"
            title="添加项目"
            disabled={busy || !running}
          >
            <Plus size={15} />
          </button>
        </div>
        {projects.map((path) => {
          const project =
            path === defaultWorkspace && path.endsWith("-workspace")
              ? "默认工作区"
              : projectName(path);
          const terminals = tabs.filter(
            (tab) => tab.kind === "terminal" && tab.workspace === path,
          );
          const expanded = !collapsed[path];
          return (
            <section
              className="sidebar-project"
              key={path}
              aria-label={`项目 ${project}`}
            >
              <div
                className={`project-heading ${workspace === path ? "current-project" : ""}`}
              >
                <button
                  className="project-disclosure"
                  title={path}
                  aria-expanded={expanded}
                  onClick={() => {
                    setCollapsed((previous) => ({
                      ...previous,
                      [path]: expanded,
                    }));
                    onProjectSelect(path);
                  }}
                >
                  <ChevronDown
                    size={13}
                    className={expanded ? "" : "collapsed"}
                  />
                  <FolderOpen size={16} />
                  <span>{project}</span>
                </button>
                <button
                  className="icon-button"
                  aria-label={`在 ${project} 新建终端`}
                  title="新建终端"
                  disabled={!running || busy}
                  onClick={() => {
                    setCollapsed((previous) => ({
                      ...previous,
                      [path]: false,
                    }));
                    onNewTerminal(path);
                  }}
                >
                  <Plus size={15} />
                </button>
              </div>
              <div
                className={`project-children ${expanded ? "expanded" : ""}`}
                inert={!expanded}
              >
                <div>
                  {!!terminals.length && (
                    <WorkspaceTabs
                      tabs={terminals}
                      saved={saved}
                      active={home ? "" : active}
                      label={`${project} 的终端`}
                      busy={busy}
                      ready={ready}
                      onSelect={onSelect}
                      onClose={onClose}
                      onRename={onRename}
                      onPin={onPin}
                      onReorder={onReorder}
                    />
                  )}
                  {!terminals.length && (
                    <p className="sidebar-empty">
                      {running ? "点击 + 新建终端" : "启动后可新建终端"}
                    </p>
                  )}
                </div>
              </div>
            </section>
          );
        })}
        {!projects.length && (
          <p className="sidebar-empty">打开本地文件夹开始工作</p>
        )}
      </div>
      <div className="sidebar-bottom">
        <button className="sidebar-status" onClick={onSafety}>
          <i className={`status-dot ${network.mode}`} />
          <span>{healthy ? "环境运行正常" : runtimeLabel}</span>
          <EnvironmentShield size={17} healthy={healthy} />
        </button>
        <div className="sidebar-footer-actions">
          <button className="sidebar-link" onClick={onSettings}>
            <Settings2 size={17} />
            设置<kbd>{shortcut("⌘,")}</kbd>
          </button>
          {updateAction}
        </div>
      </div>
    </aside>
  );
}
