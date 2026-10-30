package dictation

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// gnome-shell ignores the notify-osd "synchronous" hint, so a persistent
// "recording" banner used to stay up with later notices queued behind it. Now
// the only long-lived notification is that banner: notify remembers its ID and
// closeNotify dismisses it the moment recording stops. Every other notice is a
// fresh, self-expiring notification.
//
// Never pass -r (replace) an ID that may be gone: a transient notification that
// closed or expired takes its source with it, and replacing it trips a
// gnome-shell bug ("already disposed", then a JS error in messageTray) that
// stops all banners from showing until the shell restarts, i.e. a re-login.
var (
	notifyMu sync.Mutex
	bannerID string // the open persistent banner, "" if none
)

// notify shows a new notification. timeoutMs is the auto-dismiss time; 0
// makes it the persistent banner, kept until closeNotify. Transient (-e)
// keeps these status blips out of the notification list.
func notify(body string, timeoutMs int) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if timeoutMs == 0 {
		closeBannerLocked() // one persistent banner at a time
	}
	out, err := exec.Command("notify-send", "-a", "voicetype", "-e", "-p",
		"-t", strconv.Itoa(timeoutMs), "voicetype", body).Output()
	if err != nil {
		log.Printf("notify-send failed: %v", err)
		return
	}
	if timeoutMs == 0 {
		bannerID = strings.TrimSpace(string(out))
	}
}

// closeNotify dismisses the persistent banner right away, e.g. "recording"
// the moment recording stops.
func closeNotify() {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	closeBannerLocked()
}

func closeBannerLocked() {
	if bannerID == "" {
		return
	}
	err := exec.Command("gdbus", "call", "--session",
		"--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications",
		"--method", "org.freedesktop.Notifications.CloseNotification",
		"uint32 "+bannerID).Run()
	if err != nil {
		log.Printf("close notification: %v", err)
	}
	bannerID = ""
}
