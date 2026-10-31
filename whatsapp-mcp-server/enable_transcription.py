"""Turn on private voice-note transcription in one step.

    python3 enable_transcription.py          look: list what it would download or
                                             install, and change nothing
    python3 enable_transcription.py --yes    do it

Voice notes arrive as audio. Transcription turns them into words on this
computer; the audio never leaves it. "Do it" runs five steps, in this order:

  1. the speech model: reuse one already on this computer, else download it
     (1.6 GB, checked against a fixed checksum)
  2. the two helper programs, whisper-cli and ffmpeg: installed with Homebrew
     when they are missing
  3. the bridge's settings (.env)
  4. restart the bridge
  5. check that a voice note comes back with words

The order is the point. The bridge refuses to start when it is told to
transcribe and the model file is not there, so the settings are written only
after the model is on disk and verified and both helpers resolve. If the
bridge does not come back after the restart, the old settings go back.

Standard library only, so it runs with any python3.

This module also holds the once-only "turn it on?" hint that list_messages
attaches (take_hint) and the text the voice_note_transcription tool returns.
"""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
import plistlib
import shutil
import subprocess
import sys
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path

# --- What gets installed ----------------------------------------------------

MODEL_LABEL = "large-v3-turbo"
MODEL_SIZE_TEXT = "1.6 GB"
MODEL_FILE = "ggml-large-v3-turbo.bin"
MODEL_BYTES = 1_624_555_275
# Both values are Hugging Face's own record of the file (its Git LFS pointer).
MODEL_SHA256 = "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69"
# Pinned to a revision, so the checksum above always describes the bytes served.
MODEL_URL = (
    "https://huggingface.co/ggerganov/whisper.cpp/resolve/"
    "5359861c739e955e79d9a303bcbc70fb988958b1/" + MODEL_FILE
)
SPARE_BYTES = 500 * 1024 * 1024  # free space to leave after the download

LAUNCHD_LABEL = "co.myceliumai.whatsapp-mcp.bridge"  # scripts/install-bridge-autostart.sh
SYSTEMD_UNIT = "whatsapp-mcp-bridge.service"
# Said once transcription is on. The vault export is a separate job (the bridge's
# --export-to-vault, often run on a schedule); it rewrites a chat's file when a
# transcript arrives, at its next run.
VAULT_NOTE = ("Transcripts reach your vault the next time the vault export runs (the bridge's "
              "--export-to-vault); run it now if you want them sooner.")
# The line above the settings this command adds. Finding it again means the
# command has run here before, so a language chosen since is the member's.
MARKER = "Voice-note transcription (set by enable_transcription.py)"


@dataclass(frozen=True)
class Helper:
    program: str  # the executable the bridge runs
    formula: str  # the Homebrew package that provides it
    what: str
    size: str  # measured on Apple Silicon Homebrew, 2026-10-31; a heads-up, not a promise
    setting: str  # the bridge setting that pins its absolute path


HELPERS = (
    Helper("whisper-cli", "whisper-cpp", "listens to the audio and writes down the words",
           "about 70 MB with its libraries", "WHATSAPP_WHISPER_BIN_PATH"),
    Helper("ffmpeg", "ffmpeg", "converts WhatsApp's audio so the model can read it",
           "about 34 MB with its libraries", "WHATSAPP_FFMPEG_BIN_PATH"),
)


class SetupError(Exception):
    """Something the member needs to know; the message says what to do next."""


def supported() -> bool:
    # whisper.cpp has no supported Windows package (see SETUP.md).
    return sys.platform != "win32"


def mcp_root() -> Path:
    return Path(os.environ.get("WHATSAPP_MCP_ROOT") or Path.home() / ".claude" / "whatsapp-mcp")


def human(n: int) -> str:
    return f"{n / 1e9:.1f} GB" if n >= 1e9 else f"{n / 1e6:.0f} MB"


# --- The bridge's .env ------------------------------------------------------


def parse_env_line(line: str) -> tuple[str, str] | None:
    """KEY=VALUE from one .env line, read the way the bridge reads it (env_file.go)."""
    s = line.strip()
    if not s or s.startswith("#"):
        return None
    if s.startswith("export "):
        s = s[len("export "):]
    key, sep, value = s.partition("=")
    key = key.strip()
    if not sep or not key or " " in key or "\t" in key:
        return None
    value = value.strip()
    if value[:1] not in ("'", '"') and " #" in value:
        value = value[: value.index(" #")].strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        value = value[1:-1]
    return key, value


