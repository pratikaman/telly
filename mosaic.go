package main

import "strings"

// Teletext graphics are 2×3 "sextant" mosaics. Unicode 13 carries the same
// set in Symbols for Legacy Computing (U+1FB00–U+1FB3B), which Ghostty and
// cmux draw natively. Bit k is pixel (k%2, k/2): 1=top-left … 32=bottom-right.
func Sextant(p int) string {
	switch p {
	case 0:
		return " "
	case 21:
		return "▌"
	case 42:
		return "▐"
	case 63:
		return "█"
	}
	idx := p - 1
	if p > 21 {
		idx--
	}
	if p > 42 {
		idx--
	}
	return string(rune(0x1FB00 + idx))
}

var sextantTable [64]string

func init() {
	for p := range 64 {
		sextantTable[p] = Sextant(p)
	}
}

// Bitmap is a 1-bit image drawn with sextants: 2 px per column, 3 per row.
type Bitmap struct {
	W, H int
	P    []bool
}

func NewBitmap(w, h int) *Bitmap { return &Bitmap{W: w, H: h, P: make([]bool, w*h)} }

func (b *Bitmap) Set(x, y int, v bool) {
	if x >= 0 && y >= 0 && x < b.W && y < b.H {
		b.P[y*b.W+x] = v
	}
}

func (b *Bitmap) Get(x, y int) bool {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return false
	}
	return b.P[y*b.W+x]
}

func (b *Bitmap) FillRect(x, y, w, h int, v bool) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			b.Set(xx, yy, v)
		}
	}
}

// Glyph stamps a font glyph (rows of '#'/'.') at (x, y) with value v.
func (b *Bitmap) Glyph(x, y int, rows []string, v bool) {
	for gy, row := range rows {
		for gx, ch := range row {
			if ch == '#' {
				b.Set(x+gx, y+gy, v)
			}
		}
	}
}

func (s *Screen) DrawBitmap(x0, y0 int, b *Bitmap, on, off RGB) {
	cols, rows := (b.W+1)/2, (b.H+2)/3
	for cy := range rows {
		for cx := range cols {
			p := 0
			for k := range 6 {
				if b.Get(cx*2+k%2, cy*3+k/2) {
					p |= 1 << k
				}
			}
			s.Set(x0+cx, y0+cy, Cell{S: sextantTable[p], FG: on, BG: off, W: 1})
		}
	}
}

// --- fonts -----------------------------------------------------------------

