#!/usr/bin/env bash
#
# install-bridge-autostart.sh - keep the Go bridge running across logins.
#
# The bridge is a foreground process, so closing the terminal kills it and the
# MCP tools go dead until someone starts it again by hand. This installs a
# per-user service that starts it at login and restarts it if it falls over:
# a launchd LaunchAgent on macOS, a systemd user unit on Linux.
#
#   ./scripts/install-bridge-autostart.sh            # install
#   ./scripts/install-bridge-autostart.sh --status
#   ./scripts/install-bridge-autostart.sh --uninstall
#
# PAIR FIRST, in a terminal where you can see the QR. A background service has
# nowhere to show one, and the codes rotate about every 60 seconds.
#
# On macOS, also complete one interactive start AFTER pairing and click
# "Always Allow" on the Keychain prompt (see issue #70). Until you do, the key
# read needs an authorization dialog, and a service started at login may have
# no way to get one answered.
#
# No sudo. Everything here is per-user; nothing is installed system-wide.

set -euo pipefail

LABEL="co.myceliumai.whatsapp-mcp.bridge"
ROOT="${WHATSAPP_MCP_ROOT:-$HOME/.claude/whatsapp-mcp}"
BRIDGE_BIN="${WHATSAPP_BRIDGE_BIN:-$ROOT/whatsapp-bridge/bin/whatsapp-bridge}"
LOG_PATH="${WHATSAPP_BRIDGE_LOG:-$ROOT/bridge.log}"
STORE_DIR="${WHATSAPP_STORE_DIR:-$ROOT/store}"

PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT="$UNIT_DIR/whatsapp-mcp-bridge.service"

OS="$(uname -s)"

usage() {
  echo "usage: $0 [--install|--status|--uninstall] [--force]"
  echo "  env overrides: WHATSAPP_MCP_ROOT, WHATSAPP_BRIDGE_BIN, WHATSAPP_BRIDGE_LOG, WHATSAPP_STORE_DIR"
}

ACTION="install"
FORCE=0
for arg in "$@"; do
  case "$arg" in
    --install)   ACTION="install" ;;
    --status)    ACTION="status" ;;
    --uninstall) ACTION="uninstall" ;;
    --force)     FORCE=1 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "unknown option: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

# Non-interactive runners inherit a stripped PATH (launchd's default omits
# /opt/homebrew/bin and /usr/local/bin), which is why voice-note transcription
# fails under a service with "ffmpeg: executable file not found in $PATH" even
# when Homebrew has it installed. See docs/TROUBLESHOOTING.md. Bake a usable
# PATH into the service definition rather than shipping that footgun.
SERVICE_PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

status_macos() {
  if [ ! -f "$PLIST" ]; then
    echo "Not installed. No LaunchAgent at $PLIST"
    return 1
  fi
  echo "LaunchAgent: $PLIST"
  # `launchctl list <label>` prints the pid and last exit status when loaded.
  if launchctl list "$LABEL" 2>/dev/null; then
    :
  else
    echo "  (not currently loaded)"
  fi
  return 0
}

status_linux() {
  if [ ! -f "$UNIT" ]; then
    echo "Not installed. No systemd unit at $UNIT"
    return 1
  fi
  echo "systemd unit: $UNIT"
  systemctl --user status whatsapp-mcp-bridge.service --no-pager || true
  return 0
}

tail_log() {
  if [ -f "$LOG_PATH" ]; then
    echo
    echo "Last 10 log lines ($LOG_PATH):"
    tail -n 10 "$LOG_PATH" | sed 's/^/  /'
  fi
}

