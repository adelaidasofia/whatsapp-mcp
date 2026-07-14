# Setup

Step-by-step install for macOS, Windows, and Linux.

Two paths to a running bridge:

- **Path A — prebuilt binary (recommended, and the only sane path on Windows).** Download, configure, run. No Go, no C compiler.
- **Path B — build from source.** Needs Go + a C toolchain (the database is SQLCipher-encrypted, which compiles C).

Either way you'll also need **Python 3.11+** and **uv** for the MCP server layer (step 5).

## 1a. Path A — download the prebuilt bridge

Grab the binary for your OS from the [latest release](https://github.com/adelaidasofia/whatsapp-mcp/releases/latest) (releases v0.2.0 and newer carry binaries + SHA256 files):

- `whatsapp-bridge-darwin-arm64` — macOS (Apple Silicon)
- `whatsapp-bridge-windows-amd64.exe` — Windows
- `whatsapp-bridge-linux-amd64` — Linux

Clone the repo anyway (it carries `.env.example` and the MCP server), then drop the binary in place:

```bash
# macOS / Linux
git clone https://github.com/adelaidasofia/whatsapp-mcp.git "$HOME/.claude/whatsapp-mcp"
mkdir -p "$HOME/.claude/whatsapp-mcp/whatsapp-bridge/bin"
mv ~/Downloads/whatsapp-bridge-* "$HOME/.claude/whatsapp-mcp/whatsapp-bridge/bin/whatsapp-bridge"
chmod +x "$HOME/.claude/whatsapp-mcp/whatsapp-bridge/bin/whatsapp-bridge"
```

```powershell
# Windows PowerShell
git clone https://github.com/adelaidasofia/whatsapp-mcp.git "$env:USERPROFILE\.claude\whatsapp-mcp"
New-Item -ItemType Directory -Force "$env:USERPROFILE\.claude\whatsapp-mcp\whatsapp-bridge\bin" | Out-Null
Move-Item "$env:USERPROFILE\Downloads\whatsapp-bridge-windows-amd64.exe" "$env:USERPROFILE\.claude\whatsapp-mcp\whatsapp-bridge\bin\whatsapp-bridge.exe"
```

Skip to step 3.

## 1b. Path B — prerequisites for building from source

**macOS**

```bash
brew install go
# uv (Python package manager)
curl -LsSf https://astral.sh/uv/install.sh | sh
```

The C toolchain ships with the Xcode Command Line Tools (installed automatically the first time a build needs it).

**Windows (PowerShell)**

```powershell
winget install GoLang.Go
winget install MSYS2.MSYS2
# uv
powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
```

Then install the C compiler inside MSYS2 (one-time; open "MSYS2 UCRT64" from the Start menu):

```bash
pacman -S --noconfirm mingw-w64-ucrt-x86_64-gcc
```

And put it on your PowerShell PATH for the build:

```powershell
$env:Path = "C:\msys64\ucrt64\bin;" + $env:Path
```

**Linux (Debian/Ubuntu)**

```bash
sudo apt-get install golang gcc
curl -LsSf https://astral.sh/uv/install.sh | sh
```

**Required versions:** Go 1.24+ · Python 3.11+ · `uv` (latest). FFmpeg is only needed if you enable voice-note transcription (step 4).

Verify with the included checker:

```bash
# macOS / Linux
scripts/check_prerequisites.sh
```

```powershell
# Windows
powershell -ExecutionPolicy ByPass -File scripts\check_prerequisites.ps1
```

## 2. Path B — clone and build the bridge

```bash
# macOS / Linux
git clone https://github.com/adelaidasofia/whatsapp-mcp.git "$HOME/.claude/whatsapp-mcp"
cd "$HOME/.claude/whatsapp-mcp/whatsapp-bridge"
go build -o bin/whatsapp-bridge .
```

```powershell
# Windows PowerShell (note the .exe — Go appends it on Windows)
git clone https://github.com/adelaidasofia/whatsapp-mcp.git "$env:USERPROFILE\.claude\whatsapp-mcp"
cd "$env:USERPROFILE\.claude\whatsapp-mcp\whatsapp-bridge"
go build -o bin\whatsapp-bridge.exe .
```

## 3. Configure environment

```bash
# macOS / Linux
cd "$HOME/.claude/whatsapp-mcp"
cp .env.example .env
```

```powershell
# Windows PowerShell
cd "$env:USERPROFILE\.claude\whatsapp-mcp"
Copy-Item .env.example .env
```

The bridge loads `.env` automatically from its working directory or from next to the binary (real environment variables always win). **Zero edits are required to boot**: voice transcription defaults to `off`, storage paths default under your home directory on every OS, and the SQLCipher key auto-provisions in the platform secret store — macOS Keychain, Windows Credential Manager, or Linux libsecret. Headless setups can set `WHATSAPP_DB_KEY` (64 hex chars) instead.

The one value you probably want to set:

```
WHATSAPP_VAULT_CRM_PATH=/path/to/your/vault/CRM/
```

This enables vault CRM auto-injection: when Claude reads a chat, the MCP pulls the matching CRM note frontmatter into the response so Claude knows who the person is. Leave empty to disable.

## 4. Voice-note transcription (optional — off by default)

Transcription ships disabled so a fresh install boots with zero downloads. To enable the private local backend:

```
WHATSAPP_WHISPER_BACKEND=local-cpp
WHATSAPP_WHISPER_MODEL_PATH=/absolute/path/to/models/ggml-large-v3.bin
```

Download a model (~3 GB for `large-v3`; `ggml-small.bin` at ~500 MB also works but is meaningfully worse for Latin American Spanish):

```bash
# macOS / Linux
mkdir -p "$HOME/.claude/whatsapp-mcp/models"
curl -L -o "$HOME/.claude/whatsapp-mcp/models/ggml-large-v3.bin" \
  https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3.bin
brew install whisper-cpp ffmpeg   # macOS; Linux: build whisper.cpp + apt-get install ffmpeg
```

**Windows note:** whisper.cpp has no supported Windows package. Use the cloud backend instead (`WHATSAPP_WHISPER_BACKEND=openai-api` + `WHATSAPP_WHISPER_API_KEY=sk-...`, opt-in, sends audio to OpenAI) — or leave transcription off. FFmpeg, needed by `local-cpp` only: `winget install ffmpeg`.

With `local-cpp` enabled, the bridge refuses to boot if the model, `whisper-cli`, or `ffmpeg` are missing — fail-loud at startup instead of silent per-message failures.

## 5. Authenticate

Start the bridge in a terminal:

```bash
# macOS / Linux
cd "$HOME/.claude/whatsapp-mcp/whatsapp-bridge"
./bin/whatsapp-bridge
```

```powershell
# Windows PowerShell
cd "$env:USERPROFILE\.claude\whatsapp-mcp\whatsapp-bridge"
.\bin\whatsapp-bridge.exe
```

On first run a QR code appears and **refreshes in place every ~20 seconds — the QR on screen is always the current one.** When a whole batch expires, the bridge requests fresh codes automatically; it never exits on you mid-pairing. On your phone:

1. Open WhatsApp.
2. Settings › Linked Devices › Link a Device.
3. Scan the QR on screen.
4. Approve the link on your phone.

**Can't scan a QR** (garbled terminal, remote machine, screen reader)? Pair with a typed code instead — works identically on Android and iOS:

```bash
./bin/whatsapp-bridge --pair-phone +15551234567     # your own number, international format
```

The 8-character code prints in the terminal; on the phone use Settings › Linked Devices › Link a Device › **"Link with phone number instead"**.

**Driving this from another program?** The HTTP API is live during pairing: `GET /api/status` (includes `auth_state`), `GET /api/auth/qr` (raw rotating code), `POST /api/auth/pair-phone`, `POST /api/auth/reconnect`.

The first boot also provisions the SQLCipher key (macOS shows a one-time Keychain prompt) and starts syncing message history. Leave the bridge running; it must be active whenever Claude calls the MCP.

## 6. Register the MCP in Claude Code

Add this block to your project `.mcp.json` (at the root of the project you use Claude Code in) or register user-scoped via `claude mcp add`:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "uv",
      "args": [
        "--directory",
        "/Users/YOUR_USERNAME/.claude/whatsapp-mcp/whatsapp-mcp-server",
        "run",
        "main.py"
      ],
      "env": {
        "WHATSAPP_BRIDGE_HOST": "127.0.0.1",
        "WHATSAPP_BRIDGE_PORT": "8080"
      }
    }
  }
}
```

Windows: same block with a Windows path in `--directory`:

```json
        "C:\\Users\\YOUR_USERNAME\\.claude\\whatsapp-mcp\\whatsapp-mcp-server",