// A 3×5 font: drawn as sextants each glyph is 2 cells × 2 rows, which gives
// the tall, condensed "double height" headline look of a Ceefax page.
var font3x5 = map[rune][]string{
	'A':  {".#.", "#.#", "###", "#.#", "#.#"},
	'B':  {"##.", "#.#", "##.", "#.#", "##."},
	'C':  {".##", "#..", "#..", "#..", ".##"},
	'D':  {"##.", "#.#", "#.#", "#.#", "##."},
	'E':  {"###", "#..", "##.", "#..", "###"},
	'F':  {"###", "#..", "##.", "#..", "#.."},
	'G':  {".##", "#..", "#.#", "#.#", ".##"},
	'H':  {"#.#", "#.#", "###", "#.#", "#.#"},
	'I':  {"###", ".#.", ".#.", ".#.", "###"},
	'J':  {"..#", "..#", "..#", "#.#", ".#."},
	'K':  {"#.#", "#.#", "##.", "#.#", "#.#"},
	'L':  {"#..", "#..", "#..", "#..", "###"},
	'M':  {"#.#", "###", "###", "#.#", "#.#"},
	'N':  {"##.", "#.#", "#.#", "#.#", "#.#"},
	'O':  {".#.", "#.#", "#.#", "#.#", ".#."},
	'P':  {"##.", "#.#", "##.", "#..", "#.."},
	'Q':  {".#.", "#.#", "#.#", "##.", ".##"},
	'R':  {"##.", "#.#", "##.", "#.#", "#.#"},
	'S':  {".##", "#..", ".#.", "..#", "##."},
	'T':  {"###", ".#.", ".#.", ".#.", ".#."},
	'U':  {"#.#", "#.#", "#.#", "#.#", "###"},
	'V':  {"#.#", "#.#", "#.#", "#.#", ".#."},
	'W':  {"#.#", "#.#", "###", "###", "#.#"},
	'X':  {"#.#", "#.#", ".#.", "#.#", "#.#"},
	'Y':  {"#.#", "#.#", ".#.", ".#.", ".#."},
	'Z':  {"###", "..#", ".#.", "#..", "###"},
	'0':  {"###", "#.#", "#.#", "#.#", "###"},
	'1':  {".#.", "##.", ".#.", ".#.", "###"},
	'2':  {"##.", "..#", ".#.", "#..", "###"},
	'3':  {"##.", "..#", ".#.", "..#", "##."},
	'4':  {"#.#", "#.#", "###", "..#", "..#"},
	'5':  {"###", "#..", "##.", "..#", "##."},
	'6':  {".##", "#..", "###", "#.#", "###"},
	'7':  {"###", "..#", ".#.", ".#.", ".#."},
	'8':  {"###", "#.#", "###", "#.#", "###"},
	'9':  {"###", "#.#", "###", "..#", "##."},
	' ':  {"...", "...", "...", "...", "..."},
	'.':  {"...", "...", "...", "...", ".#."},
	',':  {"...", "...", "...", ".#.", "#.."},
	':':  {"...", ".#.", "...", ".#.", "..."},
	';':  {"...", ".#.", "...", ".#.", "#.."},
	'!':  {".#.", ".#.", ".#.", "...", ".#."},
	'?':  {"##.", "..#", ".#.", "...", ".#."},
	'\'': {".#.", ".#.", "...", "...", "..."},
	'"':  {"#.#", "#.#", "...", "...", "..."},
	'-':  {"...", "...", "###", "...", "..."},
	'+':  {"...", ".#.", "###", ".#.", "..."},
	'/':  {"..#", "..#", ".#.", "#..", "#.."},
	'&':  {".#.", "#.#", ".#.", "#.#", ".##"},
	'#':  {"#.#", "###", "#.#", "###", "#.#"},
	'(':  {".#.", "#..", "#..", "#..", ".#."},
	')':  {".#.", "..#", "..#", "..#", ".#."},
	'[':  {"##.", "#..", "#..", "#..", "##."},
	']':  {".##", "..#", "..#", "..#", ".##"},
	'|':  {".#.", ".#.", ".#.", ".#.", ".#."},
	'_':  {"...", "...", "...", "...", "###"},
	'*':  {"#.#", ".#.", "###", ".#.", "#.#"},
	'=':  {"...", "###", "...", "###", "..."},
	'%':  {"#.#", "..#", ".#.", "#..", "#.#"},
	'@':  {".#.", "#.#", "###", "#..", ".##"},
	'$':  {".##", "##.", ".#.", ".##", "##."},
	'~':  {"...", ".#.", "#.#", "...", "..."},
}

// BigText renders str in the 3×5 font (M, N and W are wider so they stay
// legible). Characters the font lacks are dropped; overlong text ends in "..".
func BigText(str string, maxCells int) *Bitmap {
	var glyphs [][]string
	px := 0
	for _, r := range strings.ToUpper(str) {
		g, ok := font3x5[r]
		if !ok {
			continue
		}
		adv := len(g[0]) + 1
		if (px+adv+8+1)/2 > maxCells {
			glyphs = append(glyphs, font3x5['.'], font3x5['.'])
			px += 8
			break
		}
		glyphs = append(glyphs, g)
		px += adv
	}
	b := NewBitmap(px, 6)
	x := 0
	for _, g := range glyphs {
		b.Glyph(x, 0, g, true)
		x += len(g[0]) + 1
	}
	return b
}

