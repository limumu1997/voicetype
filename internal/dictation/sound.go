package dictation

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Start/stop cues are synthesized here rather than taken from the system sound
// theme, so they can sound airy and "techy" without shipping audio files: each
// cue is a few enveloped sine notes with bell-like partials, an optional pitch
// glide and short echoes for a sense of space. Start cues rise, stop cues fall.

const sampleRate = 48000

// SoundNames are the selectable `sound` values besides "off"; "on" means the
// first one.
var SoundNames = []string{"crystal", "pulse", "halo", "warp", "nebula", "sonar"}

type note struct{ at, freq, gain float64 } // start (s), pitch (Hz), level

type cue struct {
	notes    []note
	partials [][2]float64 // overtone ratio, relative gain (1st must be the fundamental)
	attack   float64      // linear fade-in (s)
	decay    float64      // exponential decay time constant (s)
	glide    float64      // pitch ratio reached after glideT (1 = steady)
	glideT   float64
	echoes   [][2]float64 // delay (s), gain
	fmRatio  float64      // FM modulator/carrier ratio (0 = no FM)
	fmIndex  float64      // FM depth at onset; decays with the envelope
}

var cues = map[string][2]cue{
	// Two glassy bell notes a fifth apart; the 2.76 partial gives the glass.
	"crystal": {
		{notes: []note{{0, 880, 1}, {0.07, 1318.5, 0.9}},
			partials: [][2]float64{{1, 1}, {2, 0.25}, {2.76, 0.18}, {5.4, 0.05}},
			attack:   0.004, decay: 0.12, glide: 1, echoes: [][2]float64{{0.09, 0.35}, {0.19, 0.15}}},
		{notes: []note{{0, 1318.5, 0.9}, {0.07, 880, 1}},
			partials: [][2]float64{{1, 1}, {2, 0.25}, {2.76, 0.18}, {5.4, 0.05}},
			attack:   0.004, decay: 0.12, glide: 1, echoes: [][2]float64{{0.09, 0.35}, {0.19, 0.15}}},
	},
	// A quick sine sweep plus a high blip: the most "electronic" of the three.
	"pulse": {
		{notes: []note{{0, 620, 1}, {0.13, 2480, 0.35}},
			partials: [][2]float64{{1, 1}, {3, 0.12}},
			attack:   0.006, decay: 0.09, glide: 2.2, glideT: 0.12, echoes: [][2]float64{{0.07, 0.3}}},
		{notes: []note{{0, 1400, 1}, {0.13, 1240, 0.3}},
			partials: [][2]float64{{1, 1}, {3, 0.12}},
			attack:   0.006, decay: 0.09, glide: 0.45, glideT: 0.12, echoes: [][2]float64{{0.07, 0.3}}},
	},
	// The low presets below sit at 100-200 Hz but lean on strong overtones:
	// laptop speakers can't reproduce the fundamental, so the ear infers it
	// from the harmonics (missing fundamental) and still hears "deep".

	// A saw-like, slightly detuned tone that powers up (start) or down (stop).
	"warp": {
		{notes: []note{{0, 110, 1}},
			partials: [][2]float64{{1, 0.35}, {1.006, 0.3}, {2, 0.7}, {3, 0.65}, {4, 0.5}, {5, 0.4}, {6, 0.3}, {7, 0.2}, {8, 0.12}},
			attack:   0.02, decay: 0.16, glide: 1.8, glideT: 0.22, echoes: [][2]float64{{0.12, 0.3}, {0.26, 0.12}}},
		{notes: []note{{0, 198, 1}},
			partials: [][2]float64{{1, 0.35}, {1.006, 0.3}, {2, 0.7}, {3, 0.65}, {4, 0.5}, {5, 0.4}, {6, 0.3}, {7, 0.2}, {8, 0.12}},
			attack:   0.01, decay: 0.16, glide: 0.55, glideT: 0.22, echoes: [][2]float64{{0.12, 0.3}, {0.26, 0.12}}},
	},
	// Low metallic FM tone with a glide and a faint high glint on top.
	"nebula": {
		{notes: []note{{0, 147, 1}, {0.09, 1175, 0.12}},
			partials: [][2]float64{{1, 1}, {2, 0.35}},
			attack:   0.01, decay: 0.18, glide: 1.5, glideT: 0.15, fmRatio: 1.41, fmIndex: 3,
			echoes: [][2]float64{{0.13, 0.35}, {0.28, 0.15}}},
		{notes: []note{{0, 220, 1}, {0.09, 880, 0.1}},
			partials: [][2]float64{{1, 1}, {2, 0.35}},
			attack:   0.01, decay: 0.18, glide: 0.67, glideT: 0.15, fmRatio: 1.41, fmIndex: 3,
			echoes: [][2]float64{{0.13, 0.35}, {0.28, 0.15}}},
	},
	// A deep sonar ping with far-off echoes; the stop ping sits a fourth lower.
	"sonar": {
		{notes: []note{{0, 196, 1}},
			partials: [][2]float64{{1, 0.5}, {2, 0.8}, {3, 0.55}, {4.2, 0.25}},
			attack:   0.006, decay: 0.2, glide: 1, fmRatio: 2, fmIndex: 0.8,
			echoes: [][2]float64{{0.22, 0.3}, {0.45, 0.12}}},
		{notes: []note{{0, 147, 1}},
			partials: [][2]float64{{1, 0.5}, {2, 0.8}, {3, 0.55}, {4.2, 0.25}},
			attack:   0.006, decay: 0.2, glide: 1, fmRatio: 2, fmIndex: 0.8,
			echoes: [][2]float64{{0.22, 0.3}, {0.45, 0.12}}},
	},
	// A staggered Cmaj7 (C5 G5 B5) with a long, echoing tail: the airiest. Kept
	// an octave low and topped by B5 rather than E7 so the tail isn't shrill.
	"halo": {
		{notes: []note{{0, 523.25, 0.8}, {0.035, 784, 0.65}, {0.07, 987.8, 0.4}},
			partials: [][2]float64{{1, 1}, {1.003, 0.6}, {2, 0.15}},
			attack:   0.015, decay: 0.22, glide: 1, echoes: [][2]float64{{0.11, 0.4}, {0.23, 0.22}, {0.37, 0.1}}},
		{notes: []note{{0, 987.8, 0.4}, {0.035, 784, 0.65}, {0.07, 523.25, 0.8}},
			partials: [][2]float64{{1, 1}, {1.003, 0.6}, {2, 0.15}},
			attack:   0.015, decay: 0.22, glide: 1, echoes: [][2]float64{{0.11, 0.4}, {0.23, 0.22}, {0.37, 0.1}}},
	},
}

