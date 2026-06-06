package dictation

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// DataDir is where the extracted sidecar and the uv environment live.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "voicetype")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "voicetype")
}

// SidecarDir holds the extracted transcribe.py + pyproject.toml + .venv for the
// faster-whisper backend. transcribe.py is shared by both backends.
func SidecarDir() string {
	return filepath.Join(DataDir(), "sidecar")
}

// QwenSidecarDir holds the Qwen3-ASR backend's own pyproject.toml + .venv. It
// reuses transcribe.py from SidecarDir; only the venv differs.
func QwenSidecarDir() string {
	return filepath.Join(DataDir(), "sidecar-qwen")
}

// BackendDir returns the uv project dir whose .venv should run the sidecar for
// the given backend.
func BackendDir(backend string) string {
	if backend == "qwen" {
		return QwenSidecarDir()
	}
	return SidecarDir()
}

// SocketPath is the unix socket the daemon listens on and `toggle` connects to.
func SocketPath() string {
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "voicetype.sock")
	}
	return filepath.Join(os.TempDir(), "voicetype.sock")
}

// ConfigDir holds the user's persistent config, deliberately separate from the
// systemd unit so reinstalling the service never clobbers it.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "voicetype")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "voicetype")
}

// ConfigPath is the TOML config the daemon reads at startup.
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.toml")
}

// Config is the runtime configuration. Values come from config.toml, and any
// matching VOICETYPE_* env var overrides the file (handy for one-off runs).
type Config struct {
	Backend string // backend / VOICETYPE_BACKEND ("whisper" | "qwen")
	Model   string // model / VOICETYPE_MODEL — must match Backend (whisper vs qwen names)
	Device  string // device / VOICETYPE_DEVICE
	Compute string // compute / VOICETYPE_COMPUTE
	Lang    string // lang / VOICETYPE_LANG ("" = auto)
	Source  string // source / VOICETYPE_SOURCE ("" = default mic)
}

func LoadConfig() Config {
	f := loadConfigFile()
	backend := pick("VOICETYPE_BACKEND", f["backend"], "whisper")
	model := pick("VOICETYPE_MODEL", f["model"], "")
	if model == "" {
		model = defaultModel(backend) // unset → backend-appropriate default
	}
	return Config{
		Backend: backend,
		Model:   model,
		Device:  pick("VOICETYPE_DEVICE", f["device"], "cuda"),
		Compute: pick("VOICETYPE_COMPUTE", f["compute"], "float16"),
		Lang:    pick("VOICETYPE_LANG", f["lang"], ""),
		Source:  pick("VOICETYPE_SOURCE", f["source"], ""),
	}
}

// defaultModel is the model used when none is configured, picked to match the
// backend so that setting only `backend` still works.
func defaultModel(backend string) string {
	if backend == "qwen" {
		return "Qwen/Qwen3-ASR-0.6B"
	}
	return "large-v3"
}

// pick resolves a setting: env var wins, then the config file, then the default.
func pick(envKey, fileVal, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if fileVal != "" {
		return fileVal
	}
	return def
}

// loadConfigFile parses the flat `key = value` lines of config.toml. It is a
// deliberately tiny TOML subset (no sections or arrays) so the tool keeps zero
// third-party dependencies; a missing file just yields an empty map (defaults).
func loadConfigFile() map[string]string {
	out := map[string]string{}
	f, err := os.Open(ConfigPath())
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			// Quoted: take the content up to the matching close quote and
			// ignore anything after it (e.g. a trailing inline comment).
			q := v[0]
			if end := strings.IndexByte(v[1:], q); end >= 0 {
				v = v[1 : 1+end]
			} else {
				v = strings.Trim(v, string(q)) // unterminated; best effort
			}
		} else if i := strings.Index(v, "#"); i >= 0 {
			// Unquoted: a '#' starts an inline comment.
			v = strings.TrimSpace(v[:i])
		}
		out[k] = v
	}
	return out
}
