package dictation

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"
)

// GPaste records every clipboard change, so each dictation would pile up in
// its history even though the clipboard was only a vehicle for the paste.
// Once a paste has landed, forgetPasted deletes that entry again; text that
// wasn't pasted (no input focused, unknown focus) stays as the fallback.
//
// Deleting the current entry makes GPaste put the previous one back on the
// clipboard, i.e. it restores the user's old clipboard. Doing that too early
// races with the target app reading the paste (why clipboard restore was once
// removed), hence the delay: apps read the selection when they get the key,
// within milliseconds, far inside forgetDelay.
const forgetDelay = 1500 * time.Millisecond

// forgetPasted removes text from GPaste's history; a missing or unresponsive
// GPaste just means nothing happens.
func forgetPasted(text string) {
	time.Sleep(forgetDelay)
	gp := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "gpaste-client", args...).Output()
		return string(out), err
	}
	hist, err := gp("history", "--oneline")
	if err != nil {
		return
	}
	// Our entry is at or near the top; check only the newest few.
	lines := strings.SplitN(hist, "\n", 6)
	for _, line := range lines[:min(len(lines), 5)] {
		uuid, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		got, err := gp("get", uuid)
		if err != nil || strings.TrimSuffix(got, "\n") != text {
			continue
		}
		if _, err := gp("delete", uuid); err != nil {
			log.Printf("gpaste delete: %v", err)
		}
		return
	}
}
