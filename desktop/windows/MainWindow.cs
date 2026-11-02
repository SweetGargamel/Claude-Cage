using System.Text.Json;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace Cage.Windows;

internal sealed class MainWindow : Form
{
    private WebView2 web = new() { Dock = DockStyle.Fill, Visible = false };
    private readonly Label message = new() { AutoSize = true, MaximumSize = new Size(720, 0), Text = "正在启动 Cage 工作台…", Margin = new Padding(12) };
    private readonly FlowLayoutPanel status = new() { Dock = DockStyle.Fill, FlowDirection = FlowDirection.TopDown, Padding = new Padding(48), WrapContents = false };
    private readonly Button retry = new() { Text = "重试连接", AutoSize = true, Visible = false };
    private readonly Button existing = new() { Text = "打开现有环境保存文件", AutoSize = true, Visible = false };
    private readonly Button complete = new() { Text = "保存后停止环境并完成更新", AutoSize = true, Visible = false };
    private readonly NotifyIcon tray;
    private readonly AppUpdater updater;
    private int expectedPort;
    private bool controllerNeedsUpdate, allowQuit, bootstrapping, webFailed;
    private readonly CancellationTokenSource lifetime = new();

    public MainWindow()
    {
        Text = "Cage"; Width = 1280; Height = 850; MinimumSize = new Size(820, 580);
        StartPosition = FormStartPosition.CenterScreen; BackColor = Color.FromArgb(247, 247, 247);
        Icon = Icon.ExtractAssociatedIcon(Environment.ProcessPath!) ?? SystemIcons.Application;
        var menus = new MenuStrip();
        var file = new ToolStripMenuItem("文件");
        AddAction(file, "打开项目…", "openProject", Keys.Control | Keys.O);
        AddAction(file, "新建网页", "newTab", Keys.Control | Keys.T);
        AddAction(file, "新建终端", "newTerminal", Keys.Control | Keys.Shift | Keys.Enter);
        AddAction(file, "关闭当前标签", "closeTab", Keys.Control | Keys.W);
        file.DropDownItems.Add("关闭窗口（保留环境）", null, (_, _) => Hide());
        file.DropDownItems.Add("退出 Cage…", null, (_, _) => RequestQuit());
        var settings = new ToolStripMenuItem("设置");
        AddAction(settings, "打开设置", "settings", Keys.Control | Keys.Oemcomma);
        settings.DropDownItems.Add("检查应用更新…", null, (_, _) => updater!.Handle("check"));
        var help = new ToolStripMenuItem("帮助");
        help.DropDownItems.Add("关于 Cage", null, (_, _) => MessageBox.Show(this, $"Cage {AppUpdater.Version}\n本机隔离编码工作区", "关于 Cage"));
        menus.Items.AddRange([file, settings, help]); MainMenuStrip = menus;
        status.Controls.AddRange([message, retry, existing, complete]);
        Controls.Add(web); Controls.Add(status); Controls.Add(menus);
        retry.Click += async (_, _) => {
            if (webFailed || web.CoreWebView2 == null) await Initialize(true); else await Bootstrap();
        };
        existing.Click += async (_, _) => await Bootstrap("--existing");
        complete.Click += async (_, _) => {
            if (ConfirmStop()) await Bootstrap("--complete-update");
        };
        var trayMenu = new ContextMenuStrip();
        trayMenu.Items.Add("打开 Cage", null, (_, _) => RestoreWindow());
        trayMenu.Items.Add("退出 Cage…", null, (_, _) => RequestQuit());
        tray = new NotifyIcon { Icon = Icon, Text = "Cage", Visible = true, ContextMenuStrip = trayMenu };
        tray.DoubleClick += (_, _) => RestoreWindow();
        updater = new AppUpdater(PublishUpdate, ConfirmStop, () => { allowQuit = true; Close(); }, () => _ = Bootstrap());
        Shown += async (_, _) => {
            if (Program.SmokeOutput != null) _ = SmokeDeadline();
            await Initialize();
        };
        Resize += (_, _) => PublishVisibility(); VisibleChanged += (_, _) => PublishVisibility();
        FormClosing += (_, e) => {
            if (!allowQuit && e.CloseReason is not (CloseReason.WindowsShutDown or CloseReason.TaskManagerClosing)) { e.Cancel = true; Hide(); }
        };
        FormClosed += (_, _) => { lifetime.Cancel(); updater.Dispose(); tray.Dispose(); web.Dispose(); };
    }

    private void AddAction(ToolStripMenuItem menu, string label, string action, Keys shortcut) =>
        menu.DropDownItems.Add(new ToolStripMenuItem(label, null, (_, _) => Action(action)) { ShortcutKeys = shortcut });
    private bool ConfirmStop() => MessageBox.Show(this,
        "请先保存编辑器中的文件并结束正在运行的工作。继续后 Cage 会停止环境、安装已验证的更新并重新启动。",
        "保存并停止环境后安装重启", MessageBoxButtons.OKCancel, MessageBoxIcon.Information) == DialogResult.OK;

