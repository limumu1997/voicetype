package setup

import (
	"fmt"
	"os/exec"

	"voicetype/internal/dictation"
)

// OpenConfig opens the daemon's settings page in the default browser.
func OpenConfig() error {
	addr := dictation.LoadConfig().Web
	if addr == "" || addr == "off" {
		return fmt.Errorf("设置页已关闭 (config.toml 里 web = %q)", addr)
	}
	url := "http://" + addr + "/"
	fmt.Println(url)
	return exec.Command("xdg-open", url).Start()
}
