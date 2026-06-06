// Package assets embeds the Python sidecar so the whole tool ships as one binary.
package assets

import "embed"

// Sidecar holds the shared transcribe.py plus a pyproject.toml per backend:
// sidecar/ for faster-whisper and sidecar-qwen/ for Qwen3-ASR. The two backends
// get separate uv environments because their CUDA stacks (cu12 vs cu13 cuDNN)
// can't coexist in one venv.
//
//go:embed sidecar/transcribe.py sidecar/pyproject.toml sidecar-qwen/pyproject.toml
var Sidecar embed.FS
