package dictation

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// typeText puts the transcription on the Wayland clipboard (wl-copy) and pastes
// it into the focused window by injecting Ctrl+V through the uinput keyboard.
//
// We deliberately do NOT back up and restore the user's previous clipboard:
// restoring races with the app reading the pasted selection, which caused
// pastes to occasionally come out as the old content. The transcription simply
// stays on the clipboard afterwards, which also serves as a manual-paste
// fallback if the injected Ctrl+V doesn't land in some app.
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
