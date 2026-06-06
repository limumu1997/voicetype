# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Global push-to-toggle voice dictation for **GNOME Wayland** (verified on Ubuntu 24.04). Press a hotkey to start recording, press again to stop → local faster-whisper (GPU) transcription → text auto-pasted at the cursor. Auto-detects Chinese/English.

`ARCHITECTURE.md` (Chinese) is the authoritative deep-dive on design decisions and the gotchas behind them; read it before changing process management, IPC, or the install flow.

## Commands

```bash
go build -o voicetype ./cmd              # build the single binary
go vet ./...                            # there are no tests in this repo

# run / debug
./voicetype daemon                       # run daemon in foreground (systemd normally does this)
./voicetype toggle                       # trigger start/stop manually (same as the hotkey)
./voicetype doctor                       # dependency / GPU / mic / service self-check
journalctl --user -u voicetype -f        # daemon logs (first place to look)
systemctl --user restart voicetype       # apply config changes (restarts sidecar too)
```

## Architecture

Three runtime processes, **two codebases** (one Go binary, one Python script):

- **daemon** (`voicetype daemon`) — long-lived systemd `--user` service. Owns the recording state machine, the warm sidecar, and the virtual keyboard. Listens on a unix socket.
- **toggle** (`voicetype toggle`) — transient client; GNOME runs it on each hotkey press. Dials the socket, sends `toggle`, exits. Same binary as the daemon, dispatched by `os.Args[1]` in `cmd/main.go`.
- **sidecar** (`internal/assets/sidecar/transcribe.py`) — Python child of the daemon. Keeps the ASR model resident in VRAM; daemon talks to it over **line-delimited JSON** on stdin/stdout. Launched with `Pdeathsig=SIGTERM` so it never orphans and pins GPU memory. The script dispatches on `VOICETYPE_BACKEND`: `whisper` (default, faster-whisper) or `qwen` (Qwen3-ASR open weights).

End-to-end flow on a press: GNOME → `voicetype toggle` → unix socket → daemon. First press starts `pw-record` (16kHz mono s16 WAV). Second press SIGINTs `pw-record` to flush a valid WAV, pipes the path to the sidecar, gets text back, puts it on the clipboard via `wl-copy`, then injects **Ctrl+V** through `/dev/uinput`.

### Package layout
- `internal/dictation/` — daemon, socket server, sidecar protocol, recorder, uinput keyboard, clipboard output, notifications, path/env config.
- `internal/setup/` — `setup` (extract sidecar + `uv sync`), `install-service`, `install-hotkey`, `uninstall`, `doctor`.
- `internal/assets/` — `go:embed` of the shared `transcribe.py` + a `pyproject.toml` per backend (`sidecar/` for whisper, `sidecar-qwen/` for qwen). `voicetype setup` extracts these and builds **two separate uv venvs** under `~/.local/share/voicetype/{sidecar,sidecar-qwen}`. The split is mandatory: faster-whisper needs CUDA-12 cuDNN wheels while torch (qwen) pulls CUDA-13 cuDNN, and the two cuDNN lineages in one venv cause `CUDNN_STATUS_SUBLIBRARY_VERSION_MISMATCH` at inference. The daemon runs the shared `transcribe.py` with the venv python selected by backend (`BackendDir` in `paths.go`).

## Critical constraints (Wayland)

The whole design works around two Wayland security restrictions; preserve these workarounds:

1. **No global hotkey capture** → register a GNOME custom shortcut via `gsettings` that runs `voicetype toggle`. We never grab keys ourselves.
2. **No key injection into other windows** → drive `/dev/uinput` directly in pure Go to inject Ctrl+V. We use **clipboard + paste** (not per-character key simulation) because keycodes can't type CJK. Do **not** re-add clipboard backup/restore — it races with the target app reading the clipboard and was deliberately removed.

## Other landmines (see ARCHITECTURE.md §7, §10)

- **uv must be resolved by absolute path** (`findUV` in `sidecar.go`): systemd `--user` PATH excludes `~/.local/bin`.
- **Sidecar is exec'd as the venv python directly**, not `uv run`, so the GPU-holding process is a direct child and dies with the daemon (Pdeathsig).
- **`install-service` bakes session env vars** (`WAYLAND_DISPLAY`, `DISPLAY`, `XDG_CURRENT_DESKTOP`) into the unit file — systemd `--user` lacks them, breaking `wl-copy`/`notify-send`. ASR/mic config deliberately lives in config.toml, NOT the unit, so reinstalling the service never clobbers it.
- **`source` (mic pin)** dodges Intel SOF laptops defaulting to an empty jack mic; threaded as `cfg.Source` into `startRecording`.
- The socket only appears **after** the model loads (several seconds); pressing the hotkey before then yields "daemon not running".
- Terminal paste is Ctrl+Shift+V, so injected Ctrl+V is a no-op there — the result stays on the clipboard as a fallback.

## Config

Lives in `~/.config/voicetype/config.toml` (flat `key = value`, parsed by a tiny zero-dependency TOML subset in `loadConfigFile`, `paths.go`). `setup` scaffolds a documented default if absent. A matching `VOICETYPE_*` env var overrides the file (one-off runs). Apply changes with `systemctl --user restart voicetype`.

| Key (env) | Default | Notes |
|---|---|---|
| `backend` (`VOICETYPE_BACKEND`) | `whisper` | `whisper` (faster-whisper) or `qwen` (Qwen3-ASR) — each has its own venv |
| `model` (`VOICETYPE_MODEL`) | per backend | **must match backend**: whisper `large-v3`/`large-v3-turbo`/`medium`/`small`; qwen `Qwen/Qwen3-ASR-0.6B`/`-1.7B` |
| `device` (`VOICETYPE_DEVICE`) | `cuda` | `cuda` or `cpu` |
| `compute` (`VOICETYPE_COMPUTE`) | `float16` | `float16`/`int8_float16`/`int8` (whisper only) |
| `lang` (`VOICETYPE_LANG`) | "" (auto) | force a language, e.g. `zh`, `en` |
| `source` (`VOICETYPE_SOURCE`) | "" (default) | pin a `pw-record --target` input device |

## Runtime dependencies

System package: `wl-clipboard`. Plus PipeWire (`pw-record`), libnotify (`notify-send`), `uv`, NVIDIA driver. `/dev/uinput` must be writable (udev rule sets `0666`). CUDA libs come from pip (`nvidia-cublas-cu12`/`nvidia-cudnn-cu12`), not system CUDA.