    public void RestoreWindow() { Show(); WindowState = FormWindowState.Normal; Activate(); PublishVisibility(); }
    private void RequestQuit()
    {
        RestoreWindow();
        if (web.Visible) Action("quit");
        else { allowQuit = true; Close(); }
    }
    private void Action(string name) => Script("window.cageNativeAction?.(" + JsonSerializer.Serialize(name) + ")");
    private void PublishVisibility() => Script($"window.cageWindowVisible = {(Visible && WindowState != FormWindowState.Minimized ? "true" : "false")}; window.dispatchEvent(new Event('cage-window-visibility'));");
    private void PublishUpdate(Dictionary<string, object?> state) => Script("window.dispatchEvent(new CustomEvent('cage-app-update',{detail:" + JsonSerializer.Serialize(state) + "}));");
    private async void Script(string script)
    {
        if (web.CoreWebView2 == null || IsDisposed) return;
        try { await web.ExecuteScriptAsync(script); }
        catch (InvalidOperationException) { }
        catch (System.Runtime.InteropServices.COMException) { }
    }
    private void Error(string text)
    {
        message.Text = text; status.Show(); status.BringToFront(); retry.Show(); web.Hide();
    }

    private async Task Initialize(bool recreate = false)
    {
        retry.Enabled = false;
        try
        {
            if (recreate)
            {
                Controls.Remove(web); web.Dispose();
                web = new WebView2 { Dock = DockStyle.Fill, Visible = false };
                Controls.Add(web); status.BringToFront();
            }
            var environment = await CoreWebView2Environment.CreateAsync(null, Path.Combine(Backend.Root, "webview2"));
            await web.EnsureCoreWebView2Async(environment);
            var core = web.CoreWebView2;
            core.Settings.AreDevToolsEnabled = false; core.Settings.AreDefaultContextMenusEnabled = false;
            core.Settings.IsStatusBarEnabled = false; core.Settings.IsZoomControlEnabled = false;
            core.Settings.AreBrowserAcceleratorKeysEnabled = false;
            core.Settings.IsPasswordAutosaveEnabled = false; core.Settings.IsGeneralAutofillEnabled = false;
            core.NavigationStarting += (_, e) => { if (!UpdatePolicy.IsWorkbench(e.Uri, expectedPort)) e.Cancel = true; };
            core.NewWindowRequested += (_, e) => e.Handled = true;
            core.DownloadStarting += (_, e) => e.Cancel = true;
            core.PermissionRequested += (_, e) => e.State = CoreWebView2PermissionState.Deny;
            core.WebMessageReceived += Receive;
            core.ProcessFailed += (_, _) => { webFailed = true; Error("工作台显示进程已退出。后台任务保留，可重试连接。"); };
            core.NavigationCompleted += (_, e) => {
                if (!e.IsSuccess) { Error("无法连接 Cage 工作台。后台任务保留，可重试连接。"); return; }
                status.Hide(); web.Show(); web.Focus(); PublishVisibility();
            };
            await core.AddScriptToExecuteOnDocumentCreatedAsync(BridgeScript);
            webFailed = false;
            await Bootstrap();
        }
        catch (WebView2RuntimeNotFoundException)
        {
            Error("缺少 Microsoft Edge WebView2 Runtime。请运行 Cage 安装程序安装随附运行时后重试。");
            webFailed = true;
        }
        catch (Exception error) { webFailed = true; Error("无法初始化工作台：" + error.Message); }
        finally { retry.Enabled = true; }
    }

    private async Task SmokeDeadline()
    {
        try { await Task.Delay(TimeSpan.FromSeconds(120), lifetime.Token); allowQuit = true; Close(); }
        catch (OperationCanceledException) { }
    }

    private async Task CompleteSmoke()
    {
        if (Program.SmokeOutput == null) return;
        try
        {
            var (exit, data) = await Backend.Json(["prepare-update", "--json"], lifetime.Token);
            if (exit != 0 || data.Text("status") != "prepared") throw new IOException("自检结束时无法确认后台已停止");
            File.WriteAllText(Program.SmokeOutput, JsonSerializer.Serialize(new {
                success = true, nativeBridgeReady = true, version = AppUpdater.Version,
                webviewVersion = web.CoreWebView2.Environment.BrowserVersionString,
            }));
        }
        catch (Exception error) { Error(error.Message); }
        finally { allowQuit = true; BeginInvoke(Close); }
    }

