package setup

import (
	"fmt"
	"net"
	"os"
	"os/exec"

	"voicetype/internal/dictation"
)

// Doctor prints the status of every runtime dependency so problems are obvious.
func Doctor() {
	fmt.Println("voicetype doctor")
	fmt.Println("================")

	checkCmd("pw-record", "录音 (PipeWire)")
	checkCmd("wl-copy", "剪贴板 (wl-clipboard)")
	checkCmd("notify-send", "桌面通知 (libnotify)")
	checkCmd("uv", "Python 环境 (uv)")

	checkUinput()
	checkGPU()
	checkSidecar()
	checkDaemon()
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

func checkSidecar() {
	dir := dictation.SidecarDir()
	if _, err := os.Stat(dir + "/.venv"); err != nil {
		bad("sidecar      未安装环境 — 运行 `voicetype setup`")
		return
	}
	ok("sidecar      " + dir)
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
