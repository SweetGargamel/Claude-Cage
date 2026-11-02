#requires -Version 7.0
# Runs only on a disposable Windows test user. Does not start a VM.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$Installer)
. (Join-Path $PSScriptRoot 'windows-common.ps1')
if (-not $IsWindows) { throw 'Installation smoke tests must run on Windows.' }
$target = Join-Path $env:LOCALAPPDATA ('Programs/Cage-test-' + [guid]::NewGuid().ToString('N'))
$report = Join-Path ([IO.Path]::GetTempPath()) ('cage-smoke-' + [guid]::NewGuid().ToString('N') + '.json')
$savedKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\ClaudeCage'
if (Test-Path $savedKey) { throw 'Use a disposable Windows user without an existing Cage installation.' }
function Run-Installer {
    param([switch]$Update, [int]$ExpectedExitCode = 0)
    $arguments = @('/S')
    if ($Update) { $arguments += '/UPDATE' }
    $arguments += "/D=$target"
    $process = Start-Process -FilePath $Installer -ArgumentList $arguments -PassThru
    if (-not $process.WaitForExit(300000)) { throw 'Installer exceeded five minutes.' }
    if ($process.ExitCode -ne $ExpectedExitCode) { throw "Installer returned $($process.ExitCode), expected $ExpectedExitCode" }
}
Run-Installer
if (-not (Test-Path -LiteralPath (Join-Path $target 'Cage.exe'))) { throw 'Installer did not create Cage.exe.' }
Invoke-BuildTool (Join-Path $target 'resources/cage.exe') @('--help')
Invoke-BuildTool (Join-Path $target 'resources/mihomo.exe') @('-v')
Invoke-BuildTool (Join-Path $target 'resources/runtime/qemu/qemu-system-x86_64.exe') @('--version')
Invoke-BuildTool (Join-Path $target 'resources/runtime/openssh/ssh.exe') @('-V')
$window = Start-Process -FilePath (Join-Path $target 'Cage.exe') -ArgumentList @('--smoke-test', "`"$report`"") -PassThru
if (-not $window.WaitForExit(180000)) { throw 'Cage WebView2/workbench smoke test timed out.' }
if ($window.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $report)) { throw "Cage desktop smoke test failed ($($window.ExitCode))." }
Get-Content -LiteralPath $report
# A locked installed executable must fail before replacing the installation.
$before = (Get-FileHash -LiteralPath (Join-Path $target 'Cage.exe') -Algorithm SHA256).Hash
$locked = [IO.File]::Open((Join-Path $target 'Cage.exe'), [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
try { Run-Installer -Update -ExpectedExitCode 2 } finally { $locked.Dispose() }
if ((Get-FileHash -LiteralPath (Join-Path $target 'Cage.exe') -Algorithm SHA256).Hash -ne $before) { throw 'Failed update replaced the existing application.' }
# Reinstall through the real atomic replacement path. User data must survive.
$state = Join-Path $env:USERPROFILE '.claude-cage'
New-Item -ItemType Directory -Path $state -Force | Out-Null
$sentinel = Join-Path $state ('installer-test-' + [guid]::NewGuid().ToString('N'))
[IO.File]::WriteAllText($sentinel, 'preserve')
Run-Installer -Update
if ((Get-Content -LiteralPath $sentinel -Raw) -ne 'preserve') { throw 'Upgrade modified user data.' }
$uninstall = Start-Process -FilePath (Join-Path $target 'Uninstall.exe') -ArgumentList @('/S') -PassThru
if (-not $uninstall.WaitForExit(120000) -or $uninstall.ExitCode -ne 0) { throw 'Uninstall failed.' }
# NSIS may relaunch its uninstaller from TEMP so that it can delete the original.
$deadline = [DateTime]::UtcNow.AddSeconds(120)
while ((Test-Path -LiteralPath (Join-Path $target 'Uninstall.exe')) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
if (Test-Path -LiteralPath (Join-Path $target 'Cage.exe')) { throw 'Uninstall left Cage.exe behind.' }
if (Test-Path -LiteralPath (Join-Path $target 'Uninstall.exe')) { throw 'Uninstall left its launcher behind.' }
if ((Get-Content -LiteralPath $sentinel -Raw) -ne 'preserve') { throw 'Uninstall modified user data.' }
Remove-Item -LiteralPath $sentinel
Write-Host 'Install, WebView2 workbench, reinstall, uninstall and user-data preservation passed. VM startup was not tested.'
