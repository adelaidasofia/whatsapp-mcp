"""Turning on voice-note transcription in one step (enable_transcription.py).

What a person depends on:

- Looking changes nothing, and names what it would download or install.
- The bridge refuses to start when it is told to transcribe and the model file
  is missing, so the model comes first and the switch-on comes last: settings
  are written only after the model is on disk and both helpers resolve.
- A damaged download never becomes the model. A bridge that does not come back
  gets its old settings back.
- The settings written are ones the bridge reads, and nothing else in the
  member's .env is touched.
"""

from __future__ import annotations

import hashlib
import http.client
import os
import sys
from pathlib import Path

import pytest

import enable_transcription as et


@pytest.fixture(autouse=True)
def world(tmp_path, monkeypatch):
    """A home and a bridge folder of their own; the feature is treated as supported."""
    for var in ("HOME", "USERPROFILE"):
        monkeypatch.setenv(var, str(tmp_path / "home"))
    monkeypatch.setenv("WHATSAPP_MCP_ROOT", str(tmp_path / "root"))
    for var in ("WHATSAPP_WHISPER_MODEL_PATH", "WHATSAPP_WHISPER_LANGUAGE", "WHATSAPP_BRIDGE_PORT"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setattr(et, "supported", lambda: True)
    return tmp_path


class FakeSetup(et.Setup):
    """The real setup with the outside world swapped for a log of what it was asked to do."""

    def __init__(self, *, model=None, missing=(), download_fails=False, install_fails=False,
                 bridge_comes_back=True):
        self.said: list[str] = []
        super().__init__(say=self.said.append)
        self.calls: list = []
        self.model = model
        self.missing = set(missing)
        self.download_fails = download_fails
        self.install_fails = install_fails
        self.bridge_comes_back = bridge_comes_back

    def find_model(self):
        # The real lookup, in a home with no model in it, gives the real wording.
        return (self.model, "found") if self.model else super().find_model()

    def find_helper(self, h):
        return None if h.program in self.missing else f"/opt/bin/{h.program}"

    def download_model(self):
        self.calls.append("download_model")
        if self.download_fails:
            raise et.SetupError("The download stopped.")
        path = self.root / "models" / et.MODEL_FILE
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b"model")
        return path

    def install_helpers(self, missing):
        self.calls.append("install:" + ",".join(h.program for h in missing))
        if self.install_fails:
            raise et.SetupError("Homebrew could not install them.")
        self.missing -= {h.program for h in missing}

    def write_settings(self, model, helpers):
        self.calls.append(("write_settings", Path(model).exists()))  # was the model really there?
        return super().write_settings(model, helpers)

    def restart_bridge(self):
        self.calls.append(("restart", self.env_file.exists()))
        return True

    def wait_for_bridge(self, seconds=90):
        self.calls.append("wait")
        return {"backend": "local-cpp"} if self.bridge_comes_back else None

    def check_a_note(self, tx, seconds=180):
        self.calls.append("check")
        return 0


def test_looking_changes_nothing_and_names_what_it_would_do(monkeypatch):
    monkeypatch.setattr(sys, "platform", "darwin")
    s = FakeSetup(missing={"ffmpeg"})

    assert s.run(yes=False) == 0

    assert s.calls == [], "looking must not download, install, save or restart anything"
    assert not s.env_file.exists()
    text = "\n".join(s.said)
    assert "ffmpeg" in text and "34 MB" in text, "each package to install is named with its size"
    assert "1.6 GB" in text, "the one-time download is named with its size"
    assert "--yes" in text, "it says how to go ahead"


