package setup

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"voicetype/internal/dictation"
)

// Doctor prints the status of every runtime dependency so problems are obvious.
func Doctor() {
	fmt.Println("voicetype doctor")
	fmt.Println("================")

	cfg := dictation.LoadConfig()

	checkCmd("pw-record", "录音 (PipeWire)")
	checkCmd("wl-copy", "剪贴板 (wl-clipboard)")
	checkCmd("notify-send", "桌面通知 (libnotify)")
	checkCmd("uv", "Python 环境 (uv)")

	checkUinput()
	checkGPU()
	checkConfig(cfg)
	checkSidecar(cfg)
	checkHotwords()
	checkFocus()
	checkDaemon()
}

func checkConfig(cfg dictation.Config) {
	lang := cfg.Lang
	if lang == "" {
		lang = "auto"
	}
	ok(fmt.Sprintf("config       backend=%s model=%s lang=%s", cfg.Backend, cfg.Model, lang))
	// A local model dir can't be checked by name; just make sure it exists.
	if strings.HasPrefix(cfg.Model, "/") {
		if _, err := os.Stat(cfg.Model); err != nil {
			bad("config       本地模型目录不存在: " + cfg.Model)
		}
		return
	}
	// Otherwise the single `model` key must match the backend by name.
	var mismatch bool
	switch cfg.Backend {
	case "qwen":
		mismatch = !strings.HasPrefix(cfg.Model, "Qwen/")
	case "funasr":
		mismatch = !strings.HasPrefix(cfg.Model, "FunAudioLLM/")
	default:
		mismatch = strings.Contains(cfg.Model, "/")
	}
	if mismatch {
		warn(fmt.Sprintf("config       model=%s 看起来不属于 backend=%s — 会加载失败", cfg.Model, cfg.Backend))
	}
}

func checkHotwords() {
	data, err := os.ReadFile(dictation.HotwordsPath())
	if err != nil {
		warn("hotwords     无热词表 (" + dictation.HotwordsPath() + ")")
		return
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if w, _, _ := strings.Cut(line, "#"); strings.TrimSpace(w) != "" {
			n++
		}
	}
	ok(fmt.Sprintf("hotwords     %d 个 (%s)", n, dictation.HotwordsPath()))
}

// checkFocus first checks that the system python3 can load AT-SPI, then runs
// the same probe the daemon uses before pasting. "unknown" with working libs
// just means the focus is somewhere opaque (Chrome/Electron, a non-VTE
// terminal), not that anything is broken.
func checkFocus() {
	err := exec.Command("python3", "-I", "-c",
		`import gi; gi.require_version("Atspi", "2.0"); from gi.repository import Atspi`).Run()
	if err != nil {
		warn("focus        系统 python3 载入不了 AT-SPI (缺 python3-gi / gir1.2-atspi-2.0) — 一律 Ctrl+V")
		return
	}
	k := dictation.DetectFocus()
	note := ""
	if k == dictation.FocusUnknown {
		note = "，焦点在 Chrome/Electron 等不暴露无障碍信息的程序里时就是这样"
	}
	ok(fmt.Sprintf("focus        AT-SPI 焦点识别可用 (当前: %s%s)", k, note))
}

func checkCmd(name, desc string) {
	if path, err := exec.LookPath(name); err == nil {
		ok(fmt.Sprintf("%-12s %s (%s)", name, desc, path))
	} else {
		bad(fmt.Sprintf("%-12s %s — 缺失", name, desc))
	}
}

func checkUinput() {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		bad("/dev/uinput  无法写入 — 需要 input 组 + udev 规则: " + err.Error())
		return
	}
	f.Close()
	ok("/dev/uinput  可写 (虚拟键盘可用)")
}

func checkGPU() {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total", "--format=csv,noheader").Output()
	if err != nil {
		warn("nvidia-smi  无 GPU 或驱动 — 将回退到 CPU 转写")
		return
	}
	ok("GPU          " + trim(string(out)))
}

func checkSidecar(cfg dictation.Config) {
	dir := dictation.BackendDir(cfg.Backend)
	if _, err := os.Stat(dir + "/.venv"); err != nil {
		bad(fmt.Sprintf("sidecar      %s 后端环境未安装 (%s) — 运行 `voicetype setup`", cfg.Backend, dir))
		return
	}
	ok(fmt.Sprintf("sidecar      %s @ %s", cfg.Backend, dir))
}

func checkDaemon() {
	conn, err := net.Dial("unix", dictation.SocketPath())
	if err != nil {
		warn("daemon       未运行 — `systemctl --user start voicetype` 或 `voicetype daemon`")
		return
	}
	conn.Close()
	ok("daemon       运行中 (" + dictation.SocketPath() + ")")
}

func ok(s string)   { fmt.Println("  ✅ " + s) }
func warn(s string) { fmt.Println("  ⚠️  " + s) }
func bad(s string)  { fmt.Println("  ❌ " + s) }

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
