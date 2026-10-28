package dictation

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// All voicetype notifications share one slot: each one replaces the previous
// by ID (-r). gnome-shell ignores the notify-osd "synchronous" hint we used to
// rely on, so without the ID a persistent "recording" banner stayed up and later
// ones queued behind it until well after the text had been pasted.
var (
	notifyMu sync.Mutex
	notifyID string // last notification's ID, "" if none yet
)

// notify shows (or updates in place) the voicetype notification. timeoutMs is
// the auto-dismiss time; 0 means it stays until replaced or closed. Transient
// (-e) keeps these status blips out of the notification list.
func notify(body string, timeoutMs int) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	args := []string{"-a", "voicetype", "-e", "-p", "-t", strconv.Itoa(timeoutMs)}
	if notifyID != "" {
		args = append(args, "-r", notifyID)
	}
	args = append(args, "voicetype", body)
	out, err := exec.Command("notify-send", args...).Output()
	if err != nil {
		log.Printf("notify-send failed: %v", err)
		return
	}
	notifyID = strings.TrimSpace(string(out))
}

// closeNotify dismisses the current notification right away, e.g. the
// "recording" banner the moment recording stops.
func closeNotify() {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if notifyID == "" {
		return
	}
	err := exec.Command("gdbus", "call", "--session",
		"--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications",
		"--method", "org.freedesktop.Notifications.CloseNotification",
		"uint32 "+notifyID).Run()
	if err != nil {
		log.Printf("close notification: %v", err)
	}
}
