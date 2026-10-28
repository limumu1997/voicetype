package dictation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// typeText puts the transcription on the Wayland clipboard (wl-copy) and pastes
// it into the focused widget by injecting a paste shortcut through the uinput
// keyboard: Ctrl+Shift+V in terminals, Ctrl+V elsewhere. When the focus is known
// not to be an input (FocusNone) it skips the keystroke, which in e.g. a file
// manager would paste files; the text stays on the clipboard, and GPaste keeps
// it in history. It reports whether a paste was injected.
//
// We deliberately do NOT back up and restore the user's previous clipboard:
// restoring races with the app reading the pasted selection, which caused
// pastes to occasionally come out as the old content. The transcription simply
// stays on the clipboard afterwards, which also serves as a manual-paste
// fallback if the injected shortcut doesn't land in some app.
//
// Clipboard paste (not simulated typing) because keystroke injection can't
// produce CJK glyphs; the clipboard round-trip reproduces the text exactly.
func typeText(kbd *virtualKeyboard, text string, focus FocusKind) (bool, error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	if err := wlCopy(text); err != nil {
		return false, err
	}
	if focus == FocusNone {
		return false, nil
	}
	// Give the compositor a beat to register the new clipboard owner.
	time.Sleep(80 * time.Millisecond)
	return true, kbd.pasteShortcut(focus == FocusTerminal)
}

// wlCopy must not capture wl-copy's output through a pipe: wl-copy forks a
// child that keeps serving the selection and inherits stdout/stderr, so a pipe
// stays open until another client takes the clipboard over. Waiting on it
// delayed (or, without a clipboard manager, hung) the paste. Stderr goes to our
// own stderr (the journal) as a plain fd, which Wait doesn't block on.
func wlCopy(text string) error {
	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(text)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy (install wl-clipboard): %w", err)
	}
	return nil
}
