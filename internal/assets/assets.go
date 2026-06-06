// Package assets embeds the Python sidecar so the whole tool ships as one binary.
package assets

import "embed"

// Sidecar holds transcribe.py and pyproject.toml under sidecar/.
//
//go:embed sidecar/transcribe.py sidecar/pyproject.toml
var Sidecar embed.FS
