# check_prerequisites.ps1 — Windows twin of check_prerequisites.sh.
# Reports what's present, what's required, and what's only needed for
# source builds. Run:  powershell -ExecutionPolicy ByPass -File scripts\check_prerequisites.ps1
#
# Exit code: 1 if a REQUIRED tool (python 3.11+, uv) is missing; 0 otherwise.
# Go + gcc are only required when building the bridge from source — the
# prebuilt release binary needs neither.

$ErrorActionPreference = "SilentlyContinue"
$failures = 0

function Test-Tool {
    param(
        [string]$Label,
        [string[]]$Candidates,
        [string]$VersionArgs,
        [bool]$Required,
        [string]$Hint
    )
    foreach ($c in $Candidates) {
        $cmd = Get-Command $c -ErrorAction SilentlyContinue
        if ($cmd) {
            $ver = ""
            try { $ver = (& $c $VersionArgs.Split(" ") 2>&1 | Select-Object -First 1) } catch {}
            Write-Host ("  OK       {0,-12} {1}" -f $Label, $ver)
            return $true
        }
    }
    if ($Required) {
        Write-Host ("  MISSING  {0,-12} (required) -> {1}" -f $Label, $Hint)
        $script:failures++
    } else {
        Write-Host ("  absent   {0,-12} (optional) -> {1}" -f $Label, $Hint)
    }
    return $false
}

Write-Host "whatsapp-mcp prerequisites (Windows)"
Write-Host ""
Write-Host "Required for the MCP server:"
$null = Test-Tool -Label "python" -Candidates @("python", "py") -VersionArgs "--version" -Required $true -Hint "winget install Python.Python.3.12"
$null = Test-Tool -Label "uv" -Candidates @("uv") -VersionArgs "--version" -Required $true -Hint "powershell -ExecutionPolicy ByPass -c `"irm https://astral.sh/uv/install.ps1 | iex`""

Write-Host ""
Write-Host "Required only when building the bridge from source (prebuilt release binary needs neither):"
$null = Test-Tool -Label "go" -Candidates @("go") -VersionArgs "version" -Required $false -Hint "winget install GoLang.Go"
$null = Test-Tool -Label "gcc" -Candidates @("gcc") -VersionArgs "--version" -Required $false -Hint "winget install MSYS2.MSYS2, then in MSYS2: pacman -S mingw-w64-ucrt-x86_64-gcc, then add C:\msys64\ucrt64\bin to PATH"

Write-Host ""
Write-Host "Optional (voice-note transcription only; off by default):"
$null = Test-Tool -Label "ffmpeg" -Candidates @("ffmpeg") -VersionArgs "-version" -Required $false -Hint "winget install ffmpeg"
Write-Host "  note     whisper-cli has no supported Windows package; use WHATSAPP_WHISPER_BACKEND=openai-api or leave transcription off"

Write-Host ""
if ($failures -gt 0) {
    Write-Host "$failures required tool(s) missing. Install them and re-run."
    exit 1
}
Write-Host "All required tools present."
exit 0
