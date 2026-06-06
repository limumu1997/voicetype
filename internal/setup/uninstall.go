package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Uninstall stops and removes the systemd --user service and the GNOME hotkey.
// It leaves the extracted sidecar env and the udev rule in place (those are
// cheap to keep and the udev rule needs sudo to remove).
func Uninstall() error {
	// systemd service
	_ = exec.Command("systemctl", "--user", "disable", "--now", "voicetype.service").Run()
	home, _ := os.UserHomeDir()
	unitPath := filepath.Join(home, ".config", "systemd", "user", "voicetype.service")
	if err := os.Remove(unitPath); err == nil {
		fmt.Println("removed", unitPath)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	// GNOME hotkey: drop our path from the list and clear its keys.
	if list, err := gsettingsGet(mediaKeysSchema, "custom-keybindings"); err == nil {
		var kept []string
		for _, p := range parseGVariantList(list) {
			if p != customPath {
				kept = append(kept, p)
			}
		}
		_ = gsettingsSet(mediaKeysSchema, "custom-keybindings", formatGVariantList(kept))
		_ = exec.Command("gsettings", "reset-recursively", customSchema+":"+customPath).Run()
		fmt.Println("removed GNOME hotkey")
	}

	fmt.Println("voicetype uninstalled (sidecar env kept at", "~/.local/share/voicetype/sidecar)")
	fmt.Println("如需彻底清理: rm -rf ~/.local/share/voicetype; sudo rm /etc/udev/rules.d/99-uinput.rules")
	return nil
}
