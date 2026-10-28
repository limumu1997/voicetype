package dictation

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Replacement rules fix what the model systematically gets wrong for this
// user, typically homophones: ASR must pick one of 他/她/它 for "tā" and
// defaults to 他, while dictation aimed at computers almost always means 它.
//
// ~/.config/voicetype/replace.txt holds one `from => to` rule per line (#
// starts a comment). Matching is longest-first, left to right, so an identity
// rule shields a word from a shorter one: with `他 => 它` plus `其他 => 其他`,
// "其他" stays while every other 他 becomes 它. The file is re-read when it
// changes, so edits from the web UI apply to the next transcription.

type rule struct{ from, to []rune }

var rules struct {
	sync.Mutex
	mtime time.Time
	list  []rule // longest `from` first
}

// loadRules returns the current rules, re-parsing the file when it changed.
// A missing file means no rules.
func loadRules() []rule {
	rules.Lock()
	defer rules.Unlock()
	fi, err := os.Stat(ReplacePath())
	if err != nil {
		rules.list, rules.mtime = nil, time.Time{}
		return nil
	}
	if fi.ModTime().Equal(rules.mtime) {
		return rules.list
	}
	data, err := os.ReadFile(ReplacePath())
	if err != nil {
		return rules.list
	}
	rules.list, rules.mtime = parseRules(string(data)), fi.ModTime()
	return rules.list
}

func parseRules(text string) []rule {
	var list []rule
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		from, to, ok := strings.Cut(line, "=>")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" {
			continue
		}
		list = append(list, rule{[]rune(from), []rune(to)})
	}
	sort.SliceStable(list, func(i, j int) bool { return len(list[i].from) > len(list[j].from) })
	return list
}

// applyRules rewrites text with the longest matching rule at each position.
func applyRules(text string, list []rule) string {
	if len(list) == 0 {
		return text
	}
	rs := []rune(text)
	var out []rune
	for i := 0; i < len(rs); {
		matched := false
		for _, r := range list {
			if hasPrefixAt(rs, i, r.from) {
				out = append(out, r.to...)
				i += len(r.from)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, rs[i])
			i++
		}
	}
	return string(out)
}

func hasPrefixAt(rs []rune, i int, p []rune) bool {
	if i+len(p) > len(rs) {
		return false
	}
	for k, r := range p {
		if rs[i+k] != r {
			return false
		}
	}
	return true
}