def test_steps_run_in_order_and_the_model_is_on_disk_before_the_bridge_is_switched_on():
    s = FakeSetup(missing={"ffmpeg"})

    assert s.run(yes=True) == 0

    assert s.calls == [
        "download_model",
        "install:ffmpeg",
        ("write_settings", True),  # the model file existed when the settings were written
        ("restart", True),  # and the settings existed when the bridge was restarted
        "wait",
        "check",
    ]
    settings = et.read_env(s.env_file)
    assert settings["WHATSAPP_WHISPER_BACKEND"] == "local-cpp"
    assert Path(settings["WHATSAPP_WHISPER_MODEL_PATH"]).is_file()
    assert settings["WHATSAPP_WHISPER_LANGUAGE"] == "auto"
    assert settings["WHATSAPP_FFMPEG_BIN_PATH"] == "/opt/bin/ffmpeg"


def test_a_model_already_on_disk_is_not_downloaded_again(world):
    existing = world / "somewhere" / et.MODEL_FILE
    existing.parent.mkdir(parents=True)
    existing.write_bytes(b"model")
    s = FakeSetup(model=existing)

    assert s.run(yes=True) == 0

    assert "download_model" not in s.calls
    kept = Path(et.read_env(s.env_file)["WHATSAPP_WHISPER_MODEL_PATH"])
    assert kept == s.root / "models" / et.MODEL_FILE and os.path.samefile(kept, existing), (
        "a model found elsewhere is linked into our folder, so removing the original cannot stop the bridge starting"
    )


def test_a_failed_download_switches_nothing_on():
    s = FakeSetup(download_fails=True)

    assert s.run(yes=True) == 1

    assert s.calls == ["download_model"]
    assert not s.env_file.exists(), "a bridge told to transcribe with no model would refuse to start"


def test_a_helper_that_cannot_be_installed_switches_nothing_on():
    s = FakeSetup(missing={"ffmpeg"}, install_fails=True)

    assert s.run(yes=True) == 1

    assert not s.env_file.exists()
    assert not any(c[0] in ("write_settings", "restart") for c in s.calls if isinstance(c, tuple))


def test_a_bridge_that_does_not_come_back_gets_its_old_settings_back():
    s = FakeSetup(bridge_comes_back=False)
    original = "WHATSAPP_VAULT_CRM_PATH=/some/vault/CRM\nWHATSAPP_WHISPER_LANGUAGE=en\n"
    s.env_file.parent.mkdir(parents=True)
    s.env_file.write_text(original, encoding="utf-8")

    assert s.run(yes=True) == 1

    assert s.env_file.read_text(encoding="utf-8") == original
    assert [c for c in s.calls if isinstance(c, tuple) and c[0] == "restart"] == [("restart", True)] * 2, (
        "the bridge is restarted again so it runs on the old settings"
    )


def test_a_bridge_that_did_not_have_settings_before_has_none_after_a_failed_try():
    s = FakeSetup(bridge_comes_back=False)

    assert s.run(yes=True) == 1

    assert not s.env_file.exists()


def test_settings_keep_everything_else_and_a_language_that_was_chosen():
    before = (
        "# my bridge\n"
        "WHATSAPP_BRIDGE_PORT=8090\n"
        "WHATSAPP_WHISPER_BACKEND=off  # turned off for now\n"
        "WHATSAPP_WHISPER_LANGUAGE=pt\n"
        "WHATSAPP_VAULT_CRM_PATH=/vault with space/CRM\n"
    )
    s = FakeSetup()
    s.env_file.parent.mkdir(parents=True)
    s.env_file.write_text(before, encoding="utf-8")
    s.env = et.read_env(s.env_file)

    assert s.run(yes=True) == 0

    after = s.env_file.read_text(encoding="utf-8")
    for line in ("# my bridge", "WHATSAPP_BRIDGE_PORT=8090", "WHATSAPP_VAULT_CRM_PATH=/vault with space/CRM"):
        assert line in after, f"{line!r} must survive untouched"
    settings = et.read_env(s.env_file)
    assert settings["WHATSAPP_WHISPER_BACKEND"] == "local-cpp"
    assert settings["WHATSAPP_WHISPER_LANGUAGE"] == "pt", "a language the member chose is not overridden"
    assert after.count("WHATSAPP_WHISPER_BACKEND") == 1, "set in place, not added a second time"


