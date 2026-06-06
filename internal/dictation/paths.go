package dictation

import (
	"os"
	"path/filepath"
)

// DataDir is where the extracted sidecar and the uv environment live.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "voicetype")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "voicetype")
}

// SidecarDir holds the extracted transcribe.py + pyproject.toml + .venv.
func SidecarDir() string {
	return filepath.Join(DataDir(), "sidecar")
}

// SocketPath is the unix socket the daemon listens on and `toggle` connects to.
func SocketPath() string {
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "voicetype.sock")
	}
	return filepath.Join(os.TempDir(), "voicetype.sock")
}

// Config is the runtime configuration, all overridable via environment.
type Config struct {
	Model   string // VOICETYPE_MODEL
	Device  string // VOICETYPE_DEVICE
	Compute string // VOICETYPE_COMPUTE
	Lang    string // VOICETYPE_LANG ("" = auto)
}

func LoadConfig() Config {
	return Config{
		Model:   envOr("VOICETYPE_MODEL", "large-v3"),
		Device:  envOr("VOICETYPE_DEVICE", "cuda"),
		Compute: envOr("VOICETYPE_COMPUTE", "float16"),
		Lang:    os.Getenv("VOICETYPE_LANG"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
