package dictation

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Pasting CJK text by simulated keystrokes is impossible (no key per glyph), so
// we put text on the clipboard and inject a real Ctrl+V. On GNOME Wayland the
// only injection path that works is the kernel's uinput device — wtype's
// virtual-keyboard protocol is unsupported by Mutter, and Ubuntu only ships an
// ancient ydotool. So we drive /dev/uinput directly here, no external tool.

// ioctl request codes (asm-generic encoding, valid on amd64/arm64).
const (
	uiSetEvBit   = 0x40045564 // _IOW('U', 100, int)
	uiSetKeyBit  = 0x40045565 // _IOW('U', 101, int)
	uiDevCreate  = 0x5501     // _IO('U', 1)
	uiDevDestroy = 0x5502     // _IO('U', 2)
)

// Linux input event constants.
const (
	evSyn       = 0x00
	evKey       = 0x01
	synReport   = 0x00
	keyLeftCtrl = 29
	keyV        = 47
	busUSB      = 0x03
)

type uinputUserDev struct {
	Name [80]byte
	ID   struct{ Bustype, Vendor, Product, Version uint16 }
	FF   uint32
	_    [64]int32 // absmax
	_    [64]int32 // absmin
	_    [64]int32 // absfuzz
	_    [64]int32 // absflat
}

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

// virtualKeyboard is a uinput device kept alive for the daemon's lifetime so the
// one-time udev settle delay is paid once, not on every paste.
type virtualKeyboard struct {
	f *os.File
}

func openKeyboard() (*virtualKeyboard, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/uinput (need 'input' group + udev rule): %w", err)
	}
	fd := f.Fd()

	if err := ioctl(fd, uiSetEvBit, evKey); err != nil {
		f.Close()
		return nil, fmt.Errorf("set evbit key: %w", err)
	}
	if err := ioctl(fd, uiSetEvBit, evSyn); err != nil {
		f.Close()
		return nil, fmt.Errorf("set evbit syn: %w", err)
	}
	for _, k := range []uintptr{keyLeftCtrl, keyV} {
		if err := ioctl(fd, uiSetKeyBit, k); err != nil {
			f.Close()
			return nil, fmt.Errorf("set keybit %d: %w", k, err)
		}
	}

	var dev uinputUserDev
	copy(dev.Name[:], "voicetype-virtual-kbd")
	dev.ID.Bustype = busUSB
	dev.ID.Vendor = 0x1234
	dev.ID.Product = 0x5678
	dev.ID.Version = 1
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &dev); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return nil, fmt.Errorf("write uinput_user_dev: %w", err)
	}
	if err := ioctl(fd, uiDevCreate, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	// Let udev create the node and the compositor pick up the new device.
	time.Sleep(300 * time.Millisecond)
	return &virtualKeyboard{f: f}, nil
}

// pasteShortcut presses Ctrl+V.
func (k *virtualKeyboard) pasteShortcut() error {
	seq := []inputEvent{
		{Type: evKey, Code: keyLeftCtrl, Value: 1},
		{Type: evSyn, Code: synReport, Value: 0},
		{Type: evKey, Code: keyV, Value: 1},
		{Type: evSyn, Code: synReport, Value: 0},
		{Type: evKey, Code: keyV, Value: 0},
		{Type: evSyn, Code: synReport, Value: 0},
		{Type: evKey, Code: keyLeftCtrl, Value: 0},
		{Type: evSyn, Code: synReport, Value: 0},
	}
	for _, ev := range seq {
		if err := k.emit(ev); err != nil {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

func (k *virtualKeyboard) emit(ev inputEvent) error {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, &ev)
	_, err := k.f.Write(buf.Bytes())
	return err
}

func (k *virtualKeyboard) close() error {
	if k.f == nil {
		return nil
	}
	_ = ioctl(k.f.Fd(), uiDevDestroy, 0)
	return k.f.Close()
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