// A chunky 6×7 face for the masthead, after the Ceefax logo lettering.
var fontLogo = map[rune][]string{
	'T': {"######", "######", "..##..", "..##..", "..##..", "..##..", "..##.."},
	'E': {"######", "##....", "##....", "#####.", "##....", "##....", "######"},
	'L': {"##....", "##....", "##....", "##....", "##....", "##....", "######"},
	'Y': {"##..##", "##..##", "##..##", ".####.", "..##..", "..##..", "..##.."},
	'O': {".####.", "##..##", "##..##", "##..##", "##..##", "##..##", ".####."},
	'U': {"##..##", "##..##", "##..##", "##..##", "##..##", "##..##", ".####."},
}

// --- pictures --------------------------------------------------------------

// Pic is a packed RGB24 image.
type Pic struct {
	W, H int
	RGB  []byte
}

// Resize box-filters (or nearest-neighbours, when enlarging) src to w×h.
func (p *Pic) Resize(w, h int) *Pic {
	out := &Pic{W: w, H: h, RGB: make([]byte, w*h*3)}
	if p == nil || p.W == 0 || p.H == 0 || w <= 0 || h <= 0 {
		return out
	}
	for y := range h {
		y0 := y * p.H / h
		y1 := (y + 1) * p.H / h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range w {
			x0 := x * p.W / w
			x1 := (x + 1) * p.W / w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, n int
			for yy := y0; yy < y1; yy++ {
				o := (yy*p.W + x0) * 3
				for xx := x0; xx < x1; xx++ {
					r += int(p.RGB[o])
					g += int(p.RGB[o+1])
					b += int(p.RGB[o+2])
					o += 3
					n++
				}
			}
			d := (y*w + x) * 3
			out.RGB[d] = uint8(r / n)
			out.RGB[d+1] = uint8(g / n)
			out.RGB[d+2] = uint8(b / n)
		}
	}
	return out
}

func lum(r, g, b int) int { return 299*r + 587*g + 114*b }

func dist2(a, b [3]int) int {
	dr, dg, db := a[0]-b[0], a[1]-b[1], a[2]-b[2]
	return 2*dr*dr + 4*dg*dg + 3*db*db
}

func toRGB(c [3]int) RGB { return RGB{uint8(c[0]), uint8(c[1]), uint8(c[2])} }

// SextantCells turns a (cols*2)×(rows*3) RGB image into one two-colour
// sextant per cell (fg = the brighter cluster, bg = the darker one).
func SextantCells(rgb []byte, cols, rows int, out []Cell) {
	pw := cols * 2
	var px [6][3]int
	for cy := range rows {
		for cx := range cols {
			lo, hi := 0, 0
			var ls [6]int
			for k := range 6 {
				o := ((cy*3+k/2)*pw + cx*2 + k%2) * 3
				px[k] = [3]int{int(rgb[o]), int(rgb[o+1]), int(rgb[o+2])}
				ls[k] = lum(px[k][0], px[k][1], px[k][2])
				if ls[k] < ls[lo] {
					lo = k
				}
				if ls[k] > ls[hi] {
					hi = k
				}
			}
			cell := &out[cy*cols+cx]
			if ls[hi]-ls[lo] < 14000 {
				var s [3]int
				for k := range 6 {
					s[0] += px[k][0]
					s[1] += px[k][1]
					s[2] += px[k][2]
				}
				c := RGB{uint8(s[0] / 6), uint8(s[1] / 6), uint8(s[2] / 6)}
				*cell = Cell{S: " ", FG: c, BG: c, W: 1}
				continue
			}
			a, b := px[hi], px[lo]
			pat := 0
			for iter := 0; iter < 2; iter++ {
				var sa, sb [3]int
				na, nb := 0, 0
				pat = 0
				for k := range 6 {
					if dist2(px[k], a) <= dist2(px[k], b) {
						pat |= 1 << k
						sa[0] += px[k][0]
						sa[1] += px[k][1]
						sa[2] += px[k][2]
						na++
					} else {
						sb[0] += px[k][0]
						sb[1] += px[k][1]
						sb[2] += px[k][2]
						nb++
					}
				}
				if na > 0 {
					a = [3]int{sa[0] / na, sa[1] / na, sa[2] / na}
				}
				if nb > 0 {
					b = [3]int{sb[0] / nb, sb[1] / nb, sb[2] / nb}
				}
			}
			*cell = Cell{S: sextantTable[pat], FG: toRGB(a), BG: toRGB(b), W: 1}
		}
	}
}

