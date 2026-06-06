package dictation

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Daemon owns the warm sidecar and the recording state machine, and serves
// `toggle`/`status`/`quit` commands over a unix socket.
type Daemon struct {
	cfg Config
	sc  *sidecar
	kbd *virtualKeyboard

	mu  sync.Mutex
	rec *recorder // non-nil while recording
}

// RunDaemon starts the sidecar, listens on the socket, and blocks until killed.
func RunDaemon() error {
	cfg := LoadConfig()

	log.Printf("starting sidecar (model=%s device=%s compute=%s)...", cfg.Model, cfg.Device, cfg.Compute)
	sc, err := startSidecar(cfg)
	if err != nil {
		return err
	}
	defer sc.close()
	log.Printf("sidecar ready")

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

	d := &Daemon{cfg: cfg, sc: sc, kbd: kbd}

	// Clean shutdown on signals so the socket and sidecar don't leak.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		log.Printf("shutting down")
		ln.Close()
		os.Remove(sock)
		kbd.close()
		sc.close()
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed during shutdown
		}
		go d.handle(conn)
	}
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

func (d *Daemon) status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rec != nil {
		return "recording"
	}
	return "idle"
}

// toggle flips between idle and recording. On the stop edge it transcribes the
// captured audio and pastes the result into the focused window.
func (d *Daemon) toggle() string {
	d.mu.Lock()
	if d.rec == nil {
		r, err := startRecording()
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
	d.mu.Unlock()

	wav, err := r.stop()
	if err != nil {
		log.Printf("stop recording: %v", err)
		notify("❌ "+err.Error(), 4000)
		return "error"
	}
	defer os.Remove(wav)

	text, err := d.sc.transcribe(wav, d.cfg.Lang)
	if err != nil {
		log.Printf("transcribe: %v", err)
		notify("❌ 转写失败: "+err.Error(), 4000)
		return "error"
	}
	if text == "" {
		notify("🤔 没听清（无内容）", 2000)
		return "empty"
	}
	if err := typeText(d.kbd, text); err != nil {
		log.Printf("paste: %v", err)
		notify("⚠️ 已转写，但粘贴失败，内容已在剪贴板可手动粘贴", 4000)
		return "copied"
	}
	// Brief tick replacing the 🎤 indicator; the inserted text is the real
	// feedback, so it fades quickly to avoid notification spam.
	notify("✅", 800)
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
