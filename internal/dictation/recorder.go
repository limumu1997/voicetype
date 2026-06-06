package dictation

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// recorder captures microphone audio to a temp WAV via PipeWire's pw-record.
// 16 kHz mono s16 is what Whisper wants, so we record straight into that.
type recorder struct {
	cmd  *exec.Cmd
	path string
}

func startRecording(source string) (*recorder, error) {
	f, err := os.CreateTemp("", "voicetype-*.wav")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	path := f.Name()
	f.Close()

	args := []string{"--rate", "16000", "--channels", "1", "--format", "s16"}
	// Pin a specific input if configured; otherwise PipeWire's default source.
	// Useful on Intel SOF laptops where the default may be an empty jack mic
	// while the real internal DMIC is a separate source.
	if source != "" {
		args = append(args, "--target", source)
	}
	args = append(args, path)
	cmd := exec.Command("pw-record", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("start pw-record (is pipewire running?): %w", err)
	}
	return &recorder{cmd: cmd, path: path}, nil
}

// stop signals pw-record to finalize the WAV, waits for it, and returns the path.
// The caller owns the file and must remove it when done.
func (r *recorder) stop() (string, error) {
	// SIGINT lets pw-record flush and write a valid WAV header.
	_ = r.cmd.Process.Signal(syscall.SIGINT)
	err := r.cmd.Wait()
	// pw-record exits non-zero on SIGINT; that's expected, not a failure.
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return "", fmt.Errorf("pw-record wait: %w", err)
		}
	}
	if fi, statErr := os.Stat(r.path); statErr != nil || fi.Size() == 0 {
		os.Remove(r.path)
		return "", fmt.Errorf("no audio captured")
	}
	return r.path, nil
}
