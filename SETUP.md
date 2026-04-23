# Setup

Step-by-step install for macOS. Linux and Windows supported; see OS-specific notes at the end.

## 1. Prerequisites

Install the prereqs via `brew` (macOS) or your platform equivalent:

```bash
brew install go ffmpeg sqlcipher
# uv (Python package manager)
curl -LsSf https://astral.sh/uv/install.sh | sh
```

Required versions:

- Go 1.24+
- Python 3.11+
- FFmpeg (any recent version)
- SQLCipher 4.x
- `uv` (latest)

Verify with the included checker:

```bash
cd whatsapp-mcp
scripts/check_prerequisites.sh
```

## 2. Clone and build the bridge

```bash
git clone https://github.com/adelaidasofia/whatsapp-mcp.git "$HOME/.claude/whatsapp-mcp"
cd "$HOME/.claude/whatsapp-mcp/whatsapp-bridge"
go mod tidy
go build -o bin/whatsapp-bridge .
```

The bridge binary lands at `whatsapp-bridge/bin/whatsapp-bridge`.

## 3. Configure environment

```bash
cd "$HOME/.claude/whatsapp-mcp"
cp .env.example .env
```

Edit `.env` to match your setup. Most defaults are sensible. The one you almost certainly want to set:

```
WHATSAPP_VAULT_CRM_PATH=/path/to/your/vault/CRM/
```

This enables the vault CRM auto-injection feature. When Claude reads a chat, the MCP pulls the matching CRM note frontmatter into the response so Claude knows who the person is. Leave empty to disable.

## 4. Download Whisper model (voice-note transcription)

Default backend is local `whisper.cpp` with the `large-v3` model. Download:

```bash
mkdir -p "$HOME/.claude/whatsapp-mcp/models"
curl -L -o "$HOME/.claude/whatsapp-mcp/models/ggml-large-v3.bin" \
  https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3.bin
```

The model is about 3 GB. If that's too much, `ggml-small.bin` (500 MB) also works but is meaningfully worse for Latin American Spanish with strong regional accents.

Install `whisper.cpp`:

```bash
brew install whisper-cpp
```

The MCP detects the `whisper-cli` binary in `$PATH`. Override with `WHATSAPP_WHISPER_BIN_PATH` in `.env`.

To use the OpenAI API backend instead (opt-in, sends audio to OpenAI):

```
WHATSAPP_WHISPER_BACKEND=openai-api
WHATSAPP_WHISPER_API_KEY=sk-...
```

## 5. Authenticate

Start the bridge in a terminal:

```bash
cd "$HOME/.claude/whatsapp-mcp/whatsapp-bridge"
./bin/whatsapp-bridge
```

On first run, the bridge prints a QR code to your terminal. On your phone:

1. Open WhatsApp.
2. Settings, Linked Devices, Link a Device.
3. Point your phone camera at the QR code in the terminal.
4. Approve the link on your phone.

The bridge exchanges authentication, initializes the Keychain key for SQLCipher (you'll get a macOS prompt the first time), and starts syncing message history.

Leave the bridge running. It needs to be active whenever Claude calls the MCP.

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
        "WHATSAPP_BRIDGE_PORT": "8080",
        "WHATSAPP_VAULT_CRM_PATH": "/path/to/your/vault/CRM/",
        "WHATSAPP_WHISPER_BACKEND": "local-cpp",
        "WHATSAPP_WHISPER_MODEL_PATH": "/Users/YOUR_USERNAME/.claude/whatsapp-mcp/models/ggml-large-v3.bin"
      }
    }
  }
}
```

Replace `YOUR_USERNAME` and the CRM path with your actual values.

Validate the JSON parses:

```bash
python3 -c "import json; json.load(open('.mcp.json'))" && echo OK
```

## 7. Restart Claude Code

Close and reopen Claude Code. Verify the tools appear:

```bash
claude mcp list
```

You should see `whatsapp` in the list with the tool count. If not, check:

- The bridge is still running (port 8080 responding to `curl http://127.0.0.1:8080/healthcheck`)
- The `.mcp.json` parsed successfully
- The `uv` command is on the PATH that Claude Code uses
- No blank env values in the `env` block (those crash FastMCP on startup)

## 8. First tool call

Ask Claude:

> List my 10 most recent WhatsApp chats.

Claude calls `list_chats`, the Python MCP hits the Go bridge, the bridge queries SQLite, you get a list back. If the list is empty, history is still syncing; wait a few minutes.

## Re-authentication

WhatsApp's multidevice protocol expires linked-device sessions periodically (typically around 20 days). When that happens, the bridge logs a `StreamReplaced` or authentication-failure message. Restart the bridge; a fresh QR code prints; re-scan from your phone.

Session recovery for brief disconnections is automatic; only hard re-authentication requires a new QR scan.

## OS-specific notes

**Linux:** works identically to macOS. SQLCipher key storage falls back to a keyring service (install `libsecret`); if no keyring is available, the key is read from `WHATSAPP_DB_KEY` env var (less secure, documented fallback).

**Windows:** requires MSYS2 for the Go build (`go env -w CGO_ENABLED=1`). SQLCipher key storage uses Windows Credential Manager. FFmpeg available via `winget install ffmpeg`.

## Uninstall

1. Stop the bridge (Ctrl+C in the terminal running it).
2. Remove the MCP registration from `.mcp.json`.
3. `rm -rf $HOME/.claude/whatsapp-mcp/` (deletes database, media, audit log).
4. Remove the linked device from your phone: WhatsApp, Settings, Linked Devices, tap the device, Log Out.
5. Delete the Keychain entry: `security delete-generic-password -s whatsapp-mcp -a default`.

## Troubleshooting

See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).
