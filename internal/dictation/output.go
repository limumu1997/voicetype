package dictation

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// typeText puts text on the Wayland clipboard (wl-copy) and pastes it into the
// focused window by injecting Ctrl+V through the uinput virtual keyboard.
//
// Clipboard paste (not simulated typing) because keystroke injection can't
// produce CJK glyphs; the clipboard round-trip reproduces the text exactly.
func typeText(kbd *virtualKeyboard, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := wlCopy(text); err != nil {
		return err
	}
	// Give the compositor a beat to register the new clipboard owner.
	time.Sleep(80 * time.Millisecond)
	return kbd.pasteShortcut()
}

func wlCopy(text string) error {
	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wl-copy (install wl-clipboard): %v: %s", err, out)
	}
	return nil
}