```

Replace `YOUR_USERNAME`. Add `WHATSAPP_VAULT_CRM_PATH` (and the whisper vars from step 4, if you enabled transcription) to the `env` block as needed — never leave a key with a blank value, that crashes FastMCP at startup.

Validate the JSON parses (any OS; use `python3` on macOS/Linux if `python` isn't aliased):

```
python -c "import json; json.load(open('.mcp.json')); print('OK')"
```

## 7. Restart Claude Code

Close and reopen Claude Code. Verify the tools appear:

```bash
claude mcp list
```

You should see `whatsapp` in the list with the tool count. If not, check:

- The bridge is still running (`curl http://127.0.0.1:8080/healthcheck` — on Windows use `curl.exe`, not the PowerShell alias)
- The `.mcp.json` parsed successfully
- The `uv` command is on the PATH that Claude Code uses
- No blank env values in the `env` block (those crash FastMCP on startup)

## 8. First tool call

Ask Claude:

> List my 10 most recent WhatsApp chats.

Claude calls `list_chats`, the Python MCP hits the Go bridge, the bridge queries SQLite, you get a list back. If the list is empty, history is still syncing; wait a few minutes.

## Re-authentication

WhatsApp's multidevice protocol expires linked-device sessions periodically (typically around 20 days). When that happens the bridge now handles it in place: it logs the logout, clears the dead session, and **re-enters pairing on its own — a fresh QR starts rotating in the terminal (and over `GET /api/auth/qr`) without a restart.** Re-scan from your phone, or run with `--pair-phone` for a typed code. Supervisors can also trigger it explicitly via `POST /api/auth/reconnect`.

