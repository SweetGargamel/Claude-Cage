using System.Diagnostics;
using System.Text;
using System.Text.Json;

namespace Cage.Windows;

internal static class Backend
{
    private static string ConfigPath => Path.GetFullPath(
        Environment.GetEnvironmentVariable("CLAUDE_CAGE_CONFIG") is { Length: > 0 } configured
            ? configured : Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile), ".claude-cage", "config.json"));
    public static string Root => Path.GetDirectoryName(ConfigPath)!;
    public static string Executable => Path.Combine(AppContext.BaseDirectory, "resources", "cage.exe");

    public static ProcessStartInfo StartInfo(string executable, params string[] arguments)
    {
        var start = new ProcessStartInfo(executable) {
            UseShellExecute = false, CreateNoWindow = true, RedirectStandardOutput = true,
            RedirectStandardError = true, StandardOutputEncoding = Encoding.UTF8, StandardErrorEncoding = Encoding.UTF8,
            WorkingDirectory = AppContext.BaseDirectory,
        };
        // Helpers execute outside the installed directory. Keep an explicitly
        // selected relative profile bound to the same absolute file.
        start.Environment["CLAUDE_CAGE_CONFIG"] = ConfigPath;
        foreach (var arg in arguments) start.ArgumentList.Add(arg);
        return start;
    }

    public static Task<(int Exit, JsonElement Data)> Json(string[] arguments, CancellationToken cancellation = default) =>
        JsonAt(Executable, arguments, cancellation);

    public static async Task<(int Exit, JsonElement Data)> JsonAt(string executable, string[] arguments, CancellationToken cancellation = default)
    {
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellation);
        timeout.CancelAfter(TimeSpan.FromMinutes(3));
        using var process = new Process { StartInfo = StartInfo(executable, arguments) };
        if (!process.Start()) throw new IOException("无法启动 Cage 后台");
        using var abort = timeout.Token.Register(() => { try { if (!process.HasExited) process.Kill(); } catch (InvalidOperationException) { } });
        try
        {
            var errors = Drain(process.StandardError, timeout.Token);
            var text = await ReadLimited(process.StandardOutput, 65536, timeout.Token);
            await process.WaitForExitAsync(timeout.Token); await errors;
            try { return (process.ExitCode, JsonDocument.Parse(text).RootElement.Clone()); }
            catch (JsonException) { throw new IOException("Cage 后台未返回有效状态，请查看工作台日志"); }
        }
        finally { if (!process.HasExited) process.Kill(); }
    }

    public static async Task<string> ReadLimited(StreamReader reader, int limit, CancellationToken token)
    {
        var text = new StringBuilder(); var buffer = new char[4096]; int count;
        while ((count = await reader.ReadAsync(buffer.AsMemory(), token)) > 0)
        {
            text.Append(buffer, 0, count);
            if (text.Length > limit) throw new IOException("后台响应超过允许大小");
        }
        return text.ToString();
    }

    public static async Task Drain(StreamReader reader, CancellationToken token)
    {
        var buffer = new char[4096];
        while (await reader.ReadAsync(buffer.AsMemory(), token) > 0) { }
    }
    public static string Text(this JsonElement data, string name) =>
        data.TryGetProperty(name, out var value) && value.ValueKind == JsonValueKind.String ? value.GetString()! : "";
}
