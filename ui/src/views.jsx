import { terminalModifier } from "./platform.mjs";
import React, {
  useEffect,
  useRef,
  useState,
  useImperativeHandle,
  forwardRef,
} from "react";
import { useAppearance } from "./appearance";
import { terminalTheme, systemCodeFont } from "./appearance-state.mjs";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import RFB from "../.generated/novnc/core/rfb.js";
import { api, readClipboard, writeClipboard } from "./api";
import { installRemoteInput } from "./remote-input";
import { useWindowVisibility } from "./window-visibility";
import { RefreshCw, Clipboard, ArrowUpRight } from "lucide-react";
import "@xterm/xterm/css/xterm.css";

export const RemoteView = forwardRef(function RemoteView(
  { visible, keyboard, tabId, onStatus, onFallback },
  ref,
) {
  const windowVisible = useWindowVisibility();
  const screenVisible = visible && windowVisible;
  const box = useRef(),
    rfb = useRef(),
    input = useRef(),
    current = useRef({ visible, keyboard, onStatus, onFallback });
  current.current = { visible: screenVisible, keyboard, tabId, onStatus, onFallback };
  const failures = useRef(0);
  const [connection, setConnection] = useState("connecting"),
    [attempt, setAttempt] = useState(0);
  useImperativeHandle(ref, () => ({
    focus: () => rfb.current?.focus(),
    edit: (action) =>
      box.current?.contains(document.activeElement)
        ? input.current?.edit(action)
        : false,
    copy: () => input.current?.copy(),
    paste: () => input.current?.readAndPaste(),
    pasteText: (text) => input.current?.paste(text),
    transfer: () =>
      onFallback(input.current?.text() || "", "可在此使用本机复制、粘贴。"),
  }));
  useEffect(() => {
    if (!screenVisible) return;
    let disposed = false,
      client,
      resizeObserver,
      resizeTimer,
      reconnectTimer;
    setConnection("connecting");
    const retry = () => {
      if (disposed) return;
      setConnection("disconnected");
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(
        () => setAttempt((v) => v + 1),
        Math.min(1000 * 2 ** failures.current++, 5000),
      );
    };
    api("/vnc-credentials")
      .then(({ password }) => {
        if (disposed) return;
        client = new RFB(
          box.current,
          `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/vnc`,
          { credentials: { password } },
        );
        rfb.current = client;
        client.scaleViewport = true;
        client.resizeSession = false;
        client.background = "#ffffff";
        client.qualityLevel = 8;
        client.compressionLevel = 2;
        client.addEventListener("connect", () => {
          if (disposed) return;
          failures.current = 0;
          setConnection("connected");
          const resize = () => {
            clearTimeout(resizeTimer);
            resizeTimer = setTimeout(() => {
              if (disposed) return;
              const { width, height } = box.current.getBoundingClientRect();
              if (width >= 320 && height >= 240)
                api("/browser/resize", {
                  width: Math.min(8192, Math.floor(width * 2)),
                  height: Math.min(8192, Math.floor(height * 2)),
                }).catch(() =>
                  current.current.onStatus("画面尺寸未能调整，请重新连接画面"),
                );
            }, 160);
          };
          resizeObserver = new ResizeObserver(resize);
          resizeObserver.observe(box.current);
          resize();
          input.current = installRemoteInput(client, box.current, {
            insertText: (text) => {
              const id = current.current.tabId;
              return client.cageRunInput(() => {
                if (disposed || !current.current.visible || current.current.tabId !== id)
                  throw new Error("Input target changed");
                return api("/browser/insert-text", { id, text });
              });
            },
            mode: () => current.current.keyboard,
            isActive: () => current.current.visible,
            onStatus: (t) => current.current.onStatus(t),
            onFallback: (...args) => current.current.onFallback(...args),
          });
          if (current.current.visible) client.focus();
        });
        client.addEventListener("disconnect", () => {
          if (disposed) return;
          input.current?.dispose();
          input.current = null;
          retry();
        });
        client.addEventListener("securityfailure", () => {
          retry();
        });
      })
      .catch(() => {
        retry();
      });
    return () => {
      disposed = true;
      clearTimeout(resizeTimer);
      clearTimeout(reconnectTimer);
      resizeObserver?.disconnect();
      input.current?.dispose();
      input.current = null;
      client?.disconnect();
      rfb.current = null;
    };
  }, [attempt, screenVisible]);
  useEffect(() => {
    if (screenVisible && connection === "connected") {
      const t = setTimeout(() => rfb.current?.focus(), 80);
      return () => clearTimeout(t);
    }
  }, [screenVisible, connection]);
  return (
    <div className={`remote-view ${visible ? "" : "concealed"}`}>
      <div className="remote-canvas" ref={box} />
      {connection !== "connected" && (
        <div className="view-overlay">
          <RefreshCw className={connection === "connecting" ? "spin" : ""} />
          <h3>
            {connection === "connecting" ? "正在连接画面" : "画面连接已断开"}
          </h3>
          <p>后台任务不会因画面断开而自动结束。</p>
          {connection === "disconnected" && (
            <button
              className="primary"
              onClick={() => setAttempt((v) => v + 1)}
            >
              重新连接画面
            </button>
          )}
        </div>
      )}
    </div>
  );
});

