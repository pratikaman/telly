package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSextantMapping(t *testing.T) {
	cases := map[int]rune{1: 0x1FB00, 2: 0x1FB01, 20: 0x1FB13, 22: 0x1FB14, 41: 0x1FB27, 43: 0x1FB28, 62: 0x1FB3B}
	for p, want := range cases {
		if got := []rune(Sextant(p))[0]; got != want {
			t.Errorf("Sextant(%d) = %U, want %U", p, got, want)
		}
	}
	if Sextant(21) != "▌" || Sextant(42) != "▐" || Sextant(63) != "█" || Sextant(0) != " " {
		t.Error("special sextants wrong")
	}
}

func TestParseKey(t *testing.T) {
	cases := []struct {
		in   string
		code KeyCode
		n    int
	}{
		{"\x1b[A", KUp, 3},
		{"\x1b[1;2C", KRight, 6},
		{"\x1b[5~", KPgUp, 4},
		{"\x1bOB", KDown, 3},
		{"\r", KEnter, 1},
		{"\x7f", KBackspace, 1},
		{"\x1b_Gi=1;OK\x1b\\x", KGfxReply, 11},
		{"\x1b]11;rgb:0/0/0\x07", KUnknown, 15},
		{"é", KRune, 2},
	}
	for _, c := range cases {
		k, n := parseKey([]byte(c.in))
		if k.Code != c.code || n != c.n {
			t.Errorf("parseKey(%q) = %v,%d want %v,%d", c.in, k.Code, n, c.code, c.n)
		}
	}
	if _, n := parseKey([]byte("\x1b[")); n != 0 {
		t.Error("incomplete CSI should wait")
	}
	if _, n := parseKey([]byte{0xe2, 0x82}); n != 0 {
		t.Error("incomplete UTF-8 should wait")
	}
}

func TestScreenDiff(t *testing.T) {
	s := NewScreen(20, 3)
	s.Clear()
	s.Text(0, 0, "hello", White, Black, -1)
	var b bytes.Buffer
	s.Render(&b)
	if !strings.Contains(b.String(), "hello") {
		t.Fatal("first render missing text")
	}
	s.Clear()
	s.Text(0, 0, "hellO", White, Black, -1)
	b.Reset()
	s.Render(&b)
	if strings.Contains(b.String(), "hell") || !strings.Contains(b.String(), "O") {
		t.Fatalf("diff should only redraw the changed cell, got %q", b.String())
	}
	s.Clear()
	s.Text(0, 1, "日本", White, Black, -1)
	b.Reset()
	s.Render(&b)
	if !strings.Contains(b.String(), "日") {
		t.Fatal("wide chars missing")
	}
}

func TestTextTruncate(t *testing.T) {
	s := NewScreen(10, 1)
	s.Clear()
	if n := s.Text(0, 0, "abcdefghijkl", White, Black, 6); n != 6 {
		t.Fatalf("width %d", n)
	}
	if s.cells[5].S != "…" {
		t.Fatalf("want ellipsis, got %q", s.cells[5].S)
	}
}

func TestMosaicEncoders(t *testing.T) {
	cols, rows := 3, 2
	img := make([]byte, cols*2*rows*3*3)
	for i := range img {
		img[i] = byte(i * 37)
	}
	cells := make([]Cell, cols*rows)
	SextantCells(img, cols, rows, cells)
	CeefaxCells(img, cols, rows, cells)
	for _, c := range cells {
		for _, ch := range []RGB{c.FG, c.BG} {
			ok := false
			for _, p := range Teletext {
				if p == ch {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("ceefax colour %v not in palette", ch)
			}
		}
	}
	half := make([]byte, cols*rows*2*3)
	HalfCells(half, cols, rows, cells)
}

func TestFitRect(t *testing.T) {
	r := fitRect(Rect{0, 1, 208, 47}, 16.0/9, 17, 39)
	if r.W > 208 || r.H > 47 || r.W < 100 {
		t.Fatalf("bad fit %+v", r)
	}
	got := float64(r.W) * 17 / (float64(r.H) * 39)
	if got < 1.65 || got > 1.9 {
		t.Fatalf("aspect %.2f", got)
	}
}

func TestLinkVideo(t *testing.T) {
	v, ok := linkVideo("https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10")
	if !ok || v.ID != "dQw4w9WgXcQ" {
		t.Fatalf("%+v", v)
	}
	v, ok = linkVideo("https://youtu.be/dQw4w9WgXcQ")
	if !ok || v.ID != "dQw4w9WgXcQ" {
		t.Fatalf("%+v", v)
	}
	if _, ok := linkVideo("lofi hip hop"); ok {
		t.Fatal("plain words are a search")
	}
}
