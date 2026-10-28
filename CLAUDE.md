# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Global push-to-toggle voice dictation for **GNOME Wayland** (verified on Ubuntu 24.04). Press a hotkey to start recording, press again to stop → local GPU transcription (faster-whisper / Qwen3-ASR / Fun-ASR-Nano) → text auto-pasted at the cursor. Auto-detects Chinese/English.

`ARCHITECTURE.md` (Chinese) is the authoritative deep-dive on design decisions and the gotchas behind them; read it before changing process management, IPC, or the install flow.

## Commands

```bash
go build -o voicetype ./cmd              # build the single binary
go vet ./...                            # there are no tests in this repo

# run / debug
./voicetype daemon                       # run daemon in foreground (systemd normally does this)
./voicetype toggle                       # trigger start/stop manually (same as the hotkey)
./voicetype doctor                       # dependency / GPU / mic / hotwords / focus / service self-check
./voicetype config                       # open the settings web UI (served by the daemon)
journalctl --user -u voicetype -f        # daemon logs (first place to look)
systemctl --user restart voicetype       # apply config changes (restarts sidecar too)
```

## Architecture

Three runtime processes, **two codebases** (one Go binary, one Python script):

- **daemon** (`voicetype daemon`) — long-lived systemd `--user` service. Owns the recording state machine, the warm sidecar, and the virtual keyboard. Listens on a unix socket and serves the settings web UI on `127.0.0.1:27865`. Socket + web come up **before** the model loads (hotkey during load → "loading" notice); a sidecar that fails to load leaves the daemon up with the error shown in the web UI, while a sidecar that dies later (or exceeds the 120s transcribe timeout) makes the daemon exit 1 so systemd restarts both.
- **toggle** (`voicetype toggle`) — transient client; GNOME runs it on each hotkey press. Dials the socket, sends `toggle`, exits. Same binary as the daemon, dispatched by `os.Args[1]` in `cmd/main.go`.
- **sidecar** (`internal/assets/sidecar/transcribe.py`) — Python child of the daemon. Keeps the ASR model resident in VRAM; daemon talks to it over **line-delimited JSON** on stdin/stdout. Launched with `Pdeathsig=SIGTERM` so it never orphans and pins GPU memory. The script dispatches on `VOICETYPE_BACKEND`: `whisper` (default, faster-whisper), `qwen` (Qwen3-ASR) or `funasr` (Fun-ASR-Nano). Library output is redirected to stderr so only protocol JSON reaches stdout. The daemon rewrites the on-disk `transcribe.py` from the embedded copy on every start (`SyncScript`).

End-to-end flow on a press: GNOME → `voicetype toggle` → unix socket → daemon. First press starts `pw-record` (16kHz mono s16 WAV). Second press SIGINTs `pw-record` to flush a valid WAV, pipes the path to the sidecar, gets text back, puts it on the clipboard via `wl-copy`, then injects a paste through `/dev/uinput` chosen by focus: an AT-SPI probe (`focus.go` runs embedded `focus.py` with the **system** python3 + PyGObject) classifies the focused widget as editable → Ctrl+V, terminal → Ctrl+Shift+V, none → no keystroke (text stays on clipboard / GPaste), unknown (Chrome/Electron without a11y) → Ctrl+V.

### Package layout
- `internal/dictation/` — daemon, socket server, sidecar protocol, recorder, uinput keyboard, clipboard output, focus detection, settings web UI (`web.go` + embedded `web/index.html`), notifications, path/env config.
- `internal/setup/` — `setup [backend|all]` (extract sidecar + `uv sync`, default: only the configured backend), `install-service`, `install-hotkey`, `uninstall`, `doctor`, `config`.
- `internal/assets/` — `go:embed` of the shared `transcribe.py` + a `pyproject.toml` per backend (`sidecar/` whisper, `sidecar-qwen/` qwen, `sidecar-funasr/` funasr). `voicetype setup` extracts these and builds **separate uv venvs** under `~/.local/share/voicetype/{sidecar,sidecar-qwen,sidecar-funasr}`. The split is mandatory: faster-whisper needs CUDA-12 cuDNN wheels while torch (qwen) pulls CUDA-13 cuDNN, and the two cuDNN lineages in one venv cause `CUDNN_STATUS_SUBLIBRARY_VERSION_MISMATCH` at inference. The daemon runs the shared `transcribe.py` with the venv python selected by backend (`BackendDir` in `paths.go`).

## Critical constraints (Wayland)

The whole design works around two Wayland security restrictions; preserve these workarounds:

1. **No global hotkey capture** → register a GNOME custom shortcut via `gsettings` that runs `voicetype toggle`. We never grab keys ourselves.
2. **No key injection into other windows** → drive `/dev/uinput` directly in pure Go to inject Ctrl+V. We use **clipboard + paste** (not per-character key simulation) because keycodes can't type CJK. Do **not** re-add clipboard backup/restore — it races with the target app reading the clipboard and was deliberately removed.

## Other landmines (see ARCHITECTURE.md §7, §10)