def read_text_exact(path: Path) -> str | None:
    """The file exactly as it is (line endings included), or None when there is none."""
    try:
        with open(path, encoding="utf-8", newline="") as f:
            return f.read()
    except FileNotFoundError:
        return None
    except UnicodeDecodeError as e:
        raise SetupError(f"{path} is not plain UTF-8 text ({e.reason}), so it was left alone. "
                         "Fix or remove it and run this again.") from e


def read_env(path: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    try:
        text = read_text_exact(path) or ""
    except (OSError, SetupError):
        return out
    for line in text.split("\n"):  # the bridge splits on newlines only
        kv = parse_env_line(line)
        if kv and kv[0] not in out:  # the bridge keeps the first occurrence
            out[kv[0]] = kv[1]
    return out


def update_env_text(text: str, updates: dict[str, str]) -> str:
    """Set the given keys; every other line stays exactly as it was, line endings too."""

    def fmt(v: str) -> str:
        return f'"{v}"' if (" " in v or "#" in v) else v

    lines = text.split("\n")  # not splitlines(): that also splits on \x0c, \x85,   ...
    if lines and lines[-1] == "":
        lines.pop()  # the file ended with a newline
    cr = "\r" if "\r\n" in text else ""
    out, done = [], set()
    for raw in lines:
        body = raw.rstrip("\r")
        kv = parse_env_line(body)
        if kv and kv[0] in updates and kv[0] not in done:
            out.append(f"{kv[0]}={fmt(updates[kv[0]])}" + raw[len(body):])
            done.add(kv[0])
        else:
            out.append(raw)
    added = [k for k in updates if k not in done]
    marked = MARKER in text
    if added or not marked:
        if out and out[-1].strip():
            out.append(cr)
        if not marked:
            # Written even when every key was already there (a .env copied from
            # .env.example has them all): it is how a later run knows this one happened.
            out.append(f"# {MARKER}{cr}")
        out.extend(f"{k}={fmt(updates[k])}{cr}" for k in added)
    return "\n".join(out) + "\n"


def write_text_atomic(path: Path, text: str) -> None:
    target = Path(os.path.realpath(path))  # through a symlinked file, not over the link
    mode = (target.stat().st_mode & 0o777) if target.exists() else 0o600  # a .env may hold keys
    tmp = target.with_name(target.name + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
        f.write(text)
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, mode)
    os.replace(tmp, target)


# --- Files and programs -----------------------------------------------------


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def find_program(name: str) -> str | None:
    found = shutil.which(name)
    if found:
        return found
    for d in ("/opt/homebrew/bin", "/usr/local/bin"):  # a non-interactive shell often lacks these
        p = Path(d) / name
        if p.is_file() and os.access(p, os.X_OK):
            return str(p)
    return None


def fetch(url: str, part: Path, say=print) -> None:
    """Download url into part, carrying on from whatever an earlier try left there."""
    have = part.stat().st_size if part.exists() else 0
    req = urllib.request.Request(url, headers={"User-Agent": "whatsapp-mcp-enable-transcription"})
    if have:
        req.add_header("Range", f"bytes={have}-")
    with urllib.request.urlopen(req, timeout=60) as resp:
        resumed = bool(have) and resp.status == 206
        got = have if resumed else 0  # a 200 means the server sent the whole file again
        mark = got * 100 // MODEL_BYTES // 10 * 10 + 10
        with open(part, "ab" if resumed else "wb") as f:
            while True:
                chunk = resp.read(1 << 20)
                if not chunk:
                    break
                f.write(chunk)
                got += len(chunk)
                if got * 100 // MODEL_BYTES >= mark:
                    say(f"  downloaded {mark}%")
                    mark += 10


# --- The setup --------------------------------------------------------------


class Setup:
    """Each method that touches the outside world is its own step, so the tests
    can swap them and check the order."""

    def __init__(self, say=print):
        self.say = say
        self.root = mcp_root()
        self.bridge_dir = self._bridge_dir()
        self.env_file = self._env_file()
        self.env = read_env(self.env_file)

    # -- where things are ----------------------------------------------------

    def _bridge_dir(self) -> Path:
        """The folder the bridge runs from. It reads its .env from here."""
        plist = Path.home() / "Library" / "LaunchAgents" / f"{LAUNCHD_LABEL}.plist"
        try:
            with open(plist, "rb") as f:
                where = plistlib.load(f).get("WorkingDirectory")
            if where:
                return Path(where)
        except (OSError, ValueError):
            pass
        return self.root / "whatsapp-bridge" / "bin"

    def _env_file(self) -> Path:
        for d in (self.bridge_dir, self.bridge_dir.parent):  # where the bridge looks
            if (d / ".env").is_file():
                return d / ".env"
        return self.bridge_dir / ".env"

    def setting(self, key: str, default: str = "") -> str:
        """For the bridge's address: the shell's value first, as the bridge does."""
        return os.environ.get(key) or self.env.get(key) or default

    def in_env_file(self, key: str) -> str:
        """For what the bridge will do: only its own .env counts, not this shell."""
        return self.env.get(key, "")

    def resolve(self, value: str) -> Path:
        """A path from the .env the way the bridge reads it: relative to its folder."""
        p = Path(os.path.expandvars(os.path.expanduser(value)))
        return p if p.is_absolute() else self.bridge_dir / p

    def bridge_url(self, path: str) -> str:
        host = self.setting("WHATSAPP_BRIDGE_HOST", "127.0.0.1")
        host = f"[{host}]" if ":" in host and not host.startswith("[") else host
        return f"http://{host}:{self.setting('WHATSAPP_BRIDGE_PORT', '8080')}{path}"

    def first_run(self) -> bool:
        try:
            return MARKER not in (read_text_exact(self.env_file) or "")
        except SetupError:
            return True

    def language_would_change(self) -> bool:
        # "es" is the old default that every copied .env.example still carries; it
        # forces English notes through a Spanish decoder. Replaced once. After the
        # command has run here, an "es" is the member's own choice.
        return self.in_env_file("WHATSAPP_WHISPER_LANGUAGE") == "es" and self.first_run()

    # -- looking (changes nothing) -------------------------------------------

    def find_model(self) -> tuple[Path | None, str]:
        configured = self.in_env_file("WHATSAPP_WHISPER_MODEL_PATH")
        if configured:
            p = self.resolve(configured)
            if p.is_file():
                return p, f"already chosen in your settings: {p}"
        damaged = []
        for p in (self.root / "models" / MODEL_FILE,
                  Path.home() / ".local" / "share" / "whisper-models" / MODEL_FILE):
            if not p.is_file():
                continue
            if p.stat().st_size == MODEL_BYTES and sha256_of(p) == MODEL_SHA256:
                return p, f"found on this computer: {p}"
            damaged.append(p)
        note = (f"not on this computer. It will be downloaded once from huggingface.co "
                f"({MODEL_SIZE_TEXT}) and checked against a fixed checksum.")
        if damaged:
            note += f" A copy at {damaged[0]} is incomplete or damaged, so it will not be used."
        return None, note

    def find_helper(self, h: Helper) -> str | None:
        pinned = self.in_env_file(h.setting)
        if pinned and self.resolve(pinned).is_file():
            return str(self.resolve(pinned))
        return find_program(h.program)

    def look(self) -> dict:
        model, note = self.find_model()
        return {"model": model, "model_note": note,
                "helpers": {h.program: self.find_helper(h) for h in HELPERS}}

    def describe(self, look: dict) -> str:
        lines = ["Voice-note transcription: what it needs", ""]
        tag = "OK     " if look["model"] else "NEEDED "
        lines.append(f"  {tag} speech model ({MODEL_LABEL}): {look['model_note']}")
        for h in HELPERS:
            path = look["helpers"][h.program]
            if path:
                lines.append(f"  OK      {h.program}: {path}")
            elif sys.platform == "darwin":
                lines.append(f"  NEEDED  {h.program}: {h.what}. Installed with Homebrew "
                             f"({h.formula}), {h.size}.")
            else:
                lines.append(f"  NEEDED  {h.program}: {h.what}. You install it yourself: {self.manual(h)}")
        if self.settings_in_place(look):
            lines += ["", (f"Your settings in {self.env_file} already say all of this, so none will be "
                           "changed. It will check that the bridge is running them (restarting it only "
                           "if it is not) and look at a waiting voice note.")]
        else:
            lines += ["", "Then it will:", f"  - save the settings in {self.env_file}"]
            if self.language_would_change():
                lines.append("  - change the language setting from es (every note forced through Spanish) "
                             "to auto (detected per note); put es back afterwards if you want it, and a "
                             "later run will leave it")
            lines += ["  - restart the bridge (WhatsApp reconnects within seconds)",
                      "  - check that a voice note comes back with words"]
        lines += ["", "Everything runs on this computer. The audio never leaves it."]
        return "\n".join(lines)

    @staticmethod
    def manual(h: Helper) -> str:
        if h.program == "ffmpeg":
            return "sudo apt-get install ffmpeg" if sys.platform.startswith("linux") else "install ffmpeg"
        return ("build whisper.cpp from source or install your distro's whisper-cpp package"
                if sys.platform.startswith("linux") else "install whisper.cpp")

    # -- doing ---------------------------------------------------------------

    def download_model(self) -> Path:
        dest_dir = self.root / "models"
        dest_dir.mkdir(parents=True, exist_ok=True)
        dest = dest_dir / MODEL_FILE
        part = dest.with_name(dest.name + ".part")
        have = part.stat().st_size if part.exists() else 0
        if have > MODEL_BYTES:  # not a partial copy of this file; start over
            part.unlink()
            have = 0
        free = shutil.disk_usage(dest_dir).free
        if free < MODEL_BYTES - have + SPARE_BYTES:
            raise SetupError(f"Not enough free disk space for the speech model: it needs about "
                             f"{human(MODEL_BYTES - have + SPARE_BYTES)} and {dest_dir} has "
                             f"{human(free)}. Free some space and run this again.")
        if have < MODEL_BYTES:  # a complete .part only needs checking, not asking for again
            self.say(f"Downloading the speech model ({MODEL_SIZE_TEXT}) into {dest_dir} ...")
            try:
                fetch(MODEL_URL, part, self.say)
            except (OSError, http.client.HTTPException) as e:  # a timeout, a reset, a cut-off body
                raise SetupError(f"The download stopped ({e}). Run this again and it carries on "
                                 "from where it stopped.") from e
        size = part.stat().st_size
        if size < MODEL_BYTES:  # the server hung up early and cleanly: keep what arrived
            raise SetupError(f"The download stopped early, at {human(size)} of {MODEL_SIZE_TEXT}. "
                             "Run this again and it carries on from there.")
        if size > MODEL_BYTES or sha256_of(part) != MODEL_SHA256:
            part.unlink(missing_ok=True)
            raise SetupError("The downloaded model did not match its checksum, so it was thrown "
                             "away and nothing was switched on. Run this again.")
        os.replace(part, dest)
        self.say("  speech model downloaded and checked.")
        return dest

    def keep_model_safe(self, model: Path) -> Path:
        """A model found outside our folder is hard-linked into it, so another tool
        deleting the original cannot leave the bridge unable to start (it refuses to
        when the model file is missing). No extra space; if a link is impossible
        (another disk), the original is used as it is. A hard link shares the
        file's data: a tool that replaces its copy (as a fresh download does)
        leaves ours alone, one that rewrites it in place changes ours too."""
        home = self.root / "models"
        configured = self.in_env_file("WHATSAPP_WHISPER_MODEL_PATH")
        if home in model.parents or (configured and self.resolve(configured) == model):
            return model  # already ours, or the member's own choice
        link = home / MODEL_FILE
        try:
            if link.exists():  # ours already (same file), or some other copy we have not verified
                return link if os.path.samefile(link, model) else model
            home.mkdir(parents=True, exist_ok=True)
            os.link(model, link)
        except OSError:
            return model
        self.say(f"Linked the model into {home} so it stays even if the original is removed.")
        return link

    def install_helpers(self, missing: list[Helper]) -> None:
        brew = shutil.which("brew") or next(
            (p for p in ("/opt/homebrew/bin/brew", "/usr/local/bin/brew") if Path(p).is_file()), None)
        if sys.platform != "darwin" or not brew:
            raise SetupError(
                "Install these first, then run this again:\n"
                + "\n".join(f"  {h.program}: {self.manual(h)}" for h in missing)
                + ("\nHomebrew (https://brew.sh) is the easy way on a Mac." if sys.platform == "darwin" else ""))
        self.say("Installing " + " and ".join(f"{h.formula} ({h.size})" for h in missing) + " with Homebrew ...")
        # Install what is missing and nothing else: no upgrading what the member already has.
        env = {**os.environ, "HOMEBREW_NO_INSTALL_UPGRADE": "1",
               "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK": "1", "HOMEBREW_NO_ENV_HINTS": "1"}
        if self.run_streaming([brew, "install", *[h.formula for h in missing]], env=env) != 0:
            raise SetupError("Homebrew could not install them. Its message is above. Nothing was switched on.")

    def planned_settings(self, model: Path, helpers: dict[str, str]) -> dict[str, str]:
        updates = {"WHATSAPP_WHISPER_BACKEND": "local-cpp", "WHATSAPP_WHISPER_MODEL_PATH": str(model)}
        for h in HELPERS:
            updates[h.setting] = helpers[h.program]
        if not self.in_env_file("WHATSAPP_WHISPER_LANGUAGE") or self.language_would_change():
            updates["WHATSAPP_WHISPER_LANGUAGE"] = "auto"
        return updates

    def already_set(self, updates: dict[str, str]) -> bool:
        """True when the .env already says everything in updates. Paths are compared the
        way the bridge reads them, so ~/models and /home/me/models are the same."""
        paths = {"WHATSAPP_WHISPER_MODEL_PATH", *(h.setting for h in HELPERS)}
        for key, want in updates.items():
            have = self.in_env_file(key)
            if not have:
                return False
            if (self.resolve(have) != Path(want)) if key in paths else (have != want):
                return False
        return True

    def settings_in_place(self, look: dict) -> bool:
        return bool(look["model"] and all(look["helpers"].values())
                    and self.already_set(self.planned_settings(look["model"], look["helpers"])))

    def write_settings(self, model: Path, helpers: dict[str, str]) -> tuple[bool, str | None]:
        """Save the bridge's settings. Returns (whether the file changed, the file as it was
        or None if there was none). When the .env already says it all, it is not touched."""
        original = read_text_exact(self.env_file)
        updates = self.planned_settings(model, helpers)
        if self.already_set(updates):
            return False, original
        self.env_file.parent.mkdir(parents=True, exist_ok=True)
        write_text_atomic(self.env_file, update_env_text(original or "", updates))
        self.env = read_env(self.env_file)
        self.say(f"Settings saved in {self.env_file}")
        return True, original

    def restore_settings(self, original: str | None) -> None:
        if original is None:
            self.env_file.unlink(missing_ok=True)
        else:
            write_text_atomic(self.env_file, original)

    def restart_bridge(self) -> bool:
        """Restart the background service. False when there is none to restart."""
        if sys.platform == "darwin":
            target = f"gui/{os.getuid()}/{LAUNCHD_LABEL}"
            if self.run_quiet(["launchctl", "print", target]) != 0:
                return False
            if self.run_quiet(["launchctl", "kickstart", "-k", target]) != 0:
                raise SetupError("The settings are saved but launchd would not restart the bridge. "
                                 "Restart it yourself and run this again.")
            return True
        if sys.platform.startswith("linux") and self.run_quiet(["systemctl", "--user", "cat", SYSTEMD_UNIT]) == 0:
            if self.run_quiet(["systemctl", "--user", "restart", SYSTEMD_UNIT]) != 0:
                raise SetupError("The settings are saved but systemd would not restart the bridge. "
                                 "Restart it yourself and run this again.")
            return True
        return False

    def wait_for_bridge(self, seconds: int = 90) -> dict | None:
        """The bridge's transcription health once it is back and reports local-cpp."""
        deadline = time.time() + seconds
        while time.time() < deadline:
            tx = (self.bridge("GET", "/healthcheck") or {}).get("transcription") or {}
            if tx.get("backend") == "local-cpp":
                return tx
            time.sleep(2)
        return None

    def running_transcription(self, model: Path) -> dict | None:
        """The bridge's transcription health when it is up and already running these settings."""
        tx = (self.bridge("GET", "/healthcheck") or {}).get("transcription") or {}
        language = self.in_env_file("WHATSAPP_WHISPER_LANGUAGE") or "auto"
        if (tx.get("backend") == "local-cpp" and tx.get("language") == language
                and tx.get("model_path") and Path(tx["model_path"]) == model):
            return tx
        return None

    def report_running(self, tx: dict) -> int:
        """The bridge is already doing it: say so, and look at one note only if none has come back lately."""
        waiting = int(tx.get("pending_with_keys") or 0)
        last = int(tx.get("last_transcript_at") or 0)
        ago = int(time.time()) - last if last else None
        self.say("Transcription is already on and the bridge is running these settings, so nothing "
                 "was changed.")
        if ago is not None and ago < 3600:
            self.say(f"A voice note was transcribed {ago // 60} minute(s) ago; {waiting} are still "
                     "waiting their turn.")
            return 0
        return self.check_a_note(tx)

    def check_a_note(self, tx: dict, seconds: int = 180) -> int:
        before = int(tx.get("last_transcript_at") or 0)
        queued = int((self.bridge("POST", "/api/admin/backfill-transcripts") or {}).get("enqueued") or 0)
        if not queued:
            self.say("Transcription is on. No voice note is waiting that can be fetched (WhatsApp keeps "
                     "the audio about 14 days, and the bridge keeps its own copy only of notes that "
                     "arrived while transcription was off), so there was nothing to try it on. "
                     "Your next voice note will come with words.")
            return 0
        self.say(f"Transcription is on. Trying it on {queued} waiting voice note(s) ...")
        started = time.time()
        while time.time() - started < seconds:
            now = int(((self.bridge("GET", "/healthcheck") or {}).get("transcription") or {})
                      .get("last_transcript_at") or 0)
            if now > before:
                self.say(f"A voice note came back with words after {int(time.time() - started)} seconds. "
                         "The rest fill in over the next hours, 50 every 15 minutes.")
                return 0
            time.sleep(3)
        self.say(f"Transcription is on, but none of the {queued} waiting notes finished in "
                 f"{seconds // 60} minutes. They may be past the ~14 days WhatsApp keeps audio. "
                 "Your next new voice note is the real test; the bridge log shows progress "
                 f"({self.root / 'bridge.log'}).")
        return 3

    # -- the outside world ---------------------------------------------------

    def bridge(self, method: str, path: str) -> dict | None:
        req = urllib.request.Request(self.bridge_url(path), method=method,
                                     data=b"{}" if method == "POST" else None,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except (OSError, ValueError):
            return None

    @staticmethod
    def run_streaming(cmd: list[str], env: dict | None = None) -> int:
        try:
            return subprocess.run(cmd, check=False, stdin=subprocess.DEVNULL, env=env).returncode
        except OSError:
            return 127

    @staticmethod
    def run_quiet(cmd: list[str]) -> int:
        try:
            return subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                  check=False).returncode
        except OSError:
            return 127

    # -- the whole thing -----------------------------------------------------

    def finish(self, code: int) -> int:
        self.say(VAULT_NOTE)
        return code

    def run(self, yes: bool) -> int:
        if not supported():
            self.say("Local transcription is not available on Windows: whisper.cpp has no supported "
                     "Windows package. Leave transcription off, or see SETUP.md for the opt-in cloud backend.")
            return 1
        try:
            look = self.look()
            self.say(self.describe(look))
            if not yes:
                self.say("\nNothing has been changed. To go ahead, run this again with --yes.")
                return 0

            # The order matters: see the module docstring.
            model = look["model"] or self.download_model()
            model = self.keep_model_safe(model)
            missing = [h for h in HELPERS if not look["helpers"][h.program]]
            if missing:
                self.install_helpers(missing)
            helpers = {h.program: self.find_helper(h) for h in HELPERS}
            if not all(helpers.values()):
                raise SetupError("A helper program is still missing after the install: "
                                 + ", ".join(k for k, v in helpers.items() if not v))
            changed, original = self.write_settings(model, helpers)
            if not changed:
                # Set up by hand, or by an earlier run: change nothing. If the bridge is
                # already running these settings, do not even restart it.
                self.say("Your settings already say all of this, so none were changed.")
                tx = self.running_transcription(model)
                if tx is not None:
                    return self.finish(self.report_running(tx))
            if not self.restart_bridge():
                self.say("The settings are saved. There is no background service to restart, so stop "
                         "the bridge and start it again to finish.")
                return 0
            self.say("Bridge restarting ...")
            tx = self.wait_for_bridge()
            if tx is None:
                if not changed:
                    raise SetupError("The bridge did not come back with transcription on. Your "
                                     "settings were already right and were not changed. "
                                     f"Its log: {self.root / 'bridge.log'}")
                self.restore_settings(original)
                self.restart_bridge()
                raise SetupError("The bridge did not come back with transcription on, so your old "
                                 "settings are back and it was asked to restart once more. "
                                 f"Its log: {self.root / 'bridge.log'}")
            return self.finish(self.check_a_note(tx))
        except SetupError as e:
            self.say(f"\n{e}")
            return 1
        except OSError as e:
            self.say(f"\nStopped because of a file problem: {e}")
            return 1


# --- The once-only hint -----------------------------------------------------
#
# Asked once, at the moment voice notes show up without words. Saying "not now"
# is remembered on disk and answered with one line. Neither is ever repeated.


def state_file() -> Path:
    return mcp_root() / "voice-note-hint.json"


def _state() -> dict:
    try:
        data = json.loads(state_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _remember(**fields: float) -> bool:
    try:
        state_file().parent.mkdir(parents=True, exist_ok=True)
        write_text_atomic(state_file(), json.dumps({**_state(), **fields}) + "\n")
        return True
    except OSError:
        return False  # a hint must never break a read


def untranscribed(messages: list[dict]) -> int:
    return sum(1 for m in messages
               if m.get("type") in ("voice", "audio") and not m.get("voice_note_transcript"))


def hint_available() -> bool:
    s = _state()
    return supported() and not s.get("hint_shown_at") and not s.get("not_now_at")


def command(*args: str) -> str:
    return " ".join(['python3', f'"{Path(__file__).resolve()}"', *args])


def take_hint(count: int) -> dict | None:
    """The one-time offer. Calling it records that it was shown."""
    if not hint_available() or not _remember(hint_shown_at=time.time()):
        return None  # another reader got there first, or it cannot be recorded (so it would repeat)
    s = "s" if count != 1 else ""
    return {
        "voice_notes_without_text": count,
        "message": (f"{count} voice note{s} here can't be read yet, because transcription is off. "
                    "If you want, I can turn it on. It runs on this computer, so the audio never "
                    f"leaves it, and it needs a one-time download of about {MODEL_SIZE_TEXT}."),
        "next": ("Tell the member this once, in your own words. If they want it, call the "
                 "voice_note_transcription tool for the steps. If they say not now, call it with "
                 "not_now=true. This notice does not appear again."),
    }


def how_to_turn_on() -> dict:
    if not supported():
        return {"transcription": "unavailable",
                "message": "Local transcription is not available on Windows. See SETUP.md."}
    return {
        "transcription": "off",
        "what_it_gives": "Voice notes come with their words, so they can be read, searched and used.",
        "cost": (f"A one-time download of about {MODEL_SIZE_TEXT}, plus two small helper programs "
                 "(ffmpeg and whisper-cpp) if they are missing."),
        "privacy": "Everything runs on this computer. The audio never leaves it.",
        "steps": [
            (f"1. Run: {command()}   (it only looks: it lists exactly what it would download or "
             "install, and changes nothing)"),
            ("2. Tell the member that list in one message and ask if they want it. Install nothing "
             "before they say yes."),
            (f"3. If they say yes, run: {command('--yes')}   (a few minutes; run it in the background "
             "and tell the member when it finishes)"),
            "4. If they say not now, call this tool again with not_now=true and tell them what it returns.",
        ],
    }


def not_now() -> dict:
    saved = _remember(not_now_at=time.time())
    return {"remembered": saved,
            "say_to_member": ("No problem. You can turn this on any time: just ask me to turn on "
                              "voice-note transcription.")}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Turn on private voice-note transcription in one step.")
    ap.add_argument("--yes", action="store_true",
                    help="do it (without this, only list what it would download or install)")
    return Setup().run(ap.parse_args(argv).yes)


if __name__ == "__main__":
    sys.exit(main())
