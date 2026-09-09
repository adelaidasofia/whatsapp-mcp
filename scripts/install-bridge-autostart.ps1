# install-bridge-autostart.ps1 - keep the Go bridge running across logins.
#
# The bridge is a foreground process. Close the terminal and it dies, so the
# MCP tools go dead until someone starts it again by hand. This registers a
# per-user Scheduled Task that starts it at logon and restarts it if it falls
# over.
#
# Usage:
#   powershell -ExecutionPolicy ByPass -File scripts\install-bridge-autostart.ps1
#   powershell -ExecutionPolicy ByPass -File scripts\install-bridge-autostart.ps1 -Status
#   powershell -ExecutionPolicy ByPass -File scripts\install-bridge-autostart.ps1 -Uninstall
#
# PAIR FIRST. Run the bridge in your own terminal and scan the QR before you
# install this. A scheduled task runs with no visible window, so if the account
# is not paired yet the task just sits at a QR prompt nobody can see, and the
# codes rotate every ~60 seconds anyway.
#
# No administrator rights needed: the task is registered for the current user
# only and runs at that user's normal privilege level.
#
# ASCII only, on purpose. A .ps1 saved as UTF-8 WITHOUT a BOM that contains a
# non-ASCII punctuation character (an em dash is the usual culprit) gets read
# as cp1252 by Windows PowerShell 5.1, which can turn it into a quote character
# and break parsing in a way that reads like a syntax error somewhere else.

[CmdletBinding()]
param(
    [string]$BridgeExe = "$env:USERPROFILE\.claude\whatsapp-mcp\whatsapp-bridge\bin\whatsapp-bridge.exe",
    [string]$LogPath   = "$env:USERPROFILE\.claude\whatsapp-mcp\bridge.log",
    [string]$StoreDir  = "$env:USERPROFILE\.claude\whatsapp-mcp\store",
    [string]$TaskName  = "whatsapp-mcp-bridge",
    [switch]$Uninstall,
    [switch]$Status,
    [switch]$Force,
    [switch]$KeepRunning
)

$ErrorActionPreference = "Stop"

function Get-Task {
    try { Get-ScheduledTask -TaskName $TaskName -ErrorAction Stop } catch { $null }
}

# ---------- status ----------
if ($Status) {
    $t = Get-Task
    if (-not $t) {
        Write-Host "Not installed. No scheduled task named '$TaskName'."
        exit 1
    }
    $info = Get-ScheduledTaskInfo -TaskName $TaskName
    Write-Host "Task:        $TaskName"
    Write-Host "State:       $($t.State)"
    Write-Host "Last run:    $($info.LastRunTime)"
    # 0 = ok, 267009 = currently running. Anything else is worth reading.
    Write-Host "Last result: $($info.LastTaskResult)"
    Write-Host "Next run:    $($info.NextRunTime)"
    if (Test-Path $LogPath) {
        Write-Host ""
        Write-Host "Last 10 log lines ($LogPath):"
        Get-Content $LogPath -Tail 10 | ForEach-Object { Write-Host "  $_" }
    }
    exit 0
}

# ---------- uninstall ----------
if ($Uninstall) {
    if (-not (Get-Task)) {
        Write-Host "Nothing to remove; no task named '$TaskName'."
        exit 0
    }
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "Removed scheduled task '$TaskName'."

    # Stopping the TASK does not stop the bridge. The action runs cmd.exe purely
    # to redirect output, so the bridge is a grandchild; Task Scheduler reports
    # the task stopped while the process keeps serving. Measured: 1 process
    # before Unregister, 1 after. Someone who just asked to uninstall does not
    # expect WhatsApp to still be linked, so stop it here and say so.
    if (-not $KeepRunning) {
        $procs = @(Get-Process -Name "whatsapp-bridge" -ErrorAction SilentlyContinue)
        if ($procs.Count -gt 0) {
            foreach ($p in $procs) {
                $where = try { $p.Path } catch { "(path unavailable)" }
                Write-Host "Stopping running bridge (PID $($p.Id)) $where"
                Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
            }
        } else {
            Write-Host "No running bridge process found."
        }
    } else {
        Write-Host "Left the running bridge alone (-KeepRunning). It keeps serving until you close it."
    }
    exit 0
}

# ---------- install ----------
if (-not (Test-Path $BridgeExe)) {
    Write-Host "Bridge binary not found at:"
    Write-Host "  $BridgeExe"
    Write-Host ""
    Write-Host "Download it from https://github.com/adelaidasofia/whatsapp-mcp/releases/latest"
    Write-Host "(whatsapp-bridge-windows-amd64.exe), or pass -BridgeExe with the real path."
    exit 1
}

$sessionDb = Join-Path $StoreDir "session.db"
if (-not (Test-Path $sessionDb) -and -not $Force) {
    Write-Host "This bridge has never been started, so it is certainly not paired yet."
    Write-Host ""
    Write-Host "Pair first, in a terminal where you can see the QR:"
    Write-Host "  `"$BridgeExe`""
    Write-Host ""
    Write-Host "Scan it with WhatsApp > Settings > Linked Devices > Link a Device."
    Write-Host "Then re-run this script. Use -Force to install anyway."
    exit 1
}

$logDir = Split-Path -Parent $LogPath
if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Force -Path $logDir | Out-Null }

$workDir = Split-Path -Parent $BridgeExe

# Task Scheduler cannot redirect a process's output on its own, so the action
# runs the bridge through cmd.exe purely to append stdout+stderr to a log. The
# doubled quotes are cmd's own rule for `cmd /c "..."` when the program path
# itself is quoted; without them a path containing a space silently fails.
$cmdArgs = '/c ""{0}" >> "{1}" 2>&1"' -f $BridgeExe, $LogPath

$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\cmd.exe" `
                                  -Argument $cmdArgs `
                                  -WorkingDirectory $workDir

$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# Every one of these settings is load-bearing:
#   AllowStartIfOnBatteries / DontStopIfGoingOnBatteries - the DEFAULT is to
#     refuse to start on battery and to kill the task when a laptop unplugs.
#     On a student's laptop that default means the task appears installed and
#     simply never runs.
#   ExecutionTimeLimit 0 - the default stops a task after 3 days. This is a
#     long-lived process; it should not be reaped.
#   RestartCount/RestartInterval - come back after a crash or a network drop.
#   MultipleInstances IgnoreNew - never start a second bridge on top of the
#     first; they would fight over the same SQLite store and the same port.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew

if (Get-Task) {
    Write-Host "Replacing existing task '$TaskName'."
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
}

Register-ScheduledTask -TaskName $TaskName `
                       -Action $action `
                       -Trigger $trigger `
                       -Settings $settings `
                       -Description "Starts the whatsapp-mcp Go bridge at logon and keeps it running." `
                       | Out-Null

Write-Host "Installed scheduled task '$TaskName'."
Write-Host "  Runs:    $BridgeExe"
Write-Host "  At:      logon of $env:USERNAME"
Write-Host "  Log:     $LogPath"
Write-Host ""
Write-Host "Start it now without logging out:"
Write-Host "  Start-ScheduledTask -TaskName $TaskName"
Write-Host ""
Write-Host "Check it:      powershell -ExecutionPolicy ByPass -File scripts\install-bridge-autostart.ps1 -Status"
Write-Host "Remove it:     powershell -ExecutionPolicy ByPass -File scripts\install-bridge-autostart.ps1 -Uninstall"
exit 0