def test_the_old_spanish_default_is_replaced_by_automatic_detection():
    s = FakeSetup()
    s.env_file.parent.mkdir(parents=True)
    s.env_file.write_text("WHATSAPP_WHISPER_LANGUAGE=es\n", encoding="utf-8")
    s.env = et.read_env(s.env_file)

    assert s.run(yes=True) == 0

    assert et.read_env(s.env_file)["WHATSAPP_WHISPER_LANGUAGE"] == "auto"


def test_settings_with_a_space_in_a_path_read_back_the_same():
    updates = {"WHATSAPP_WHISPER_MODEL_PATH": "/data/some one/models/ggml.bin"}
    text = et.update_env_text("", updates)
    again = et.update_env_text(text, updates)
    parsed = dict(kv for kv in map(et.parse_env_line, again.splitlines()) if kv)
    assert parsed == updates
    assert again == text, "running it twice changes nothing"


# --- the model --------------------------------------------------------------


@pytest.fixture
def tiny_model(monkeypatch):
    """Stand in for the real 1.6 GB model: same checks, 100 bytes."""
    data = b"m" * 100
    monkeypatch.setattr(et, "MODEL_BYTES", len(data))
    monkeypatch.setattr(et, "MODEL_SHA256", hashlib.sha256(data).hexdigest())
    return data


def test_a_model_found_in_the_usual_places_is_reused(world, tiny_model):
    shared = world / "home" / ".local" / "share" / "whisper-models" / et.MODEL_FILE
    shared.parent.mkdir(parents=True)
    shared.write_bytes(tiny_model)

    found, _ = et.Setup(say=lambda s: None).find_model()

    assert found == shared


def test_a_damaged_copy_is_not_used(world, tiny_model):
    shared = world / "home" / ".local" / "share" / "whisper-models" / et.MODEL_FILE
    shared.parent.mkdir(parents=True)
    shared.write_bytes(b"x" * len(tiny_model))  # right size, wrong bytes

    found, note = et.Setup(say=lambda s: None).find_model()

    assert found is None
    assert "damaged" in note


def test_a_download_that_fails_its_checksum_never_becomes_the_model(tiny_model, monkeypatch):
    def bad_fetch(url, part, say=print):
        part.write_bytes(b"x" * len(tiny_model))  # complete, but not the right bytes

    monkeypatch.setattr(et, "fetch", bad_fetch)
    s = et.Setup(say=lambda s: None)

    with pytest.raises(et.SetupError, match="checksum"):
        s.download_model()

    models = s.root / "models"
    assert list(models.iterdir()) == [], "neither the model nor a half-finished copy is left behind"


def test_a_download_that_matches_its_checksum_becomes_the_model(tiny_model, monkeypatch):
    monkeypatch.setattr(et, "fetch", lambda url, part, say=print: part.write_bytes(tiny_model))
    s = et.Setup(say=lambda s: None)

    path = s.download_model()

    assert path == s.root / "models" / et.MODEL_FILE
    assert path.read_bytes() == tiny_model
    assert not path.with_name(path.name + ".part").exists()


def test_not_enough_disk_stops_before_downloading(tiny_model, monkeypatch):
    called = []
    monkeypatch.setattr(et, "fetch", lambda *a, **k: called.append(1))
    monkeypatch.setattr(et.shutil, "disk_usage", lambda p: type("U", (), {"free": 10})())
    s = et.Setup(say=lambda s: None)

    with pytest.raises(et.SetupError, match="free disk space"):
        s.download_model()

    assert called == []


# --- found by the independent review ----------------------------------------


