package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// envForService are the session vars the daemon needs to reach the compositor
// (wl-copy / notify-send). systemd --user does not reliably inherit them, so we
// bake whatever is present at install time straight into the unit. ASR and mic
// settings deliberately live in config.toml instead, so reinstalling the
// service never clobbers them.
var envForService = []string{
	"WAYLAND_DISPLAY",
	"DISPLAY",
	"XDG_CURRENT_DESKTOP",
}

// Service writes the systemd --user unit pointing at this binary and enables it.
// Run this from a graphical-session terminal so WAYLAND_DISPLAY is captured.
func Service() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, _ = filepath.Abs(bin)

	var envLines []string
	for _, k := range envForService {
		if v := os.Getenv(k); v != "" {
			envLines = append(envLines, fmt.Sprintf("Environment=%s=%s", k, v))
		}
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		fmt.Println("⚠️  WAYLAND_DISPLAY 未设置——请在 GNOME 图形终端里运行本命令，")
		fmt.Println("    否则守护进程将无法访问剪贴板/通知。")
	}

	unit := fmt.Sprintf(`[Unit]
Description=voicetype voice dictation daemon
After=graphical-session.target

[Service]
Type=simple
ExecStart=%s daemon
%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, bin, strings.Join(envLines, "\n"))

	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, "voicetype.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", unitPath)

	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "enable", "--now", "voicetype.service"); err != nil {
		return err
	}
	fmt.Println("voicetype.service enabled and started")
	fmt.Println("  logs:    journalctl --user -u voicetype -f")
	fmt.Println("  restart: systemctl --user restart voicetype")
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