// 4×4 Bayer matrix, scaled to ±0.5 of a 255 step.
var bayer4 = [4][4]int{{0, 8, 2, 10}, {12, 4, 14, 6}, {3, 11, 1, 9}, {15, 7, 13, 5}}

// CeefaxCells is SextantCells limited to the eight teletext colours:
// each pixel is ordered-dithered to the palette, then every cell keeps
// its two most common colours, exactly the constraint a 1980s page had.
func CeefaxCells(rgb []byte, cols, rows int, out []Cell) {
	pw := cols * 2
	for cy := range rows {
		for cx := range cols {
			var q [6]int
			var orig [6][3]int
			var count [8]int
			for k := range 6 {
				x, y := cx*2+k%2, cy*3+k/2
				o := (y*pw + x) * 3
				t := (bayer4[y&3][x&3]*16 + 8) - 128
				idx := 0
				for ch := range 3 {
					v := int(rgb[o+ch])
					orig[k][ch] = v
					if v+t/2 >= 128 {
						idx |= 1 << ch
					}
				}
				q[k] = idx
				count[idx]++
			}
			c1, c2 := -1, -1
			for i := range 8 {
				if count[i] == 0 {
					continue
				}
				if c1 < 0 || count[i] > count[c1] {
					c2 = c1
					c1 = i
				} else if c2 < 0 || count[i] > count[c2] {
					c2 = i
				}
			}
			cell := &out[cy*cols+cx]
			if c2 < 0 {
				c := palIdx(c1)
				*cell = Cell{S: " ", FG: c, BG: c, W: 1}
				continue
			}
			p1, p2 := palIdx(c1), palIdx(c2)
			a := [3]int{int(p1.R), int(p1.G), int(p1.B)}
			b := [3]int{int(p2.R), int(p2.G), int(p2.B)}
			pat := 0
			for k := range 6 {
				if q[k] == c1 || (q[k] != c2 && dist2(orig[k], a) <= dist2(orig[k], b)) {
					pat |= 1 << k
				}
			}
			*cell = Cell{S: sextantTable[pat], FG: p1, BG: p2, W: 1}
		}
	}
}

// palIdx maps a 3-bit RGB index (bit0 R, bit1 G, bit2 B) to the palette.
func palIdx(i int) RGB {
	var c RGB
	if i&1 != 0 {
		c.R = 255
	}
	if i&2 != 0 {
		c.G = 255
	}
	if i&4 != 0 {
		c.B = 255
	}
	return c
}

// HalfCells renders a cols×(rows*2) image with upper-half blocks.
func HalfCells(rgb []byte, cols, rows int, out []Cell) {
	for cy := range rows {
		for cx := range cols {
			o1 := ((cy*2)*cols + cx) * 3
			o2 := ((cy*2+1)*cols + cx) * 3
			top := RGB{rgb[o1], rgb[o1+1], rgb[o1+2]}
			bot := RGB{rgb[o2], rgb[o2+1], rgb[o2+2]}
			if top == bot {
				out[cy*cols+cx] = Cell{S: " ", FG: top, BG: bot, W: 1}
			} else {
				out[cy*cols+cx] = Cell{S: "▀", FG: top, BG: bot, W: 1}
			}
		}
	}
}

func (s *Screen) DrawCells(r Rect, cells []Cell) {
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			if i := y*r.W + x; i < len(cells) {
				s.Set(r.X+x, r.Y+y, cells[i])
			}
		}
	}
}
