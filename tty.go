package main

import (
	"errors"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type KeyCode int

const (
	KRune KeyCode = iota
	KEnter
	KEsc
	KBackspace
	KTab
	KUp
	KDown
	KLeft
	KRight
	KPgUp
	KPgDn
	KHome
	KEnd
	KDelete
	KCtrlC
	KCtrlU
	KCtrlW
	KCtrlL
	KUnknown
	KGfxReply // a kitty graphics response, not a key
)

type Key struct {
	Code KeyCode
	Rune rune
}

// TTY owns the controlling terminal: raw mode, input decoding and size.
type TTY struct {
	in, out *os.File
	state   *term.State
	raw     chan []byte
	Keys    chan Key
	Resize  chan struct{}
}

type Size struct{ Cols, Rows, PxW, PxH int }

func (s Size) CellW() float64 {
	if s.PxW > 0 && s.Cols > 0 {
		return float64(s.PxW) / float64(s.Cols)
	}
	return 10
}

func (s Size) CellH() float64 {
	if s.PxH > 0 && s.Rows > 0 {
		return float64(s.PxH) / float64(s.Rows)
	}
	return 21
}

func OpenTTY() (*TTY, error) {
	in, out := os.Stdin, os.Stdout
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil, errors.New("telly needs an interactive terminal")
	}
	st, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	t := &TTY{
		in: in, out: out, state: st,
		raw:    make(chan []byte, 256),
		Keys:   make(chan Key, 256),
		Resize: make(chan struct{}, 1),
	}
	go t.readLoop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		for range sig {
			select {
			case t.Resize <- struct{}{}:
			default:
			}
		}
	}()
	return t, nil
}

func (t *TTY) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := t.in.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			t.raw <- b
		}
		if err != nil {
			close(t.raw)
			return
		}
	}
}

func (t *TTY) Write(s string) { t.out.WriteString(s) }

func (t *TTY) WriteBytes(b []byte) { t.out.Write(b) }

func (t *TTY) Size() Size {
	ws, err := unix.IoctlGetWinsize(int(t.out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return Size{80, 24, 0, 0}
	}
	return Size{int(ws.Col), int(ws.Row), int(ws.Xpixel), int(ws.Ypixel)}
}

// Enter switches to the alternate screen, hides the cursor and disables autowrap.
func (t *TTY) Enter() {
	t.Write("\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[0m\x1b[2J")
}

func (t *TTY) Close() {
	t.Write("\x1b_Ga=d,d=A,q=2\x1b\\\x1b[0m\x1b[2J\x1b[?7h\x1b[?25h\x1b[?1049l")
	term.Restore(int(t.in.Fd()), t.state)
}

var daResp = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)

// Query writes a request and collects replies until the primary device
// attributes answer (which every terminal sends) or the timeout passes.
// Must be called before StartKeys.
func (t *TTY) Query(req string, timeout time.Duration) string {
	t.Write(req + "\x1b[c")
	var acc []byte
	deadline := time.After(timeout)
	for {
		select {
		case b, ok := <-t.raw:
			if !ok {
				return string(acc)
			}
			acc = append(acc, b...)
			if daResp.Match(acc) {
				return string(acc)
			}
		case <-deadline:
			return string(acc)
		}
	}
}

func (t *TTY) StartKeys() { go t.parseLoop() }

func (t *TTY) parseLoop() {
	var buf []byte
	for {
		var timeout <-chan time.Time
		if len(buf) > 0 {
			timeout = time.After(30 * time.Millisecond)
		}
		select {
		case b, ok := <-t.raw:
			if !ok {
				close(t.Keys)
				return
			}
			buf = append(buf, b...)
		case <-timeout:
			if buf[0] == 0x1b {
				t.Keys <- Key{Code: KEsc}
			}
			buf = buf[1:]
		}
		for len(buf) > 0 {
			k, n := parseKey(buf)
			if n == 0 {
				break
			}
			buf = buf[n:]
			if k.Code == KGfxReply {
				gfxAcks.Add(1)
				continue
			}
			if k.Code != KUnknown {
				t.Keys <- k
			}
		}
	}
}

// parseKey decodes one key from b. n == 0 means more bytes are needed.
func parseKey(b []byte) (Key, int) {
	c := b[0]
	switch {
	case c == 0x1b:
		if len(b) == 1 {
			return Key{}, 0
		}
		switch b[1] {
		case '[':
			for i := 2; i < len(b); i++ {
				if b[i] >= 0x40 && b[i] <= 0x7e {
					return csiKey(string(b[2:i]), b[i]), i + 1
				}
			}
			if len(b) > 64 {
				return Key{Code: KUnknown}, len(b)
			}
			return Key{}, 0
		case 'O':
			if len(b) < 3 {
				return Key{}, 0
			}
			return csiKey("", b[2]), 3
		case '_', 'P', ']':
			for i := 2; i < len(b); i++ {
				if b[i] == 0x07 {
					return Key{Code: KUnknown}, i + 1
				}
				if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
					if b[1] == '_' && b[2] == 'G' {
						return Key{Code: KGfxReply}, i + 2
					}
					return Key{Code: KUnknown}, i + 2
				}
			}
			return Key{}, 0
		default:
			return Key{Code: KEsc}, 1
		}
	case c == '\r' || c == '\n':
		return Key{Code: KEnter}, 1
	case c == 0x7f || c == 0x08:
		return Key{Code: KBackspace}, 1
	case c == '\t':
		return Key{Code: KTab}, 1
	case c == 0x03:
		return Key{Code: KCtrlC}, 1
	case c == 0x15:
		return Key{Code: KCtrlU}, 1
	case c == 0x17:
		return Key{Code: KCtrlW}, 1
	case c == 0x0c:
		return Key{Code: KCtrlL}, 1
	case c < 0x20:
		return Key{Code: KUnknown}, 1
	}
	if !utf8.FullRune(b) {
		return Key{}, 0
	}
	r, n := utf8.DecodeRune(b)
	return Key{Code: KRune, Rune: r}, n
}

func csiKey(params string, final byte) Key {
	switch final {
	case 'A':
		return Key{Code: KUp}
	case 'B':
		return Key{Code: KDown}
	case 'C':
		return Key{Code: KRight}
	case 'D':
		return Key{Code: KLeft}
	case 'H':
		return Key{Code: KHome}
	case 'F':
		return Key{Code: KEnd}
	case '~':
		switch params {
		case "5":
			return Key{Code: KPgUp}
		case "6":
			return Key{Code: KPgDn}
		case "3":
			return Key{Code: KDelete}
		case "1", "7":
			return Key{Code: KHome}
		case "4", "8":
			return Key{Code: KEnd}
		}
	}
	return Key{Code: KUnknown}
}
