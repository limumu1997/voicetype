package dictation

import (
	"log"
	"os/exec"
)

// notify shows a desktop notification, replacing the previous one from this app
// (via the synchronous hint) so recording/done states don't stack up.
func notify(title, body string) {
	cmd := exec.Command("notify-send",
		"-a", "voicetype",
		"-h", "string:x-canonical-private-synchronous:voicetype",
		title, body)
	if err := cmd.Run(); err != nil {
		log.Printf("notify-send failed: %v", err)
	}
}