export function TerminalView({ session, visible, onStatus, onControls }) {
  const { appearance, theme, variant } = useAppearance();
  const box = useRef(),
    instance = useRef(),
    fit = useRef(),
    socket = useRef();
  const [connected, setConnected] = useState(false),
    [attempt, setAttempt] = useState(0);
  const visibleRef = useRef(visible);
  visibleRef.current = visible;
  useEffect(() => {
    const terminal = new Terminal({
      fontFamily: theme.fonts.code || systemCodeFont,
      fontSize: appearance.codeFontSize,
      lineHeight: 1.3,
      cursorBlink: true,
      minimumContrastRatio: 4.5,
      scrollback: 6000,
      theme: terminalTheme(theme, variant),
    });
    const fitter = new FitAddon();
    terminal.loadAddon(fitter);
    terminal.open(box.current);
    instance.current = terminal;
    fit.current = fitter;
    const ws = new WebSocket(
      `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/terminals/${session.id}/stream`,
    );
    ws.binaryType = "arraybuffer";
    socket.current = ws;
    const resize = () => {
      if (!visibleRef.current) return;
      try {
        fitter.fit();
        if (ws.readyState === WebSocket.OPEN)
          ws.send(
            JSON.stringify({
              type: "resize",
              cols: terminal.cols,
              rows: terminal.rows,
            }),
          );
      } catch {}
    };
    ws.onopen = () => {
      setConnected(true);
      resize();
      if (visibleRef.current) terminal.focus();
    };
    ws.onmessage = (event) => terminal.write(new Uint8Array(event.data));
    ws.onclose = () => setConnected(false);
    terminal.onData((data) => {
      if (ws.readyState === WebSocket.OPEN)
        ws.send(JSON.stringify({ type: "input", data }));
    });
    onControls?.({
      edit(action) {
        if (!box.current?.contains(document.activeElement)) return false;
        if (action === "copy") {
          if (terminal.hasSelection())
            void writeClipboard(terminal.getSelection()).catch(() =>
              onStatus("无法写入本机剪贴板"),
            );
          return true;
        }
        if (action === "paste") {
          void readClipboard()
            .then((text) => terminal.paste(text))
            .catch(() => onStatus("无法读取本机剪贴板"));
          return true;
        }
        if (action === "selectAll") {
          terminal.selectAll();
          return true;
        }
        return false;
      },
    });
    terminal.attachCustomKeyEventHandler((event) => {
      if (!terminalModifier(event) || event.type !== "keydown") return true;
      const key = event.key.toLowerCase();
      if (key === "c") {
        event.preventDefault();
        if (terminal.hasSelection())
          writeClipboard(terminal.getSelection())
            .then(() => onStatus("已复制终端选中文本"))
            .catch(() => onStatus("请使用右键菜单复制选中文本"));
        return false;
      }
      if (key === "v") {
        event.preventDefault();
        readClipboard()
          .then((text) => terminal.paste(text))
          .catch(() => onStatus("请使用浏览器的粘贴菜单"));
        return false;
      }
      if (key === "a") {
        event.preventDefault();
        terminal.selectAll();
        return false;
      }
      if (["+", "=", "-"].includes(key)) {
        event.preventDefault();
        terminal.options.fontSize = Math.max(
          10,
          Math.min(26, terminal.options.fontSize + (key === "-" ? -1 : 1)),
        );
        resize();
        return false;
      }
      return !terminalModifier(event);
    });
    const observer = new ResizeObserver(resize);
    observer.observe(box.current);
    resize();
    return () => {
      onControls?.(null);
      observer.disconnect();
      ws.close();
      terminal.dispose();
      instance.current = null;
    };
  }, [session.id, attempt]);
  useEffect(() => {
    if (visible) {
      fit.current?.fit();
      instance.current?.focus();
      const ws = socket.current,
        t = instance.current;
      if (ws?.readyState === WebSocket.OPEN && t)
        ws.send(JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }));
    }
  }, [visible]);
  useEffect(() => {
    if (!instance.current) return;
    instance.current.options.theme = terminalTheme(theme, variant);
    instance.current.options.fontFamily = theme.fonts.code || systemCodeFont;
    instance.current.options.fontSize = appearance.codeFontSize;
    if (visibleRef.current) fit.current?.fit();
  }, [theme, variant, appearance.codeFontSize]);
  return (
    <div className={`terminal-view ${visible ? "" : "concealed"}`}>
      <div className="terminal-host" ref={box} />
      {!connected && (
        <div className="terminal-note">
          {session.closed
            ? "终端会话已结束。可从项目旁的 + 新建终端。"
            : "正在连接终端…"}
          {!session.closed && (
            <button onClick={() => setAttempt((v) => v + 1)}>
              <RefreshCw size={13} />
              重新连接
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export function Modal({ title, children, onClose, wide = false, footer }) {
  const panel = useRef(),
    closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const previous = document.activeElement;
    const first = panel.current.querySelector(
      "input:not([disabled]),button:not([disabled]),select:not([disabled])",
    );
    first?.focus();
    const key = (e) => {
      if (
        [...document.querySelectorAll('[role="dialog"]')].at(-1) !==
        panel.current
      )
        return;
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        closeRef.current?.();
      }
      if (e.key === "Tab") {
        const nodes = [
          ...panel.current.querySelectorAll(
            "input:not([disabled]),button:not([disabled]),select:not([disabled]),textarea:not([disabled]),a[href]",
          ),
        ].filter((n) => n.offsetParent !== null);
        if (!nodes.length) return;
        const index = nodes.indexOf(document.activeElement);
        if (e.shiftKey && index <= 0) {
          e.preventDefault();
          nodes.at(-1).focus();
        } else if (!e.shiftKey && (index === nodes.length - 1 || index < 0)) {
          e.preventDefault();
          nodes[0].focus();
        }
      }
    };
    const closeEvent = () => {
      if (
        [...document.querySelectorAll('[role="dialog"]')].at(-1) ===
        panel.current
      )
        closeRef.current?.();
    };
    document.addEventListener("cage-close-modal", closeEvent);
    document.addEventListener("keydown", key, true);
    return () => {
      document.removeEventListener("keydown", key, true);
      document.removeEventListener("cage-close-modal", closeEvent);
      previous?.focus?.();
    };
  }, []);
  return (
    <div
      className="modal-scrim"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose?.();
      }}
    >
      <section
        ref={panel}
        className={`modal ${wide ? "wide" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <header className="modal-head">
          <h2>{title}</h2>
          {onClose && (
            <button
              className="icon-button close-modal"
              aria-label="关闭面板"
              onClick={onClose}
            >
              ×
            </button>
          )}
        </header>
        {children}
        {footer && <footer className="modal-footer">{footer}</footer>}
      </section>
    </div>
  );
}