case "$ACTION" in
  status)
    rc=0
    case "$OS" in
      Darwin) status_macos || rc=$? ;;
      Linux)  status_linux || rc=$? ;;
      *) echo "unsupported OS: $OS" >&2; exit 2 ;;
    esac
    tail_log
    exit "$rc"
    ;;

  uninstall)
    case "$OS" in
      Darwin)
        if [ -f "$PLIST" ]; then
          launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true
          rm -f "$PLIST"
          echo "Removed $PLIST and stopped the agent."
        else
          echo "Nothing to remove; no LaunchAgent at $PLIST"
        fi
        ;;
      Linux)
        if [ -f "$UNIT" ]; then
          systemctl --user disable --now whatsapp-mcp-bridge.service 2>/dev/null || true
          rm -f "$UNIT"
          systemctl --user daemon-reload 2>/dev/null || true
          echo "Removed $UNIT and stopped the service."
        else
          echo "Nothing to remove; no unit at $UNIT"
        fi
        ;;
      *) echo "unsupported OS: $OS" >&2; exit 2 ;;
    esac
    exit 0
    ;;
esac

# ---------- install ----------
if [ ! -x "$BRIDGE_BIN" ]; then
  echo "Bridge binary not found (or not executable) at:"
  echo "  $BRIDGE_BIN"
  echo
  echo "Download it from https://github.com/adelaidasofia/whatsapp-mcp/releases/latest"
  echo "  macOS Apple Silicon: whatsapp-bridge-darwin-arm64"
  echo "  macOS Intel:         whatsapp-bridge-darwin-amd64"
  echo "  Linux:               whatsapp-bridge-linux-amd64"
  echo
  echo "then: chmod +x <file> && mv <file> \"$BRIDGE_BIN\""
  echo "Or set WHATSAPP_BRIDGE_BIN to the real path."
  exit 1
fi

if [ ! -f "$STORE_DIR/session.db" ] && [ "$FORCE" -eq 0 ]; then
  echo "This bridge has never been started, so it is certainly not paired yet."
  echo
  echo "Pair first, in a terminal where you can see the QR:"
  echo "  \"$BRIDGE_BIN\""
  echo
  echo "Scan it with WhatsApp > Settings > Linked Devices > Link a Device."
  echo "Then re-run this script. Use --force to install anyway."
  exit 1
fi

mkdir -p "$(dirname "$LOG_PATH")"

case "$OS" in
  Darwin)
    mkdir -p "$HOME/Library/LaunchAgents"
    cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BRIDGE_BIN</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$SERVICE_PATH</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>30</integer>
    <key>StandardOutPath</key>
    <string>$LOG_PATH</string>
    <key>StandardErrorPath</key>
    <string>$LOG_PATH</string>
    <key>WorkingDirectory</key>
    <string>$(dirname "$BRIDGE_BIN")</string>
</dict>
</plist>
PLIST_EOF
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null || launchctl load "$PLIST"
    echo "Installed LaunchAgent: $PLIST"
    echo "  Runs: $BRIDGE_BIN"
    echo "  Log:  $LOG_PATH"
    echo
    echo "Status:  $0 --status"
    echo "Remove:  $0 --uninstall"
    ;;

  Linux)
    mkdir -p "$UNIT_DIR"
    cat > "$UNIT" <<UNIT_EOF
[Unit]
Description=whatsapp-mcp Go bridge
After=network-online.target

[Service]
Type=simple
ExecStart=$BRIDGE_BIN
WorkingDirectory=$(dirname "$BRIDGE_BIN")
Environment=PATH=$SERVICE_PATH
Restart=on-failure
RestartSec=30
StandardOutput=append:$LOG_PATH
StandardError=append:$LOG_PATH

[Install]
WantedBy=default.target
UNIT_EOF
    systemctl --user daemon-reload
    systemctl --user enable --now whatsapp-mcp-bridge.service
    echo "Installed systemd user unit: $UNIT"
    echo "  Runs: $BRIDGE_BIN"
    echo "  Log:  $LOG_PATH"
    echo
    echo "The unit only runs while you are logged in unless lingering is on:"
    echo "  sudo loginctl enable-linger \"$USER\""
    echo
    echo "Status:  $0 --status"
    echo "Remove:  $0 --uninstall"
    ;;

  *)
    echo "unsupported OS: $OS" >&2
    echo "Windows: use scripts\\install-bridge-autostart.ps1" >&2
    exit 2
    ;;
esac
