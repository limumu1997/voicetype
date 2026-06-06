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

// Sidecar extracts the embedded sidecar files and runs `uv sync` to build the
// per-backend environments. Safe to re-run; it overwrites the scripts and
// reuses the venvs. The faster-whisper and Qwen3-ASR backends get separate uv
// environments because their CUDA stacks can't share one venv (see assets.go).
func Sidecar() error {
	if err := buildBackend("sidecar", dictation.SidecarDir(), "faster-whisper + CUDA libs"); err != nil {
		return err
	}
	if err := buildBackend("sidecar-qwen", dictation.QwenSidecarDir(), "qwen-asr + torch"); err != nil {
		return err
	}
	if err := writeConfigTemplate(); err != nil {
		return err
	}
	fmt.Println("Note: the model weights themselves download on first transcription.")
	return nil
}

// writeConfigTemplate drops a documented default config.toml if none exists. It
// never overwrites an existing file, so user settings survive re-running setup.
func writeConfigTemplate() error {
	path := dictation.ConfigPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	const tmpl = `# voicetype 配置 — ASR 与录音设置
# 改完执行: systemctl --user restart voicetype
# 同名环境变量 (VOICETYPE_*) 若设置会覆盖此处。

backend = "whisper"  # whisper | qwen
# model 必须与 backend 匹配:
#   whisper -> large-v3 / large-v3-turbo / medium / small
#   qwen    -> Qwen/Qwen3-ASR-0.6B / Qwen/Qwen3-ASR-1.7B
model = "large-v3"
device = "cuda"      # cuda | cpu
compute = "float16"  # float16 | int8_float16 | int8 (仅 whisper)
lang = ""            # "" = 自动; zh / en ...
source = ""          # 录音输入设备名; "" = 系统默认 (pw-record --target)
`
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote default config", path)
	return nil
}

// buildBackend extracts the embedded files under src/ into dir and runs uv sync.
func buildBackend(src, dir, what string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	entries, err := fs.ReadDir(assets.Sidecar, src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := assets.Sidecar.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, e.Name())
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %s\n", dst)
	}

	fmt.Printf("Running uv sync for %s (downloads %s, first time is slow)...\n", src, what)
	cmd := exec.Command("uv", "sync", "--project", dir)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uv sync %s: %w", src, err)
	}
	fmt.Println("Environment ready at", dir)
	return nil
}
