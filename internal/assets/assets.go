// Package assets embeds the Python sidecar so the whole tool ships as one binary.
package assets

import "embed"

// Sidecar holds the shared transcribe.py plus a pyproject.toml per backend:
// sidecar/ for faster-whisper, sidecar-qwen/ for Qwen3-ASR and sidecar-funasr/
// for Fun-ASR-Nano. Each backend gets its own uv environment because their
// CUDA stacks (cu12 vs cu13 cuDNN) and pinned transformers versions can't
// coexist in one venv.
//
//go:embed sidecar/transcribe.py sidecar/pyproject.toml sidecar-qwen/pyproject.toml sidecar-funasr/pyproject.toml
var Sidecar embed.FS
