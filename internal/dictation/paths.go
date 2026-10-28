package dictation

import (
	"bufio"
	"fmt"
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

// FunASRSidecarDir holds the Fun-ASR-Nano backend's own pyproject.toml + .venv.
// Like the qwen dir, it reuses transcribe.py from SidecarDir.
func FunASRSidecarDir() string {
	return filepath.Join(DataDir(), "sidecar-funasr")
}

// BackendDir returns the uv project dir whose .venv should run the sidecar for
// the given backend.
func BackendDir(backend string) string {
	switch backend {
	case "qwen":
		return QwenSidecarDir()
	case "funasr":
		return FunASRSidecarDir()
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

// HotwordsPath is the user's hotword list (one term per line). The sidecar
// re-reads it whenever it changes, so edits apply without a restart.
func HotwordsPath() string {
	return filepath.Join(ConfigDir(), "hotwords.txt")
}

// Config is the runtime configuration. Values come from config.toml, and any
// matching VOICETYPE_* env var overrides the file (handy for one-off runs).
type Config struct {
	Backend string // backend / VOICETYPE_BACKEND ("whisper" | "qwen" | "funasr")
	Model   string // model / VOICETYPE_MODEL — name or local dir, must match Backend
	Device  string // device / VOICETYPE_DEVICE
	Compute string // compute / VOICETYPE_COMPUTE
	Lang    string // lang / VOICETYPE_LANG ("" = auto)
	Source  string // source / VOICETYPE_SOURCE ("" = default mic)
	Web     string // web / VOICETYPE_WEB — config UI listen addr ("off" disables)
	Sound   string // sound / VOICETYPE_SOUND — start/stop cue preset, or "off"
	VAD     string // vad / VOICETYPE_VAD — funasr VAD segmenter (name or local dir), "off" disables
}

// ConfigKeys are the config.toml keys, in the order the template lists them.
var ConfigKeys = []string{"backend", "model", "device", "compute", "lang", "vad", "source", "sound", "web"}

// Map returns the config as config.toml key/value pairs.
func (c Config) Map() map[string]string {
	return map[string]string{
		"backend": c.Backend, "model": c.Model, "device": c.Device,
		"compute": c.Compute, "lang": c.Lang, "source": c.Source, "sound": c.Sound, "web": c.Web, "vad": c.VAD,
	}
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
		Web:     pick("VOICETYPE_WEB", f["web"], "127.0.0.1:27865"),
		Sound:   pick("VOICETYPE_SOUND", f["sound"], "on"),
		VAD:     pick("VOICETYPE_VAD", f["vad"], "fsmn-vad"),
	}
}

// defaultModel is the model used when none is configured, picked to match the
// backend so that setting only `backend` still works.
func defaultModel(backend string) string {
	switch backend {
	case "qwen":
		return "Qwen/Qwen3-ASR-0.6B"
	case "funasr":
		return "FunAudioLLM/Fun-ASR-Nano-2512"
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
		out[strings.TrimSpace(k)], _ = splitValue(v)
	}
	return out
}

// splitValue parses the right-hand side of `key = value  # comment` into the
// value and the trailing comment (with its leading whitespace, or "").
func splitValue(rhs string) (val, comment string) {
	v := strings.TrimSpace(rhs)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		// Quoted: take the content up to the matching close quote; anything
		// after it (e.g. an inline comment) is the comment.
		q := v[0]
		end := strings.IndexByte(v[1:], q)
		if end < 0 {
			return strings.Trim(v, string(q)), "" // unterminated; best effort
		}
		val, rest := v[1:1+end], v[2+end:]
		if i := strings.Index(rest, "#"); i >= 0 {
			comment = "  " + rest[i:]
		}
		return val, comment
	}
	if i := strings.Index(v, "#"); i >= 0 {
		// Unquoted: a '#' starts an inline comment.
		return strings.TrimSpace(v[:i]), "  " + v[i:]
	}
	return v, ""
}

// SaveConfig writes values into config.toml in place: each `key = ...` line
// gets the new value with its inline comment kept, so the documented template
// survives edits from the web UI. Keys missing from the file are appended.
func SaveConfig(values map[string]string) error {
	for k, v := range values {
		// The tiny parser has no escapes, so these couldn't be read back.
		if strings.ContainsAny(v, "\"\n") {
			return fmt.Errorf("%s 不能包含双引号或换行", k)
		}
	}
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	done := map[string]bool{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, rhs, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(k)
		v, has := values[k]
		if !ok || !has {
			continue
		}
		_, comment := splitValue(rhs)
		lines[i] = fmt.Sprintf("%s = %q%s", k, v, comment)
		done[k] = true
	}
	for _, k := range ConfigKeys {
		if v, has := values[k]; has && !done[k] {
			lines = append(lines, fmt.Sprintf("%s = %q", k, v))
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
