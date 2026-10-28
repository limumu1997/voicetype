package dictation

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Daemon owns the warm sidecar and the recording state machine, and serves
// `toggle`/`status`/`quit` commands over a unix socket plus the config web UI.
type Daemon struct {
	cfg Config
	kbd *virtualKeyboard

	mu      sync.Mutex
	rec     *recorder // non-nil while recording
	sound   string    // cue setting; changed live from the web UI
	sc      *sidecar  // nil until the model has loaded
	loadErr error     // why the sidecar failed to start, if it did
}

// RunDaemon opens the socket and web UI first, then loads the model, and
// blocks until killed. Serving first means a hotkey press during the several
// seconds of model loading gets a "loading" notice instead of "daemon not
// running", and a model that fails to load leaves the daemon up so the web UI
// can show the error and fix the config.
func RunDaemon() error {
	cfg := LoadConfig()

	kbd, err := openKeyboard()
	if err != nil {
		return err
	}
	defer kbd.close()
	log.Printf("virtual keyboard ready")

	sock := SocketPath()
	_ = os.Remove(sock) // clear any stale socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	defer os.Remove(sock)
	log.Printf("listening on %s", sock)

	d := &Daemon{cfg: cfg, kbd: kbd, sound: cfg.Sound}
	if cfg.Web != "" && cfg.Web != "off" {
		go func() {
			if err := d.serveWeb(cfg.Web); err != nil {
				log.Printf("web ui: %v", err)
			}
		}()
	}

	var stopping atomic.Bool
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		stopping.Store(true)
		log.Printf("shutting down")
		ln.Close()
		os.Remove(sock)
		kbd.close()
		d.mu.Lock()
		sc := d.sc
		d.mu.Unlock()
		if sc != nil {
			sc.close()
		}
		os.Exit(0)
	}()

	go d.loadSidecar(sock, &stopping)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed during shutdown
		}
		go d.handle(conn)
	}
}

// loadSidecar starts the model and watches it. A failed load is retried a few
// times (a transient OOM or a kill mid-load shouldn't stick), then kept for the
// web UI rather than exiting: restarting on a bad config would just crash-loop.
func (d *Daemon) loadSidecar(sock string, stopping *atomic.Bool) {
	cfg := d.cfg
	t0 := time.Now()
	var sc *sidecar
	var err error
	for attempt, backoff := range []time.Duration{5 * time.Second, 15 * time.Second, 0} {
		log.Printf("starting sidecar (backend=%s model=%s device=%s compute=%s)...", cfg.Backend, cfg.Model, cfg.Device, cfg.Compute)
		if sc, err = startSidecar(cfg); err == nil || backoff == 0 {
			break
		}
		log.Printf("sidecar failed to start (attempt %d): %v; retrying in %s", attempt+1, err, backoff)
		time.Sleep(backoff)
	}
	d.mu.Lock()
	d.sc, d.loadErr = sc, err
	d.mu.Unlock()
	if err != nil {
		log.Printf("sidecar failed to start: %v", err)
		notify("❌ 识别引擎启动失败（打开 voicetype config 查看）", 6000)
		return
	}
	took := time.Since(t0).Seconds()
	log.Printf("sidecar ready in %.1fs", took)
	// Tell a crash recovery apart by the marker the watcher below leaves.
	what := "语音输入已就绪"
	if _, err := os.Stat(recoveredMarker()); err == nil {
		os.Remove(recoveredMarker())
		what = "语音输入已恢复"
	}
	notify(fmt.Sprintf("🎙️ %s（%s，加载 %.0f 秒）", what, cfg.Backend, took), 4000)

	// A sidecar that dies on its own (OOM, CUDA error, timeout kill) would leave
	// every later toggle failing while the daemon looks healthy. Exit non-zero
	// instead so systemd's Restart=on-failure brings both back with a fresh model.
	<-sc.done
	if stopping.Load() {
		return
	}
	log.Printf("sidecar exited unexpectedly (%s); exiting for systemd to restart", sc.exitState)
	notify("⚠️ 识别引擎退出，正在自动重启…", 4000)
	os.WriteFile(recoveredMarker(), nil, 0o644)
	os.Remove(sock)
	os.Exit(1)
}