def test_a_download_the_server_cut_short_keeps_what_arrived(tiny_model, monkeypatch):
    half = tiny_model[: len(tiny_model) // 2]
    # The server hangs up early and cleanly: urllib returns what came, with no error.
    monkeypatch.setattr(et, "fetch", lambda url, part, say=print: part.write_bytes(half))
    s = et.Setup(say=lambda s: None)

    with pytest.raises(et.SetupError, match="stopped early"):
        s.download_model()

    part = s.root / "models" / (et.MODEL_FILE + ".part")
    assert part.read_bytes() == half, "what arrived is kept, so the next run carries on instead of starting over"


def test_a_response_cut_off_mid_body_is_a_message_not_a_traceback(tiny_model, monkeypatch):
    def cut(url, part, say=print):
        raise http.client.IncompleteRead(b"x", 5)

    monkeypatch.setattr(et, "fetch", cut)

    with pytest.raises(et.SetupError, match="Run this again"):
        et.Setup(say=lambda s: None).download_model()


# Keep in step with TestSettingsWrittenByTheSetupCommandBootTheBridge in
# whatsapp-bridge/voice_note_setup_test.go: the text the command writes into a
# bridge folder that had no .env, which the bridge must be able to read.
GOLDEN_FRESH_ENV = """\
# Voice-note transcription (set by enable_transcription.py)
WHATSAPP_WHISPER_BACKEND=local-cpp
WHATSAPP_WHISPER_MODEL_PATH={model}
WHATSAPP_WHISPER_BIN_PATH=/opt/homebrew/bin/whisper-cli
WHATSAPP_FFMPEG_BIN_PATH=/opt/homebrew/bin/ffmpeg
WHATSAPP_WHISPER_LANGUAGE=auto
"""


def test_the_settings_it_writes_into_a_new_env_are_exactly_these(world):
    model = world / "models" / "ggml-large-v3-turbo.bin"
    s = FakeSetup()

    s.write_settings(model, {"whisper-cli": "/opt/homebrew/bin/whisper-cli", "ffmpeg": "/opt/homebrew/bin/ffmpeg"})

    assert s.env_file.read_text(encoding="utf-8") == GOLDEN_FRESH_ENV.format(model=model)


def test_a_crlf_env_stays_crlf_and_a_failed_try_restores_it_byte_for_byte():
    original = b"WHATSAPP_VAULT_CRM_PATH=/vault\r\nWHATSAPP_WHISPER_BACKEND=off\r\n"
    failing = FakeSetup(bridge_comes_back=False)
    failing.env_file.parent.mkdir(parents=True)
    failing.env_file.write_bytes(original)
    assert failing.run(yes=True) == 1
    assert failing.env_file.read_bytes() == original

    working = FakeSetup()
    working.env_file.write_bytes(original)
    assert working.run(yes=True) == 0
    text = working.env_file.read_bytes().decode("utf-8")
    assert "\n" not in text.replace("\r\n", ""), "every line ending is still CRLF"
    assert "WHATSAPP_VAULT_CRM_PATH=/vault" in text


def test_a_value_holding_an_odd_line_separator_is_not_split_in_two():
    # str.splitlines() also splits on U+2028, \x85, \x0c ...; the bridge splits on \n only.
    before = "WHATSAPP_VAULT_CRM_PATH=/vault\u2028name\nWHATSAPP_WHISPER_BACKEND=off\n"

    after = et.update_env_text(before, {"WHATSAPP_WHISPER_BACKEND": "local-cpp"})

    parsed = dict(kv for kv in map(et.parse_env_line, after.split("\n")) if kv)
    assert parsed["WHATSAPP_VAULT_CRM_PATH"] == "/vault\u2028name"
    assert parsed["WHATSAPP_WHISPER_BACKEND"] == "local-cpp"


@pytest.mark.skipif(sys.platform == "win32", reason="symlinks need privileges on Windows")
def test_a_symlinked_env_stays_a_symlink(world):
    real = world / "real.env"
    real.write_text("A=1\n", encoding="utf-8")
    link = world / ".env"
    link.symlink_to(real)

    et.write_text_atomic(link, "A=2\n")

    assert link.is_symlink() and real.read_text(encoding="utf-8") == "A=2\n"


def test_an_env_that_is_not_text_is_left_alone():
    s = FakeSetup()
    s.env_file.parent.mkdir(parents=True)
    s.env_file.write_bytes(b"A=\xff\xfe\n")

    assert s.run(yes=True) == 1

    assert s.env_file.read_bytes() == b"A=\xff\xfe\n"
    assert not any(isinstance(c, tuple) and c[0] == "restart" for c in s.calls)


def test_the_old_spanish_default_is_replaced_once_then_the_members_choice_stands():
    first = FakeSetup()
    first.env_file.parent.mkdir(parents=True)
    first.env_file.write_text("WHATSAPP_WHISPER_LANGUAGE=es\n", encoding="utf-8")
    first.env = et.read_env(first.env_file)
    first.run(yes=False)
    assert "auto" in "\n".join(first.said), "the look step says the language will change"
    assert first.run(yes=True) == 0
    assert et.read_env(first.env_file)["WHATSAPP_WHISPER_LANGUAGE"] == "auto"

    # The member goes back to Spanish on purpose and runs the command again.
    text = first.env_file.read_text(encoding="utf-8").replace("LANGUAGE=auto", "LANGUAGE=es")
    first.env_file.write_text(text, encoding="utf-8")
    again = FakeSetup()
    assert again.run(yes=True) == 0
    assert et.read_env(again.env_file)["WHATSAPP_WHISPER_LANGUAGE"] == "es"


def test_a_model_from_a_shared_folder_is_linked_into_ours_and_outlives_the_original(world):
    shared = world / "home" / ".local" / "share" / "whisper-models" / et.MODEL_FILE
    shared.parent.mkdir(parents=True)
    shared.write_bytes(b"model")
    s = et.Setup(say=lambda s: None)

    kept = s.keep_model_safe(shared)
    shared.unlink()  # some other tool cleans up its folder

    assert kept == s.root / "models" / et.MODEL_FILE
    assert kept.read_bytes() == b"model", "the bridge refuses to start without its model file"


def test_when_a_link_is_not_possible_the_original_is_used(world, monkeypatch):
    shared = world / "elsewhere" / et.MODEL_FILE
    shared.parent.mkdir(parents=True)
    shared.write_bytes(b"model")
    monkeypatch.setattr(et.os, "link", lambda *a: (_ for _ in ()).throw(OSError("different disk")))

    assert et.Setup(say=lambda s: None).keep_model_safe(shared) == shared


def test_a_chosen_es_survives_when_the_env_already_has_every_key():
    # A .env copied from .env.example already holds every key the command writes,
    # so nothing is appended: the "this has run here" marker must still be left.
    copied = ("WHATSAPP_WHISPER_BACKEND=off\nWHATSAPP_WHISPER_LANGUAGE=es\n"
              "WHATSAPP_WHISPER_BIN_PATH=\nWHATSAPP_FFMPEG_BIN_PATH=\nWHATSAPP_WHISPER_MODEL_PATH=\n")
    first = FakeSetup()
    first.env_file.parent.mkdir(parents=True)
    first.env_file.write_text(copied, encoding="utf-8")
    first.env = et.read_env(first.env_file)
    assert first.run(yes=True) == 0
    assert et.read_env(first.env_file)["WHATSAPP_WHISPER_LANGUAGE"] == "auto"

    first.env_file.write_text(first.env_file.read_text(encoding="utf-8").replace("LANGUAGE=auto", "LANGUAGE=es"),
                              encoding="utf-8")
    again = FakeSetup()
    assert again.run(yes=True) == 0
    assert et.read_env(again.env_file)["WHATSAPP_WHISPER_LANGUAGE"] == "es"
    assert again.env_file.read_text(encoding="utf-8").count(et.MARKER) == 1, "the marker is not added twice"
