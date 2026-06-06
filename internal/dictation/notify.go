package dictation

import (
	"log"
	"os/exec"
	"strconv"
)

// notify shows a desktop notification, replacing the previous one from this app
// (via the synchronous hint) so states update in place instead of stacking.
// timeoutMs is the auto-dismiss time; 0 means it stays until replaced.
func notify(body string, timeoutMs int) {
	cmd := exec.Command("notify-send",
		"-a", "voicetype",
		"-t", strconv.Itoa(timeoutMs),
		"-h", "string:x-canonical-private-synchronous:voicetype",
		"voicetype", body)
	if err := cmd.Run(); err != nil {
		log.Printf("notify-send failed: %v", err)
	}
}
