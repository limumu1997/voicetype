package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const serviceUnit = `[Unit]
Description=voicetype voice dictation daemon
After=graphical-session.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`

// Service writes the systemd --user unit pointing at this binary, imports the
// Wayland session env, and enables+starts it.
func Service() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, _ = filepath.Abs(bin)

	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, "voicetype.service")
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(serviceUnit, bin)), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", unitPath)

	// The daemon needs the compositor's display to talk to wl-copy/notify-send.
	run("systemctl", "--user", "import-environment",
		"WAYLAND_DISPLAY", "DISPLAY", "XDG_CURRENT_DESKTOP")
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