func (d *Daemon) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return
	}
	switch cmd := sc.Text(); cmd {
	case "toggle":
		fmt.Fprintln(conn, d.toggle())
	case "status":
		fmt.Fprintln(conn, d.status())
	case "quit":
		fmt.Fprintln(conn, "bye")
		os.Exit(0)
	default:
		fmt.Fprintf(conn, "unknown command: %s\n", cmd)
	}
}

// status is one of recording | idle | loading | error.
func (d *Daemon) status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.rec != nil:
		return "recording"
	case d.sc != nil:
		return "idle"
	case d.loadErr != nil:
		return "error"
	}
	return "loading"
}

// engine returns the loaded sidecar, or nil plus a user-facing reason.
func (d *Daemon) engine() (*sidecar, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.sc != nil:
		return d.sc, ""
	case d.loadErr != nil:
		return nil, "❌ 识别引擎启动失败（打开 voicetype config 查看）"
	}
	return nil, "⏳ 模型加载中，请稍后再试"
}

// toggle flips between idle and recording. On the stop edge it transcribes the
// captured audio and pastes the result into the focused window.
func (d *Daemon) toggle() string {
	sc, why := d.engine()
	if sc == nil {
		notify(why, 3000)
		return d.status()
	}

	d.mu.Lock()
	if d.rec == nil {
		// Let the cue's loud body finish so the mic doesn't pick it up; its
		// faint tail rings on into the recording.
		playCue(d.sound, false, 250*time.Millisecond)
		r, err := startRecording(d.cfg.Source)
		if err != nil {
			d.mu.Unlock()
			log.Printf("start recording: %v", err)
			notify("❌ 录音失败: "+err.Error(), 4000)
			return "error"
		}
		d.rec = r
		d.mu.Unlock()
		notify("🎤 录音中… 再按一次结束", 0)
		return "recording"
	}

	// Stop edge: take ownership of the recorder, then release the lock so a new
	// toggle isn't blocked by transcription.
	r := d.rec
	d.rec = nil
	sound := d.sound
	d.mu.Unlock()
	closeNotify() // drop the "recording" banner now, not after transcription (~20ms)

	wav, err := r.stop()
	playCue(sound, true, 0)
	if err != nil {
		log.Printf("stop recording: %v", err)
		notify("❌ "+err.Error(), 4000)
		return "error"
	}
	defer os.Remove(wav)

	// Look up the focus while the model runs; it rarely changes in that second.
	focusc := make(chan FocusKind, 1)
	go func() { focusc <- DetectFocus() }()

	text, err := sc.transcribe(wav, d.cfg.Lang)
	if err != nil {
		log.Printf("transcribe: %v", err)
		notify("❌ 转写失败: "+err.Error(), 4000)
		return "error"
	}
	if text == "" {
		log.Printf("transcribed nothing")
		notify("🤔 没听清（无内容）", 2000)
		return "empty"
	}
	focus := <-focusc
	log.Printf("transcribed %d chars, focus=%s", len([]rune(text)), focus)
	pasted, err := typeText(d.kbd, text, focus)
	if err != nil {
		log.Printf("paste (focus=%s): %v", focus, err)
		notify("⚠️ 已转写，但粘贴失败，内容已在剪贴板可手动粘贴", 4000)
		return "copied"
	}
	if !pasted {
		log.Printf("no input focused; left on clipboard")
		notify("📋 没有输入框，已存入剪贴板（GPaste 可找回）", 3000)
		return "copied"
	}
	// No success notification: the pasted text and the stop cue are the
	// feedback, and the recording banner is already gone.
	return "ok"
}

// Toggle is the client side: connect to the daemon and ask it to toggle.
func Toggle() error {
	conn, err := net.Dial("unix", SocketPath())
	if err != nil {
		return fmt.Errorf("daemon not running? (%w)", err)
	}
	defer conn.Close()
	fmt.Fprintln(conn, "toggle")
	resp, _ := bufio.NewReader(conn).ReadString('\n')
	fmt.Print(resp)
	return nil
}

// recoveredMarker is left behind when the daemon exits for systemd to restart
// it, so the next start can announce a recovery rather than a plain start.
func recoveredMarker() string {
	return SocketPath() + ".recovering"
}