    private async Task Bootstrap(params string[] options)
    {
        if (bootstrapping || web.CoreWebView2 == null) return;
        bootstrapping = true;
        message.Text = "正在连接 Cage 工作台…"; status.Show(); status.BringToFront();
        retry.Hide(); existing.Hide(); complete.Hide();
        try
        {
            var (exit, data) = await Backend.Json(["desktop", "--json", ..options], lifetime.Token);
            if (exit != 0) throw new IOException(data.Text("message") is { Length: > 0 } detail ? detail : "工作台未能启动");
            controllerNeedsUpdate = data.Text("status") != "ready";
            if (data.Text("status") == "upgrade_pending")
            {
                message.Text = data.Text("message"); existing.Show(); complete.Show(); return;
            }
            var entry = UpdatePolicy.Entry(data.Text("url")); expectedPort = entry.Port;
            Text = controllerNeedsUpdate ? "Cage（更新待完成）" : "Cage";
            web.CoreWebView2.Navigate(entry.AbsoluteUri);
        }
        catch (OperationCanceledException) { if (!lifetime.IsCancellationRequested) Error("连接后台超时，请重试。"); }
        catch (Exception error) { Error(error.Message); }
        finally { bootstrapping = false; }
    }

    private async void Receive(object? sender, CoreWebView2WebMessageReceivedEventArgs e)
    {
        // CoreWebView2.WebMessageReceived is top-document only. Frame messages
        // have a separate CoreWebView2Frame event, which this host never handles.
        if (!UpdatePolicy.IsWorkbench(e.Source, expectedPort)) return;
        string id = "";
        try
        {
            if (e.WebMessageAsJson.Length > 2 * 1024 * 1024) throw new InvalidDataException("请求过大");
            var body = JsonDocument.Parse(e.WebMessageAsJson).RootElement;
            id = body.Text("id");
            if (!Guid.TryParse(id, out _)) return;
            var request = body.GetProperty("request");
            object? result;
            switch (request.Text("action"))
            {
                case "workbenchReady":
                    if (controllerNeedsUpdate) throw new IOException("后台版本尚未同步，更新未完成");
                    updater.MarkReady(); result = true; _ = CompleteSmoke(); break;
                case "appUpdate": result = updater.Handle(request.Text("operation")); break;
                case "chooseFolder":
                    using (var picker = new FolderBrowserDialog { Description = "仅此文件夹会与 Cage 共享。请选择具体项目。", UseDescriptionForTitle = true, ShowNewFolderButton = true })
                        result = picker.ShowDialog(this) == DialogResult.OK ? picker.SelectedPath : "";
                    break;
                case "clipboardRead": result = Clipboard.ContainsText() ? Clipboard.GetText() : ""; break;
                case "clipboardWrite":
                    if (request.Text("text") is { Length: > 0 } text) Clipboard.SetText(text); else Clipboard.Clear();
                    result = true; break;
                case "appearance":
                    var mode = request.Text("mode"); var background = request.Text("background");
                    if (mode is not ("system" or "light" or "dark") || !System.Text.RegularExpressions.Regex.IsMatch(background, "^#[0-9a-fA-F]{6}$"))
                        throw new InvalidDataException("外观参数无效");
                    BackColor = ColorTranslator.FromHtml(background); web.DefaultBackgroundColor = BackColor;
                    web.CoreWebView2.Profile.PreferredColorScheme = mode == "dark" ? CoreWebView2PreferredColorScheme.Dark : mode == "light" ? CoreWebView2PreferredColorScheme.Light : CoreWebView2PreferredColorScheme.Auto;
                    result = true; break;
                case "refreshSession":
                    var (exit, data) = await Backend.Json(controllerNeedsUpdate ? ["desktop", "--json", "--existing"] : ["desktop", "--json"], lifetime.Token);
                    if (exit != 0) throw new IOException("工作台会话恢复失败");
                    result = new { url = UpdatePolicy.Entry(data.Text("url")).AbsoluteUri }; break;
                case "reconnect": result = true; _ = Bootstrap(); break;
                case "quitNow": result = true; allowQuit = true; BeginInvoke(Close); break;
                default: throw new InvalidDataException("未知原生操作");
            }
            web.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(new { id, result }));
        }
        catch (Exception error)
        {
            if (id.Length != 0 && !IsDisposed) web.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(new { id, error = error.Message }));
        }
    }

    private const string BridgeScript = """
        (() => {
          if (window !== window.top || location.protocol !== 'http:' || location.hostname !== 'localhost') return;
          const pending = new Map();
          chrome.webview.addEventListener('message', ({data}) => {
            const item = pending.get(data.id); if (!item) return;
            pending.delete(data.id); clearTimeout(item.timer);
            data.error ? item.reject(new Error(data.error)) : item.resolve(data.result);
          });
          const cage = {postMessage(request) { return new Promise((resolve, reject) => {
            const id = crypto.randomUUID();
            const timer = setTimeout(() => { pending.delete(id); reject(new Error('原生操作超时，请重试')); }, 240000);
            pending.set(id, {resolve, reject, timer}); chrome.webview.postMessage({id, request});
          }); }};
          Object.defineProperty(window, 'webkit', {value: {messageHandlers: {cage}}, writable: false});
        })();
        """;
}
