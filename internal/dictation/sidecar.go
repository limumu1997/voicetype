package dictation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

// sidecar drives the long-running faster-whisper Python process. The model
// stays loaded in VRAM across requests; we talk to it over line-delimited JSON.
type sidecar struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu sync.Mutex // serialize transcription requests
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

// startSidecar launches `uv run` in the extracted sidecar dir and blocks until
// the model reports ready.
func startSidecar(cfg Config) (*sidecar, error) {
	dir := SidecarDir()
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("sidecar not installed at %s (run `voicetype setup`): %w", dir, err)
	}

	// Launch the venv python directly (not via `uv run`) so the model-holding
	// process is a direct child: Pdeathsig then guarantees it dies with the
	// daemon and never orphans, which would otherwise pin GPU memory.
	script := filepath.Join(dir, "transcribe.py")
	var cmd *exec.Cmd
	if py := filepath.Join(dir, ".venv", "bin", "python"); fileExists(py) {
		cmd = exec.Command(py, script)
	} else {
		cmd = exec.Command(findUV(), "run", "--project", dir, "python", script)
	}
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Env = append(os.Environ(),
		"VOICETYPE_MODEL="+cfg.Model,
		"VOICETYPE_DEVICE="+cfg.Device,
		"VOICETYPE_COMPUTE="+cfg.Compute,
		"VOICETYPE_LANG="+cfg.Lang,
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
	}
	sc.stdout.Buffer(make([]byte, 0, 64*1024), 1<<20)

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

func (s *sidecar) close() {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		if err := s.cmd.Wait(); err != nil {
			log.Printf("sidecar exited: %v", err)
		}
	}
}
