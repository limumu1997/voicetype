package dictation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"voicetype/internal/assets"
)

// transcribeTimeout bounds a single request. A hung sidecar (GPU wedge, stuck
// model download) is killed on expiry; the daemon then exits and systemd
// restarts both, instead of leaving the hotkey silently dead.
const transcribeTimeout = 120 * time.Second

// sidecar drives the long-running faster-whisper Python process. The model
// stays loaded in VRAM across requests; we talk to it over line-delimited JSON.
type sidecar struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu sync.Mutex // serialize transcription requests

	done      chan struct{} // closed when the process exits
	exitState string        // how it exited; valid after done is closed
}

type sidecarReq struct {
	Wav      string `json:"wav"`
	Language string `json:"language,omitempty"`
}

type sidecarResp struct {
	Ready bool   `json:"ready"`
	Text  string `json:"text"`
	Error string `json:"error"`
}

// startSidecar launches the backend's venv python on transcribe.py and blocks
// until the model reports ready.
func startSidecar(cfg Config) (*sidecar, error) {
	// transcribe.py is shared; only the venv differs per backend (see assets.go).
	dir := BackendDir(cfg.Backend)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("%s backend not installed at %s (run `voicetype setup %s`): %w", cfg.Backend, dir, cfg.Backend, err)
	}
	if err := SyncScript(); err != nil {
		return nil, fmt.Errorf("update transcribe.py: %w", err)
	}
	script := filepath.Join(SidecarDir(), "transcribe.py")

	// Launch the venv python directly (not via `uv run`) so the model-holding
	// process is a direct child: Pdeathsig then guarantees it dies with the
	// daemon and never orphans, which would otherwise pin GPU memory.
	var cmd *exec.Cmd
	if py := filepath.Join(dir, ".venv", "bin", "python"); fileExists(py) {
		cmd = exec.Command(py, script)
	} else {
		cmd = exec.Command(findUV(), "run", "--project", dir, "python", script)
	}
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Env = append(os.Environ(),
		"VOICETYPE_BACKEND="+cfg.Backend,
		"VOICETYPE_MODEL="+cfg.Model,
		"VOICETYPE_DEVICE="+cfg.Device,
		"VOICETYPE_COMPUTE="+cfg.Compute,
		"VOICETYPE_LANG="+cfg.Lang,
		"VOICETYPE_HOTWORDS="+HotwordsPath(),
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // sidecar logs go to the daemon's stderr/journal

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sidecar: %w", err)
	}

	sc := &sidecar{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		done:   make(chan struct{}),
	}
	sc.stdout.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// Process.Wait (not Cmd.Wait) so the stdout pipe isn't closed under the
	// scanner; a dead sidecar still surfaces as EOF there.
	go func() {
		if st, err := cmd.Process.Wait(); err != nil {
			sc.exitState = err.Error()
		} else {
			sc.exitState = st.String()
		}
		close(sc.done)
	}()

	// Wait for the readiness line (model load can take several seconds).
	for sc.stdout.Scan() {
		var r sidecarResp
		if err := json.Unmarshal(sc.stdout.Bytes(), &r); err != nil {
			continue // ignore stray non-JSON lines
		}
		if r.Ready {
			return sc, nil
		}
	}
	return nil, fmt.Errorf("sidecar exited before ready")
}

// transcribe sends a WAV path and returns the recognized text.
func (s *sidecar) transcribe(wav, lang string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timer := time.AfterFunc(transcribeTimeout, func() {
		log.Printf("sidecar unresponsive for %s, killing it", transcribeTimeout)
		s.cmd.Process.Kill()
	})
	defer timer.Stop()

	req, _ := json.Marshal(sidecarReq{Wav: wav, Language: lang})
	if _, err := fmt.Fprintf(s.stdin, "%s\n", req); err != nil {
		return "", fmt.Errorf("write to sidecar: %w", err)
	}
	if !s.stdout.Scan() {
		return "", fmt.Errorf("sidecar closed: %v", s.stdout.Err())
	}
	var r sidecarResp
	if err := json.Unmarshal(s.stdout.Bytes(), &r); err != nil {
		return "", fmt.Errorf("bad sidecar response: %w", err)
	}
	if r.Error != "" {
		return "", fmt.Errorf("transcribe: %s", r.Error)
	}
	return r.Text, nil
}

// findUV resolves the uv binary by absolute path so the daemon works under
// systemd --user, whose PATH excludes ~/.local/bin where uv is usually installed.
func findUV() string {
	if p, err := exec.LookPath("uv"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	if cand := filepath.Join(home, ".local", "bin", "uv"); fileExists(cand) {
		return cand
	}
	return "uv"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// close asks the sidecar to exit by closing its stdin, killing it if it doesn't
// go within a few seconds.
func (s *sidecar) close() {
	s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		s.cmd.Process.Kill()
		<-s.done
	}
}

// SyncScript writes the transcribe.py embedded in this binary into the sidecar
// dir when the copy on disk differs, so upgrading the binary can never leave a
// stale sidecar script behind (which once silently ignored the model setting).
func SyncScript() error {
	want, err := assets.Sidecar.ReadFile("sidecar/transcribe.py")
	if err != nil {
		return err
	}
	path := filepath.Join(SidecarDir(), "transcribe.py")
	if have, _ := os.ReadFile(path); bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(SidecarDir(), 0o755); err != nil {
		return err
	}
	log.Printf("updating %s from the embedded copy", path)
	return os.WriteFile(path, want, 0o644)
}