// render synthesizes a cue as 16-bit mono WAV bytes, normalized to a gentle peak.
func (c cue) render() []byte {
	var noteEnd, echoEnd float64
	for _, n := range c.notes {
		noteEnd = max(noteEnd, n.at+c.attack+6*c.decay)
	}
	for _, e := range c.echoes {
		echoEnd = max(echoEnd, e[0])
	}
	dry := make([]float64, int((noteEnd+echoEnd)*sampleRate)+1)
	for _, n := range c.notes {
		start := int(n.at * sampleRate)
		length := int((c.attack + 6*c.decay) * sampleRate)
		for _, p := range c.partials {
			phase, modPhase := 0.0, 0.0
			for i := 0; i < length && start+i < len(dry); i++ {
				t := float64(i) / sampleRate
				f := n.freq * p[0]
				if c.glide != 1 && c.glideT > 0 {
					f *= math.Pow(c.glide, min(t/c.glideT, 1))
				}
				phase += 2 * math.Pi * f / sampleRate
				env := math.Exp(-t / c.decay)
				if t < c.attack {
					env *= t / c.attack
				}
				mod := 0.0
				if c.fmRatio > 0 {
					modPhase += 2 * math.Pi * f * c.fmRatio / sampleRate
					mod = c.fmIndex * env * math.Sin(modPhase)
				}
				dry[start+i] += n.gain * p[1] * env * math.Sin(phase+mod)
			}
		}
	}
	out := append([]float64(nil), dry...)
	for _, e := range c.echoes {
		d := int(e[0] * sampleRate)
		for i := 0; i+d < len(out); i++ {
			out[i+d] += e[1] * dry[i]
		}
	}
	var peak float64
	for _, v := range out {
		peak = max(peak, math.Abs(v))
	}
	scale := 0.35 * 32767 / max(peak, 1e-9)

	var b bytes.Buffer
	data := len(out) * 2
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+data))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), // PCM, mono
		uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(data))
	for _, v := range out {
		binary.Write(&b, binary.LittleEndian, int16(v*scale))
	}
	return b.Bytes()
}

// soundName maps a `sound` setting to a cue name, or "" for off.
func soundName(setting string) string {
	switch setting {
	case "off":
		return ""
	case "", "on":
		return SoundNames[0]
	}
	if _, ok := cues[setting]; ok {
		return setting
	}
	return SoundNames[0]
}

// cueFile renders the cue once into the runtime dir and returns its path.
func cueFile(name string, stop bool) (string, error) {
	which := "start"
	if stop {
		which = "stop"
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, fmt.Sprintf("voicetype-cue-%s-%s.wav", name, which))
	// Render once per process: the runtime dir outlives restarts, and a file
	// from an older binary would keep playing a since-retuned cue.
	renderedMu.Lock()
	defer renderedMu.Unlock()
	if rendered[path] {
		return path, nil
	}
	c := cues[name][0]
	if stop {
		c = cues[name][1]
	}
	if err := os.WriteFile(path, c.render(), 0o644); err != nil {
		return "", err
	}
	rendered[path] = true
	return path, nil
}

var (
	renderedMu sync.Mutex
	rendered   = map[string]bool{}
)

// playCue plays the start or stop cue of a `sound` setting through PipeWire.
// lead > 0 blocks up to that long: the start cue uses it so its loud body is
// over before recording begins, while the faint tail rings on. Failures only
// log, since a cue is optional.
func playCue(setting string, stop bool, lead time.Duration) {
	name := soundName(setting)
	if name == "" {
		return
	}
	path, err := cueFile(name, stop)
	if err != nil {
		log.Printf("render cue: %v", err)
		return
	}
	cmd := exec.Command("pw-play", path)
	if err := cmd.Start(); err != nil {
		log.Printf("play cue: %v", err)
		return
	}
	go cmd.Wait()
	if lead > 0 {
		time.Sleep(lead)
	}
}
