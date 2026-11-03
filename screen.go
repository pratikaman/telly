package main

import (
	"bytes"
	"strconv"

	"github.com/rivo/uniseg"
)

type RGB struct{ R, G, B uint8 }

// The teletext palette, sampled from a Ceefax P100 render: pure primaries on black.
var (
	Black   = RGB{0, 0, 0}
	Red     = RGB{255, 0, 0}
	Green   = RGB{0, 255, 0}
	Yellow  = RGB{255, 255, 0}
	Blue    = RGB{0, 0, 255}
	Magenta = RGB{255, 0, 255}
	Cyan    = RGB{0, 255, 255}
	White   = RGB{255, 255, 255}
)

var Teletext = [8]RGB{Black, Red, Green, Yellow, Blue, Magenta, Cyan, White}

type Rect struct{ X, Y, W, H int }

type Cell struct {
	S  string
	FG RGB
	BG RGB
	W  int8 // 1 normal, 2 wide head, 0 continuation of a wide head
}

var blankCell = Cell{S: " ", FG: White, BG: Black, W: 1}

// Screen is a cell buffer that renders only what changed since the last frame.
type Screen struct {
	W, H  int
	cells []Cell
	prev  []Cell
	full  bool
}

func NewScreen(w, h int) *Screen {
	s := &Screen{}
	s.Resize(w, h)
	return s
}

func (s *Screen) Resize(w, h int) {
	s.W, s.H = w, h
	s.cells = make([]Cell, w*h)
	s.prev = make([]Cell, w*h)
	s.full = true
}

func (s *Screen) Invalidate() { s.full = true }

func (s *Screen) Clear() {
	for i := range s.cells {
		s.cells[i] = blankCell
	}
}

func (s *Screen) Set(x, y int, c Cell) {
	if x < 0 || y < 0 || x >= s.W || y >= s.H {
		return
	}
	s.cells[y*s.W+x] = c
}

func (s *Screen) Fill(x, y, w, h int, bg RGB) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			s.Set(xx, yy, Cell{S: " ", FG: White, BG: bg, W: 1})
		}
	}
}

func StrWidth(str string) int {
	w := 0
	gr := uniseg.NewGraphemes(str)
	for gr.Next() {
		gw := gr.Width()
		if gw > 2 {
			gw = 2
		}
		if g := gr.Str(); g == "\t" || g == "\n" || g == "\r" {
			gw = 1
		}
		w += gw
	}
	return w
}

// Text draws str at (x, y), truncating with an ellipsis to maxW cells
// (maxW < 0 means "to the right edge"). Returns the width drawn.
func (s *Screen) Text(x, y int, str string, fg, bg RGB, maxW int) int {
	if y < 0 || y >= s.H {
		return 0
	}
	if maxW < 0 {
		maxW = s.W - x
	}
	if maxW <= 0 {
		return 0
	}
	total := StrWidth(str)
	ellipsis := total > maxW
	limit := maxW
	if ellipsis {
		limit = maxW - 1
	}
	cx, used := x, 0
	gr := uniseg.NewGraphemes(str)
	for gr.Next() {
		g := gr.Str()
		w := gr.Width()
		if g == "\t" || g == "\n" || g == "\r" {
			g, w = " ", 1
		}
		if w <= 0 {
			continue
		}
		if w > 2 {
			w = 2
		}
		if used+w > limit {
			break
		}
		if cx >= 0 && cx+w <= s.W {
			s.cells[y*s.W+cx] = Cell{S: g, FG: fg, BG: bg, W: int8(w)}
			if w == 2 {
				s.cells[y*s.W+cx+1] = Cell{S: "", FG: fg, BG: bg, W: 0}
			}
		}
		cx += w
		used += w
	}
	if ellipsis {
		s.Set(cx, y, Cell{S: "…", FG: fg, BG: bg, W: 1})
		used++
	}
	return used
}

// TextRight right-aligns str so that it ends at column xEnd (exclusive).
func (s *Screen) TextRight(xEnd, y int, str string, fg, bg RGB) int {
	w := StrWidth(str)
	return s.Text(xEnd-w, y, str, fg, bg, w)
}

func (s *Screen) TextCenter(x, y, w int, str string, fg, bg RGB) {
	sw := StrWidth(str)
	if sw > w {
		s.Text(x, y, str, fg, bg, w)
		return
	}
	s.Text(x+(w-sw)/2, y, str, fg, bg, sw)
}

// narrowSafe reports glyphs every modern terminal draws exactly one cell wide,
// so the renderer can keep streaming without re-positioning the cursor.
func narrowSafe(str string) bool {
	if len(str) == 1 {
		return str[0] >= 0x20 && str[0] < 0x7f
	}
	r := []rune(str)
	if len(r) != 1 {
		return false
	}
	c := r[0]
	return (c >= 0x2500 && c <= 0x259f) || (c >= 0x1fb00 && c <= 0x1fb3b) || c == '…' || c == '·'
}

func appendSGR(buf *bytes.Buffer, fgSet bool, fg RGB, bgSet bool, bg RGB) {
	var tmp [64]byte
	b := tmp[:0]
	b = append(b, "\x1b["...)
	if fgSet {
		b = append(b, "38;2;"...)
		b = strconv.AppendUint(b, uint64(fg.R), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(fg.G), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(fg.B), 10)
	}
	if bgSet {
		if fgSet {
			b = append(b, ';')
		}
		b = append(b, "48;2;"...)
		b = strconv.AppendUint(b, uint64(bg.R), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(bg.G), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(bg.B), 10)
	}
	b = append(b, 'm')
	buf.Write(b)
}

func appendCUP(buf *bytes.Buffer, x, y int) {
	var tmp [24]byte
	b := append(tmp[:0], "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)
	b = append(b, 'H')
	buf.Write(b)
}

// Render appends the escape sequences that bring the terminal up to date.
func (s *Screen) Render(buf *bytes.Buffer) {
	curX, curY := -1, -1
	havePen := false
	var penFG, penBG RGB
	for y := 0; y < s.H; y++ {
		for x := 0; x < s.W; x++ {
			i := y*s.W + x
			c := s.cells[i]
			if c.W == 0 {
				if x > 0 && s.cells[i-1].W == 2 {
					continue
				}
				c = Cell{S: " ", FG: c.FG, BG: c.BG, W: 1}
				s.cells[i] = c
			}
			if c.W == 2 && x == s.W-1 {
				c = Cell{S: " ", FG: c.FG, BG: c.BG, W: 1}
				s.cells[i] = c
			}
			if !s.full && c == s.prev[i] && (c.W != 2 || s.cells[i+1] == s.prev[i+1]) {
				continue
			}
			if curX != x || curY != y {
				appendCUP(buf, x, y)
			}
			fgSet := !havePen || penFG != c.FG
			bgSet := !havePen || penBG != c.BG
			if fgSet || bgSet {
				appendSGR(buf, fgSet, c.FG, bgSet, c.BG)
				penFG, penBG, havePen = c.FG, c.BG, true
			}
			buf.WriteString(c.S)
			if c.W == 1 && narrowSafe(c.S) {
				curX, curY = x+1, y
			} else {
				curX = -1
			}
		}
	}
	copy(s.prev, s.cells)
	s.full = false
}
