package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Video frames ask the terminal to acknowledge them. gfxSent counts frames
// written, gfxAcks replies read back; the gap is how far the terminal lags.
var gfxSent, gfxAcks atomic.Int64

// GfxMode is how (and whether) we can push pixels with the kitty graphics protocol.
type GfxMode int

const (
	GfxNone   GfxMode = iota // no image support: mosaic renderers only
	GfxDirect                // pixels inline through the pty (zlib + base64)
	GfxFile                  // pixels in a temp file the terminal reads and deletes
)

func (m GfxMode) String() string {
	switch m {
	case GfxDirect:
		return "kitty graphics, inline"
	case GfxFile:
		return "kitty graphics, temp files"
	}
	return "no image support"
}

type Gfx struct {
	Mode GfxMode
	Dir  string
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func tempDirs() []string {
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		d = strings.TrimRight(d, "/")
		for _, x := range out {
			if x == d {
				return
			}
		}
		out = append(out, d)
	}
	add(os.TempDir())
	if rp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		add(rp)
	}
	add("/tmp")
	return out
}

// ProbeGfx asks the terminal which kitty transmission mediums it accepts.
// cmux (libghostty) accepts temp files only under $TMPDIR, Ghostty also /tmp.
func ProbeGfx(t *TTY) *Gfx {
	g := &Gfx{}
	force := strings.ToLower(os.Getenv("TELLY_GFX"))
	if force == "none" || force == "off" || force == "0" {
		return g
	}
	px := []byte{0, 0, 0}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;%s\x1b\\", b64(px))
	type cand struct {
		id   int
		path string
	}
	var cands []cand
	if force != "direct" {
		for k, d := range tempDirs() {
			f, err := os.CreateTemp(d, "tty-graphics-protocol-telly-probe-*.rgb")
			if err != nil {
				continue
			}
			f.Write(px)
			f.Close()
			c := cand{40 + k, f.Name()}
			cands = append(cands, c)
			fmt.Fprintf(&sb, "\x1b_Gi=%d,s=1,v=1,a=q,t=t,f=24;%s\x1b\\", c.id, b64([]byte(c.path)))
		}
	}
	resp := t.Query(sb.String(), 1500*time.Millisecond)
	for _, c := range cands {
		os.Remove(c.path) // the terminal deletes accepted files; clean up the rest
	}
	if strings.Contains(resp, "i=31;OK") {
		g.Mode = GfxDirect
	}
	for _, c := range cands {
		if strings.Contains(resp, fmt.Sprintf("i=%d;OK", c.id)) {
			g.Mode = GfxFile
			g.Dir = filepath.Dir(c.path)
			break
		}
	}
	return g
}

// Place transmits an RGB24 image and places it at cell (x, y) scaled to
// cols×rows cells. It returns the escape sequence and the temp file (if any).
// With ack the terminal replies once it has taken the image in.
func (g *Gfx) Place(id int, rgb []byte, w, h int, r Rect, ack bool) (string, string, error) {
	q := "q=2"
	if ack {
		q = "q=0"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b[%d;%dH", r.Y+1, r.X+1)
	switch g.Mode {
	case GfxFile:
		f, err := os.CreateTemp(g.Dir, fmt.Sprintf("tty-graphics-protocol-telly-%d-*.rgb", os.Getpid()))
		if err != nil {
			return "", "", err
		}
		if _, err := f.Write(rgb[:w*h*3]); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", "", err
		}
		f.Close()
		fmt.Fprintf(&sb, "\x1b_Ga=T,t=t,f=24,s=%d,v=%d,i=%d,p=1,c=%d,r=%d,C=1,%s;%s\x1b\\",
			w, h, id, r.W, r.H, q, b64([]byte(f.Name())))
		return sb.String(), f.Name(), nil
	case GfxDirect:
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, zlib.BestSpeed)
		zw.Write(rgb[:w*h*3])
		zw.Close()
		data := b64(zb.Bytes())
		first := true
		for len(data) > 0 {
			n := min(4096, len(data))
			chunk := data[:n]
			data = data[n:]
			m := 0
			if len(data) > 0 {
				m = 1
			}
			if first {
				fmt.Fprintf(&sb, "\x1b_Ga=T,t=d,f=24,o=z,s=%d,v=%d,i=%d,p=1,c=%d,r=%d,C=1,%s,m=%d;%s\x1b\\",
					w, h, id, r.W, r.H, q, m, chunk)
				first = false
			} else {
				fmt.Fprintf(&sb, "\x1b_Gm=%d;%s\x1b\\", m, chunk)
			}
		}
		return sb.String(), "", nil
	}
	return "", "", fmt.Errorf("no graphics support")
}

func gfxDelete(id int) string { return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id) }

const gfxDeleteAll = "\x1b_Ga=d,d=A,q=2\x1b\\"

// Cleanup removes temp frames the terminal never got round to reading.
func (g *Gfx) Cleanup() {
	if g.Mode != GfxFile {
		return
	}
	files, _ := filepath.Glob(filepath.Join(g.Dir, fmt.Sprintf("tty-graphics-protocol-telly-%d-*", os.Getpid())))
	for _, f := range files {
		os.Remove(f)
	}
}
