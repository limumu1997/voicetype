// Package setup handles first-run installation: extracting the embedded Python
// sidecar, building its uv environment, and wiring up the systemd service and
// GNOME hotkey.
package setup

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"voicetype/internal/assets"
	"voicetype/internal/dictation"
)

// Sidecar extracts the embedded sidecar files and runs `uv sync` to build its
// environment. Safe to re-run; it overwrites the scripts and reuses the venv.
func Sidecar() error {
	dir := dictation.SidecarDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	entries, err := fs.ReadDir(assets.Sidecar, "sidecar")
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := assets.Sidecar.ReadFile(filepath.Join("sidecar", e.Name()))
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, e.Name())
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %s\n", dst)
	}

	fmt.Println("Running uv sync (downloads faster-whisper + CUDA libs, first time is slow)...")
	cmd := exec.Command("uv", "sync", "--project", dir)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uv sync: %w", err)
	}
	fmt.Println("Sidecar environment ready at", dir)
	fmt.Println("Note: the Whisper model itself downloads on first transcription.")
	return nil
}