Session recovery for brief disconnections is automatic; only a hard logout requires re-pairing.

## OS-specific notes

**Linux:** SQLCipher key storage uses libsecret — install `secret-tool` (`sudo apt-get install libsecret-tools`) or set `WHATSAPP_DB_KEY`.

**Windows:** SQLCipher key storage uses Windows Credential Manager (target `whatsapp-mcp:default`) — implemented natively, no extra install. Prefer the prebuilt release binary over building from source; if you do build, the MSYS2 UCRT64 gcc from step 1b is required because the encrypted database layer compiles C.

## Uninstall

1. Stop the bridge (Ctrl+C in the terminal running it).
2. Remove the MCP registration from `.mcp.json`.
3. Delete the install folder:
   - macOS/Linux: `rm -rf "$HOME/.claude/whatsapp-mcp/"`
   - Windows: `Remove-Item -Recurse -Force "$env:USERPROFILE\.claude\whatsapp-mcp"`
4. Remove the linked device from your phone: WhatsApp › Settings › Linked Devices › tap the device › Log Out.
5. Delete the stored DB key:
   - macOS: `security delete-generic-password -s whatsapp-mcp -a default`
   - Windows: `cmdkey /delete:whatsapp-mcp:default`
   - Linux: `secret-tool clear service whatsapp-mcp account default`

## Troubleshooting

See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).
