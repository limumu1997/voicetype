package dictation

import (
	"context"
	_ "embed"
	"log"
	"os/exec"
	"strings"
	"time"
)

// Wayland gives us no way to ask the compositor what has keyboard focus, so we
// ask the accessibility bus (AT-SPI) instead. The query runs with the system
// python3 + PyGObject, which GNOME ships, keeping the Go side dependency-free.

//go:embed focus.py
var focusScript string

// FocusKind says where the pasted text would land.
type FocusKind string

const (
	FocusEditable FocusKind = "editable" // text field: Ctrl+V
	FocusTerminal FocusKind = "terminal" // VTE terminal: Ctrl+Shift+V
	FocusNone     FocusKind = "none"     // focused widget isn't an input: don't paste
	FocusUnknown  FocusKind = "unknown"  // no a11y info (Chrome/Electron, error): Ctrl+V as before
)

// DetectFocus classifies the currently focused widget. Any failure degrades to
// FocusUnknown, which keeps the old always-Ctrl+V behaviour.
func DetectFocus() FocusKind {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-I", "-c", focusScript).Output()
	if err != nil {
		log.Printf("focus detection: %v", err)
		return FocusUnknown
	}
	switch k := FocusKind(strings.TrimSpace(string(out))); k {
	case FocusEditable, FocusTerminal, FocusNone:
		return k
	}
	return FocusUnknown
}
