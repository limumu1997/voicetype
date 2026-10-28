package dictation

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The config web UI: a single embedded page plus a small JSON API, served by
// the daemon on loopback. It edits config.toml (applied by restarting the
// service) and hotwords.txt (live), and can test-record a mic end to end.

//go:embed web/index.html
var webIndex []byte

func (d *Daemon) serveWeb(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndex)
	})
	mux.HandleFunc("GET /api/state", d.apiState)
	mux.HandleFunc("POST /api/config", d.apiConfig)
	mux.HandleFunc("POST /api/hotwords", d.apiHotwords)
	mux.HandleFunc("POST /api/restart", d.apiRestart)
	mux.HandleFunc("POST /api/test", d.apiTest)
	mux.HandleFunc("POST /api/sound", d.apiSound)
	log.Printf("config ui on http://%s", addr)
	return http.ListenAndServe(addr, localOnly(addr, mux))
}

// localOnly rejects requests whose Host isn't our loopback address (DNS
// rebinding) and POSTs that aren't JSON (a cross-site page can only send JSON
// after a CORS preflight, which we never answer), so other websites open in
// the browser can't rewrite the config.
func localOnly(addr string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	allowed := map[string]bool{addr: true, "localhost:" + port: true, "127.0.0.1:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "json only", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type audioSource struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

func (d *Daemon) apiState(w http.ResponseWriter, r *http.Request) {
	saved := LoadConfig().Map()
	running := d.cfg.Map()
	d.mu.Lock()
	running["sound"] = d.sound // applied live, never needs a restart
	d.mu.Unlock()
	restart := false
	for k, v := range saved {
		if k != "web" && running[k] != v {
			restart = true
		}
	}
	d.mu.Lock()
	loadErr := ""
	if d.loadErr != nil {
		loadErr = d.loadErr.Error()
	}
	d.mu.Unlock()
	hotwords, _ := os.ReadFile(HotwordsPath())
	writeJSON(w, map[string]any{
		"status":        d.status(),
		"error":         loadErr,
		"running":       running,
		"saved":         saved,
		"restartNeeded": restart,
		"hotwords":      string(hotwords),
		"sources":       listSources(),
		"gpu":           gpuMemory(),
		"systemd":       os.Getenv("INVOCATION_ID") != "",
		"configPath":    ConfigPath(),
		"hotwordsPath":  HotwordsPath(),
	})
}

func (d *Daemon) apiConfig(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	values := map[string]string{}
	for _, k := range ConfigKeys {
		if v, ok := in[k]; ok {
			values[k] = strings.TrimSpace(v)
		}
	}
	switch values["backend"] {
	case "whisper", "qwen", "funasr", "":
	default:
		http.Error(w, "backend 只能是 whisper / qwen / funasr", http.StatusBadRequest)
		return
	}
	if err := SaveConfig(values); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v, ok := values["sound"]; ok {
		d.mu.Lock()
		d.sound = v
		d.mu.Unlock()
	}
	log.Printf("config saved from web ui")
	d.apiState(w, r)
}

func (d *Daemon) apiHotwords(w http.ResponseWriter, r *http.Request) {
	var in struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text := strings.TrimRight(in.Text, "\n") + "\n"
	path := HotwordsPath()
	err := os.WriteFile(path+".tmp", []byte(text), 0o644)
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiRestart restarts the service through systemd so the new config is read.
// --no-block because systemctl runs inside our own cgroup and is killed along
// with us; the restart job itself lives in the systemd manager.
func (d *Daemon) apiRestart(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("INVOCATION_ID") == "" {
		http.Error(w, "守护进程不是由 systemd 启动的，请手动重启", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		if out, err := exec.Command("systemctl", "--user", "restart", "--no-block", "voicetype").CombinedOutput(); err != nil {
			log.Printf("restart: %v: %s", err, out)
		}
	}()
}

// apiSound previews a cue setting: its start cue, then its stop cue.
func (d *Daemon) apiSound(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go func() {
		playCue(in.Name, false, 700*time.Millisecond)
		playCue(in.Name, true, 0)
	}()
	writeJSON(w, map[string]any{"ok": true})
}

// apiTest records a few seconds from the given source and transcribes it, so a
// mic can be picked by level and checked end to end before saving.
func (d *Daemon) apiTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source  string
		Seconds int
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in.Seconds = min(max(in.Seconds, 1), 10)
	sc, why := d.engine()
	if sc == nil {
		http.Error(w, why, http.StatusConflict)
		return
	}
	if d.status() == "recording" {
		http.Error(w, "正在用热键录音，结束后再试", http.StatusConflict)
		return
	}
	rec, err := startRecording(in.Source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	time.Sleep(time.Duration(in.Seconds) * time.Second)
	wav, err := rec.stop()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(wav)
	rms, peak, err := wavLevel(wav)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	text, err := sc.transcribe(wav, d.cfg.Lang)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"rmsDb": rms, "peakDb": peak, "text": text})
}

// wavLevel returns the RMS and peak level in dBFS of a 16-bit PCM WAV.
func wavLevel(path string) (rmsDb, peakDb float64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	// Walk the RIFF chunks to "data"; pw-record may add chunks before it.
	i := 12
	for i+8 <= len(b) && string(b[i:i+4]) != "data" {
		i += 8 + int(binary.LittleEndian.Uint32(b[i+4:i+8]))
	}
	if i+8 > len(b) {
		return 0, 0, errors.New("wav: no data chunk")
	}
	pcm := b[i+8:]
	var sum, peak float64
	n := len(pcm) / 2
	for j := 0; j < n; j++ {
		v := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[2*j:]))))
		sum += v * v
		peak = max(peak, v)
	}
	db := func(x float64) float64 {
		if x < 1 {
			return -96
		}
		return math.Round(20*math.Log10(x/32768)*10) / 10
	}
	return db(math.Sqrt(sum / float64(max(n, 1)))), db(peak), nil
}

// listSources lists PipeWire capture sources (monitors excluded) via pactl.
func listSources() []audioSource {
	out, err := exec.Command("pactl", "-f", "json", "list", "sources").Output()
	if err != nil {
		return nil
	}
	var raw []struct{ Name, Description string }
	if json.Unmarshal(out, &raw) != nil {
		return nil
	}
	var srcs []audioSource
	for _, s := range raw {
		if !strings.HasSuffix(s.Name, ".monitor") {
			srcs = append(srcs, audioSource{s.Name, s.Description})
		}
	}
	return srcs
}

// gpuMemory reports used/total VRAM, or "" without an NVIDIA GPU.
func gpuMemory() string {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return ""
	}
	used, total, ok := strings.Cut(strings.TrimSpace(strings.Split(string(out), "\n")[0]), ",")
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s / %s MiB", strings.TrimSpace(used), strings.TrimSpace(total))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
