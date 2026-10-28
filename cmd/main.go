// voicetype: global push-to-toggle voice dictation for GNOME Wayland.
//
// A warm faster-whisper sidecar (kept in VRAM) transcribes mic audio; the text
// is pasted into the focused window via a uinput Ctrl+V. Bind `voicetype toggle`
// to a hotkey: press to start recording, press again to stop + type.
package main

import (
	"fmt"
	"os"

	"voicetype/internal/dictation"
	"voicetype/internal/setup"
)

const usage = `voicetype — 全局语音输入 (GNOME Wayland)

用法:
  voicetype setup [后端]     构建 Python 边车环境 (uv sync)
                             默认只建 config 里选的后端; whisper|qwen|funasr|all
  voicetype install-service  安装并启动 systemd --user 守护进程
  voicetype install-hotkey   [组合键]
                             绑定 GNOME 全局快捷键 (默认 <Alt>v)
  voicetype uninstall        停用并移除 systemd 服务与 GNOME 快捷键
  voicetype daemon           前台运行守护进程 (通常由 systemd 调用)
  voicetype toggle           开始/停止录音 (绑定到快捷键的就是它)
  voicetype doctor           检查依赖与运行状态
  voicetype config           在浏览器打开设置页 (守护进程需在运行)

首次安装顺序:
  1. sudo apt install wl-clipboard          # 唯一需要的系统包
  2. 配置 /dev/uinput 权限 (见 README)
  3. voicetype setup
  4. voicetype install-service
  5. voicetype install-hotkey <Alt>v
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "daemon":
		err = dictation.RunDaemon()
	case "toggle":
		err = dictation.Toggle()
	case "setup":
		err = setup.Sidecar(arg(2))
	case "install-service":
		err = setup.Service()
	case "install-hotkey":
		binding := arg(2)
		if binding == "" {
			binding = "<Alt>v"
		}
		err = setup.Hotkey(binding)
	case "uninstall":
		err = setup.Uninstall()
	case "doctor":
		setup.Doctor()
	case "config":
		err = setup.OpenConfig()
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Printf("unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// arg returns os.Args[i], or "" when absent.
func arg(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}
