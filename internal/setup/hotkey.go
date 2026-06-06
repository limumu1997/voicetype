package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	mediaKeysSchema = "org.gnome.settings-daemon.plugins.media-keys"
	customSchema    = "org.gnome.settings-daemon.plugins.media-keys.custom-keybinding"
	customPath      = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voicetype/"
)

// Hotkey registers a GNOME custom keybinding that runs `voicetype toggle`.
// It appends to the existing custom-keybindings list without clobbering others.
func Hotkey(binding string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, _ = filepath.Abs(bin)

	list, err := gsettingsGet(mediaKeysSchema, "custom-keybindings")
	if err != nil {
		return fmt.Errorf("read custom-keybindings (GNOME only): %w", err)
	}
	paths := parseGVariantList(list)
	if !contains(paths, customPath) {
		paths = append(paths, customPath)
	}
	if err := gsettingsSet(mediaKeysSchema, "custom-keybindings", formatGVariantList(paths)); err != nil {
		return err
	}

	target := fmt.Sprintf("%s:%s", customSchema, customPath)
	if err := gsettingsSet(target, "name", "voicetype toggle"); err != nil {
		return err
	}
	if err := gsettingsSet(target, "command", bin+" toggle"); err != nil {
		return err
	}
	if err := gsettingsSet(target, "binding", binding); err != nil {
		return err
	}
	fmt.Printf("hotkey %s -> %s toggle\n", binding, bin)
	return nil
}

func gsettingsGet(schema, key string) (string, error) {
	out, err := exec.Command("gsettings", "get", schema, key).Output()
	return strings.TrimSpace(string(out)), err
}

func gsettingsSet(schema, key, value string) error {
	cmd := exec.Command("gsettings", "set", schema, key, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// parseGVariantList turns "['a', 'b']" or "@as []" into []string.
func parseGVariantList(s string) []string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "["); i >= 0 {
		s = s[i:]
	}
	s = strings.Trim(s, "[]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "'\"")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formatGVariantList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = "'" + it + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