- **uv must be resolved by absolute path** (`findUV` in `sidecar.go`): systemd `--user` PATH excludes `~/.local/bin`.
- **Sidecar is exec'd as the venv python directly**, not `uv run`, so the GPU-holding process is a direct child and dies with the daemon (Pdeathsig).
- **`install-service` bakes session env vars** (`WAYLAND_DISPLAY`, `DISPLAY`, `XDG_CURRENT_DESKTOP`) into the unit file — systemd `--user` lacks them, breaking `wl-copy`/`notify-send`. ASR/mic config deliberately lives in config.toml, NOT the unit, so reinstalling the service never clobbers it.
- **`source` (mic pin)** dodges Intel SOF laptops defaulting to an empty jack mic; threaded as `cfg.Source` into `startRecording`.
- **Never capture `wl-copy` output through a pipe** (`CombinedOutput`, a `bytes.Buffer`): wl-copy forks a child that keeps serving the selection and inherits stdout/stderr, so `Wait` blocks until another client takes the clipboard over — GPaste doesn't, so the paste keystroke was delayed indefinitely. `wlCopy` sends stderr to the daemon's own fd instead.
- **gnome-shell ignores the notify-osd `x-canonical-private-synchronous` hint.** Notifications share one slot by ID (`notify-send -p` / `-r`), and the "recording" banner is closed via D-Bus `CloseNotification` the moment recording stops (synchronously, so it can't race a later error notice). No success notification.
- **Sidecar load is retried** (5s, 15s; 3 attempts) before the daemon settles into the error state; a ready notice reports the load time, and a `voicetype.sock.recovering` marker turns it into a "recovered" notice after a crash restart.
- Recordings auto-stop after `maxRecording` (5 min) so a forgotten second press can't grow the WAV until it blows the 120s transcribe timeout.
- Fun-ASR-Nano is built into funasr ≥ 1.4.16: load it **without** `trust_remote_code` (with it, funasr logs a spurious "Loading remote code failed: model" on every start).
- Don't enable a11y globally (`org.a11y.Status.IsEnabled`) to make Chrome/Electron visible to the focus probe — it slows every Chromium app.
- Hotwords (`~/.config/voicetype/hotwords.txt`, re-read on mtime) bias **strongly**: a wrong homophone pulls correct output wrong. Keep the list curated; don't bulk-import IME user dictionaries.
- Pinned `source` names can change (SOF renamed `hw_sofhdadsp_6` → `HiFi__Mic1`); a missing target silently falls back to the default (empty jack) mic. The web UI flags missing sources and has a test-record level meter.

## Config

Lives in `~/.config/voicetype/config.toml` (flat `key = value`, parsed by a tiny zero-dependency TOML subset in `loadConfigFile`, `paths.go`; the web UI writes it back with `SaveConfig`, replacing values in place and keeping comments). `setup` scaffolds a documented default if absent. A matching `VOICETYPE_*` env var overrides the file (one-off runs). Apply changes with `systemctl --user restart voicetype`.

| Key (env) | Default | Notes |
|---|---|---|
| `backend` (`VOICETYPE_BACKEND`) | `whisper` | `whisper` / `qwen` / `funasr` — each has its own venv |
| `model` (`VOICETYPE_MODEL`) | per backend | **must match backend**, or an absolute local model dir: whisper `large-v3`/`large-v3-turbo`/`medium`/`small`; qwen `Qwen/Qwen3-ASR-0.6B`/`-1.7B`; funasr `FunAudioLLM/Fun-ASR-Nano-2512` |
| `device` (`VOICETYPE_DEVICE`) | `cuda` | `cuda` or `cpu` |
| `compute` (`VOICETYPE_COMPUTE`) | `float16` | `float16`/`int8_float16`/`int8` (whisper only) |
| `lang` (`VOICETYPE_LANG`) | "" (auto) | force a language, e.g. `zh`, `en` |
| `source` (`VOICETYPE_SOURCE`) | "" (default) | pin a `pw-record --target` input device |
| `sound` (`VOICETYPE_SOUND`) | `crystal` | start/stop cues synthesized in Go (`sound.go`: low sci-fi `warp`/`nebula`/`sonar`, bright `crystal`/`pulse`/`halo`, or `off`; low presets keep ≥40% energy above 300 Hz because laptop speakers can't play the fundamental), played with `pw-play`; recording starts 250ms into the start cue so its loud body isn't captured. Applied live from the web UI |
| `web` (`VOICETYPE_WEB`) | `127.0.0.1:27865` | settings UI listen addr; `off` disables (27865: clear of Gradio's 7860+ and the 7890 proxy) |

## Runtime dependencies

System package: `wl-clipboard`. Plus PipeWire (`pw-record`, `pactl`), libnotify (`notify-send`), `uv`, NVIDIA driver, system `python3-gi` + `gir1.2-atspi-2.0` for focus detection (optional; without them every paste is Ctrl+V). `/dev/uinput` must be writable (udev rule sets `0666`). CUDA libs come from pip (`nvidia-cublas-cu12`/`nvidia-cudnn-cu12`), not system CUDA.
