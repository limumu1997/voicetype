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

// backends maps each backend to its embedded asset dir and what `uv sync`
// pulls for it.
var backends = []struct{ name, src, what string }{
	{"whisper", "sidecar", "faster-whisper + CUDA libs"},
	{"qwen", "sidecar-qwen", "qwen-asr + torch"},
	{"funasr", "sidecar-funasr", "funasr + torch"},
}

// Sidecar extracts the embedded sidecar files and runs `uv sync` to build the
// backend environment. which is a backend name, "all", or "" for the backend
// selected in config.toml; each venv is GBs of torch/CUDA, so only build what's
// used. Safe to re-run; it overwrites the scripts and reuses the venvs. Every
// backend gets its own uv environment because their CUDA stacks can't share
// one venv (see assets.go).
func Sidecar(which string) error {
	if which == "" {
		which = dictation.LoadConfig().Backend
	}
	built := 0
	for _, b := range backends {
		if which != "all" && which != b.name {
			continue
		}
		if err := buildBackend(b.src, dictation.BackendDir(b.name), b.what); err != nil {
			return err
		}
		built++
	}
	if built == 0 {
		return fmt.Errorf("unknown backend %q (whisper | qwen | funasr | all)", which)
	}
	// transcribe.py lives in the whisper dir; make sure it exists even when
	// only another backend was built.
	if err := dictation.SyncScript(); err != nil {
		return err
	}
	if err := writeConfigTemplate(); err != nil {
		return err
	}
	if err := writeHotwordsTemplate(); err != nil {
		return err
	}
	if err := writeReplaceTemplate(); err != nil {
		return err
	}
	fmt.Println("Note: the model weights themselves download on first start (unless model is a local dir).")
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

backend = "whisper"  # whisper | qwen | funasr  (换后端先跑 voicetype setup <后端>)
# model 必须与 backend 匹配，也可以写本地模型目录的绝对路径 (不展开 ~):
#   whisper -> large-v3 / large-v3-turbo / medium / small
#   qwen    -> Qwen/Qwen3-ASR-0.6B / Qwen/Qwen3-ASR-1.7B
#   funasr  -> FunAudioLLM/Fun-ASR-Nano-2512
model = "large-v3"
device = "cuda"      # cuda | cpu
compute = "float16"  # float16 | int8_float16 | int8 (仅 whisper)
lang = ""            # "" = 自动; zh / en ...
vad = "fsmn-vad"     # 长录音按停顿切段 (仅 funasr); 可填本地目录; "off" 关闭
source = ""          # 录音输入设备名; "" = 系统默认 (pw-record --target)
sound = "crystal"    # 提示音: warp | nebula | sonar (低沉) | crystal | pulse | halo (清亮) | off
web = "127.0.0.1:27865"  # 设置页地址 (voicetype config 打开); "off" 关闭
`
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote default config", path)
	return nil
}

// writeHotwordsTemplate drops a starter hotwords.txt if none exists; like the
// config, an existing list is never overwritten.
func writeHotwordsTemplate() error {
	path := dictation.HotwordsPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	const tmpl = `# voicetype 热词表 — 每行一个词，# 开头为注释
# 放通用模型容易认错、而你常说的专有名词：项目名、人名、地名、技术术语。
# 改完立即生效（下次转写时自动重读），无需重启。宁缺毋滥，几十个以内为宜。
`
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote hotwords template", path)
	return nil
}

// writeReplaceTemplate drops a commented replace.txt if none exists; rules
// are opt-in, so the template only shows the syntax.
func writeReplaceTemplate() error {
	path := dictation.ReplacePath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	const tmpl = `# voicetype 替换规则 — 识别出来后按规则改字，治模型老认错的同音字
# 每行一条: 原文 => 替换    # 开头为注释；保存后下次转写生效
# 长的规则优先，所以"原样保留"的规则能保护词组。例：主要跟电脑/AI 说话时把"他"写成"它"
#   他 => 它
#   其他 => 其他
#   吉他 => 吉他
`
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote replace rules template", path)
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
