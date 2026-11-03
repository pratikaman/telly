package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type View int

const (
	ViewHome View = iota
	ViewResults
	ViewPlayer
	ViewHelp
	ViewCookies
)

type SearchDone struct {
	Gen     int
	Results []Video
	Err     error
}

type ResolveDone struct {
	Gen int
	St  *Streams
	Err error
}

type ThumbDone struct {
	Key string
	Pic *Pic
}

type resizeEvent struct{}

var suggestions = []string{
	"lofi hip hop radio",
	"tiny desk concert",
	"kurzgesagt",
	"nasa live",
	"synthwave mix",
	"bob ross",
	"jazz cafe ambience",
	"ceefax",
}

type thumbState struct {
	key  string
	rect Rect
}

type posterCache struct {
	key   string
	rect  Rect
	mode  RenderMode
	cells []Cell
}

type App struct {
	tty  *TTY
	gfx  *Gfx
	yt   *YT
	ff   string
	scr  *Screen
	size Size

	events chan any
	view   View
	mode   RenderMode
	quit   bool

	// home
	input     []rune
	homeFocus int // 0 search box, 1 history, 2 suggestions
	homeSel   int
	history   []Video

	// results
	query     string
	editing   bool
	results   []Video
	sel, top  int
	searching bool
	searchGen int
	searchErr string

	pageDigits   string
	pageDigitsAt time.Time

	// player
	now          *Video
	nowPage      int
	returnTo     View
	st           *Streams
	pl           *Player
	resolving    bool
	resolveGen   int
	resolveErr   string
	gotFrame     bool
	lastFrame    FrameEvent
	seekPending  bool
	seekTarget   float64
	seekAt       time.Time
	fullscreen   bool
	endRetries   int
	volume       float64
	finishedNote string
	startAt      float64

	loadStart time.Time
	status    string
	statusAt  time.Time
	helpFrom  View

	thumbs       map[string]*Pic
	thumbLoading map[string]bool
	thumbSem     chan struct{}
	wantThumb    thumbState
	shownThumb   thumbState
	poster       posterCache

	pre, post     strings.Builder
	resizePending bool
}

func NewApp(tty *TTY, gfx *Gfx, yt *YT, ff string, mode RenderMode) *App {
	a := &App{
		tty: tty, gfx: gfx, yt: yt, ff: ff, mode: mode,
		events:       make(chan any, 64),
		thumbs:       map[string]*Pic{},
		thumbLoading: map[string]bool{},
		thumbSem:     make(chan struct{}, 4),
		volume:       1,
	}
	if a.mode == ModeHD && gfx.Mode == GfxNone {
		a.mode = ModeMosaic
	}
	a.history = loadHistory()
	return a
}

func (a *App) Run(initial string) {
	a.tty.Enter()
	a.size = a.tty.Size()
	a.scr = NewScreen(a.size.Cols, a.size.Rows)
	if initial != "" {
		if v, ok := linkVideo(initial); ok {
			a.returnTo = ViewHome
			a.play(v, 101)
		} else {
			a.search(initial)
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !a.quit {
		a.draw()
		select {
		case k, ok := <-a.tty.Keys:
			if !ok {
				a.quit = true
				break
			}
			a.onKey(k)
		case <-a.tty.Resize:
			if !a.resizePending {
				a.resizePending = true
				time.AfterFunc(120*time.Millisecond, func() { a.events <- resizeEvent{} })
			}
		case ev := <-a.events:
			a.onEvent(ev)
		case <-ticker.C:
			a.onTick()
		}
	}
	a.stopPlayer()
}

// linkVideo recognises YouTube links, bare video ids (with -v), other URLs and files.
func linkVideo(s string) (Video, bool) {
	s = strings.TrimSpace(s)
	if fileExists(s) {
		abs, _ := filepath.Abs(s)
		return Video{URL: abs, Title: filepath.Base(s)}, true
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return Video{}, false
	}
	for _, marker := range []string{"v=", "youtu.be/", "/shorts/", "/live/", "/embed/"} {
		if i := strings.Index(s, marker); i >= 0 {
			id := s[i+len(marker):]
			if len(id) >= 11 && ytIDRe.MatchString(id[:11]) {
				return Video{ID: id[:11], Title: "YouTube " + id[:11]}, true
			}
		}
	}
	return Video{URL: s, Title: s}, true
}

// --- drawing -----------------------------------------------------------------

func (a *App) draw() {
	s := a.scr
	s.Clear()
	a.wantThumb = thumbState{}
	switch a.view {
	case ViewHome:
		a.drawHome()
	case ViewResults:
		a.drawResults()
	case ViewPlayer:
		a.drawPlayer()
	case ViewHelp:
		a.drawHelp()
	case ViewCookies:
		a.drawCookies()
	}
	var buf bytes.Buffer
	buf.WriteString(a.pre.String())
	a.pre.Reset()
	s.Render(&buf)
	a.syncThumb(&buf)
	buf.WriteString(a.post.String())
	a.post.Reset()
	if buf.Len() > 0 {
		a.tty.WriteBytes(buf.Bytes())
	}
}

func (a *App) syncThumb(buf *bytes.Buffer) {
	if a.wantThumb == a.shownThumb {
		return
	}
	if a.shownThumb.key != "" {
		buf.WriteString(gfxDelete(50))
	}
	a.shownThumb = thumbState{}
	if a.wantThumb.key == "" {
		return
	}
	pic := a.thumbs[a.wantThumb.key]
	if pic == nil {
		return
	}
	cmd, _, err := a.gfx.Place(50, pic.RGB, pic.W, pic.H, a.wantThumb.rect, false)
	if err != nil {
		return
	}
	buf.WriteString(cmd)
	a.shownThumb = a.wantThumb
}

func (a *App) loading() bool {
	return a.searching || (a.view == ViewPlayer && (a.resolving || (a.pl != nil && !a.gotFrame)))
}

// header is the teletext row 0: "P100  TELLY 1 100 Tue 03 Nov 14:22/07".
// While a page is on its way the middle page counter rolls, like a TV
// cycling through the carousel until your page comes round.
func (a *App) header(page int) {
	s := a.scr
	now := time.Now()
	pg := fmt.Sprintf("P%d", page)
	if a.pageDigits != "" {
		pg = "P" + a.pageDigits + strings.Repeat("-", 3-len(a.pageDigits))
	}
	s.Text(1, 0, pg, White, Black, -1)
	roll := page
	if a.loading() {
		roll = 100 + int(time.Since(a.loadStart)/(40*time.Millisecond))%800
	}
	clock := now.Format("15:04/05")
	mid := fmt.Sprintf("TELLY 1 %03d %s", roll, now.Format("Mon 02 Jan"))
	xc := s.W - 1 - len(clock)
	s.Text(xc, 0, clock, Yellow, Black, -1)
	if xm := xc - 1 - len(mid); xm > len(pg)+2 {
		s.Text(xm, 0, mid, White, Black, -1)
	}
}

func (a *App) showMast() bool { return a.scr.H >= 26 && a.scr.W >= 48 }

// masthead draws the three white boxed letters and the blue band, the way
// the BBC/CEEFAX masthead sits at the top of P100. Three rows tall.
func (a *App) masthead(y int) {
	s := a.scr
	x := 1
	if s.W >= 72 {
		for _, ch := range "YOU" {
			b := NewBitmap(14, 9)
			b.FillRect(0, 0, 14, 9, true)
			stampWide(b, 1, 1, fontLogo[ch], false)
			s.DrawBitmap(x, y, b, White, Black)
			x += 8
		}
	}
	bandW := s.W - 1 - x
	b := NewBitmap(bandW*2, 9)
	word := "TELLY"
	ww := len(word)*16 - 4
	ox := (bandW*2 - ww) / 2
	for i, ch := range word {
		stampWide(b, ox+i*16, 1, fontLogo[ch], true)
	}
	s.DrawBitmap(x, y, b, Yellow, Blue)
}

// stampWide draws a glyph with doubled pixel width (chunky masthead letters).
func stampWide(b *Bitmap, x, y int, rows []string, v bool) {
	for gy, row := range rows {
		for gx, ch := range row {
			if ch == '#' {
				b.Set(x+gx*2, y+gy, v)
				b.Set(x+gx*2+1, y+gy, v)
			}
		}
	}
}

// titleBlock: a small coloured section label, a double-height headline with
// the page number on the right, and a blue rule. Returns the next free row.
func (a *App) titleBlock(y int, label string, labelFG RGB, headline string, page int) int {
	s := a.scr
	if label != "" {
		s.Text(1, y, label, labelFG, Black, s.W-2)
	}
	pb := BigText(strconv.Itoa(page), 12)
	pw := pb.W / 2
	s.DrawBitmap(s.W-1-pw, y+1, pb, Yellow, Black)
	hb := BigText(headline, s.W-4-pw)
	if hb.W > 0 && strings.TrimSpace(headline) != "" {
		s.DrawBitmap(1, y+1, hb, White, Black)
	} else {
		s.Text(1, y+1, headline, White, Black, s.W-4-pw)
	}
	for x := 1; x < s.W-1; x++ {
		s.Set(x, y+3, Cell{S: "▂", FG: Blue, BG: Black, W: 1})
	}
	return y + 4
}

func (a *App) banner(text string, fg RGB) {
	s := a.scr
	y := s.H - 2
	s.Fill(1, y, s.W-2, 1, Blue)
	s.TextCenter(1, y, s.W-2, text, fg, Blue)
}

type fastItem struct {
	key, label string
	fg         RGB
}

// fastext: the four coloured shortcuts along the bottom of every page.
func (a *App) fastext(items [4]fastItem) {
	s := a.scr
	y := s.H - 1
	slot := (s.W - 2) / 4
	for i, it := range items {
		x := 1 + i*slot
		if it.key != "" {
			x += s.Text(x, y, it.key, White, Black, slot) + 1
		}
		s.Text(x, y, it.label, it.fg, Black, slot-StrWidth(it.key)-1)
	}
}

func (a *App) statusLine() string {
	if a.status != "" && time.Since(a.statusAt) < 4*time.Second {
		return a.status
	}
	return ""
}

func (a *App) setStatus(f string, args ...any) {
	a.status = fmt.Sprintf(f, args...)
	a.statusAt = time.Now()
}

// indexEntry draws "TITLE ............ 201" the way Ceefax lists pages.
func (a *App) indexEntry(x, y, w int, title string, page int, selected bool) {
	s := a.scr
	bg := Black
	if selected {
		bg = Blue
		s.Fill(x, y, w, 1, Blue)
	}
	num := strconv.Itoa(page)
	s.Text(x+w-len(num)-1, y, num, White, bg, -1)
	s.Text(x+1, y, title, Yellow, bg, w-len(num)-3)
}

func (a *App) drawHome() {
	s := a.scr
	a.header(100)
	y := 1
	if a.showMast() {
		a.masthead(1)
		y = 5
	}
	y = a.titleBlock(y, "Welcome to telly", Cyan, "WHAT'S ON?", 100)
	y++
	// search box
	s.Text(1, y, "SEARCH", Cyan, Black, -1)
	bx := 8
	s.Fill(bx, y, s.W-1-bx, 1, Blue)
	txt := string(a.input)
	room := s.W - 3 - bx
	if w := StrWidth(txt); w > room-1 {
		r := []rune(txt)
		for StrWidth(string(r)) > room-1 && len(r) > 0 {
			r = r[1:]
		}
		txt = string(r)
	}
	n := s.Text(bx+1, y, txt, Yellow, Blue, room)
	if a.homeFocus == 0 && time.Now().UnixMilli()/500%2 == 0 {
		s.Set(bx+1+n, y, Cell{S: "█", FG: White, BG: Blue, W: 1})
	}
	if len(a.input) == 0 && a.homeFocus != 0 {
		s.Text(bx+1, y, "type to search YouTube", Cyan, Blue, room)
	} else if len(a.input) == 0 {
		s.Text(bx+3, y, "type to search YouTube, ENTER to go", Cyan, Blue, room-2)
	}
	y += 2

	colW := (s.W - 3) / 2
	lx, rx := 1, 2+colW
	listH := s.H - 3 - (y + 1)
	s.Text(lx+1, y, "RECENTLY WATCHED", Cyan, Black, -1)
	s.Text(rx+1, y, "TUNE IN", Cyan, Black, -1)
	y++
	if len(a.history) == 0 {
		s.Text(lx+1, y, "Nothing yet. Pick a channel →", White, Black, colW-2)
	}
	// Ceefax index pages breathe: one blank row between entries when it fits.
	step := 1
	if listH >= 2*len(suggestions) {
		step = 2
	}
	for i := 0; i < len(a.history) && i*step < listH; i++ {
		a.indexEntry(lx, y+i*step, colW, a.history[i].Title, 201+i, a.homeFocus == 1 && a.homeSel == i)
	}
	for i := 0; i < len(suggestions) && i*step < listH; i++ {
		a.indexEntry(rx, y+i*step, colW, strings.ToUpper(suggestions[i]), 301+i, a.homeFocus == 2 && a.homeSel == i)
	}
	if st := a.statusLine(); st != "" {
		a.banner(st, White)
	} else if a.finishedNote != "" {
		a.banner(a.finishedNote, Yellow)
	} else {
		a.banner("telly: YouTube at your fingertips", Yellow)
	}
	a.fastext([4]fastItem{
		{"ENTER", "Search", Red},
		{"TAB", "Lists", Green},
		{"?", "Help", Yellow},
		{"^C", "Quit", Cyan},
	})
}

func fmtDur(sec float64) string {
	if sec <= 0 {
		return ""
	}
	t := int(sec + 0.5)
	h, m, s := t/3600, t/60%60, t%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func fmtViews(n int64, live bool) string {
	var s string
	switch {
	case n >= 1_000_000_000:
		s = fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		s = fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		s = fmt.Sprintf("%.1fK", float64(n)/1e3)
	case n > 0:
		s = strconv.FormatInt(n, 10)
	default:
		return ""
	}
	s = strings.Replace(s, ".0", "", 1)
	if live {
		return s + " watching"
	}
	return s + " views"
}

func (a *App) listGeometry() (top, rows, listW int, preview bool) {
	y := 1
	if a.showMast() {
		y = 5
	}
	top = y + 4
	rows = (a.scr.H - 3 - top) / 2
	listW = a.scr.W - 2
	if a.scr.W >= 90 {
		listW = a.scr.W * 58 / 100
		preview = true
	}
	return
}

func (a *App) drawResults() {
	s := a.scr
	page := 101 + a.sel
	a.header(page)
	y := 1
	if a.showMast() {
		a.masthead(1)
		y = 5
	}
	label := "Search results"
	if len(a.results) > 0 {
		label = fmt.Sprintf("Search results  %d of %d", a.sel+1, len(a.results))
	}
	y = a.titleBlock(y, label, Yellow, a.query, page)
	if a.editing {
		ly := y - 4
		s.Fill(0, ly, s.W, 1, Black)
		s.Text(1, ly, "SEARCH", Cyan, Black, -1)
		s.Fill(8, ly, s.W-9, 1, Blue)
		n := s.Text(9, ly, string(a.input), Yellow, Blue, s.W-12)
		if time.Now().UnixMilli()/500%2 == 0 {
			s.Set(9+n, ly, Cell{S: "█", FG: White, BG: Blue, W: 1})
		}
	}
	top, rows, listW, preview := a.listGeometry()
	switch {
	case a.searching:
		s.Text(2, top+1, "Searching YouTube for \""+a.query+"\"…", Cyan, Black, s.W-4)
	case a.searchErr != "":
		s.Text(2, top+1, "PAGE NOT AVAILABLE", Red, Black, -1)
		s.Text(2, top+2, a.searchErr, White, Black, s.W-4)
	case len(a.results) == 0:
		s.Text(2, top+1, "No results. Press / to search again.", White, Black, -1)
	}
	if a.sel < a.top {
		a.top = a.sel
	}
	if rows > 0 && a.sel >= a.top+rows {
		a.top = a.sel - rows + 1
	}
	for i := a.top; i < len(a.results) && i < a.top+rows; i++ {
		v := a.results[i]
		ly := top + (i-a.top)*2
		bg := Black
		if i == a.sel {
			bg = Blue
			s.Fill(1, ly, listW, 2, Blue)
		}
		s.Text(2, ly, strconv.Itoa(101+i), White, bg, -1)
		dur, durFG := fmtDur(v.Duration), White
		if v.Live {
			dur, durFG = "● LIVE", Red
			if bg == Blue {
				durFG = White
			}
		}
		dw := StrWidth(dur)
		s.Text(listW-dw, ly, dur, durFG, bg, -1)
		s.Text(7, ly, v.Title, Yellow, bg, listW-dw-8)
		x := 7 + s.Text(7, ly+1, v.Channel, Cyan, bg, listW-24)
		if vw := fmtViews(v.Views, v.Live); vw != "" {
			s.Text(x+2, ly+1, vw, Green, bg, listW-x-3)
		}
	}
	if preview && len(a.results) > 0 && a.sel < len(a.results) {
		a.drawPreview(Rect{listW + 3, top, s.W - listW - 5, s.H - 3 - top}, a.results[a.sel])
	}
	if st := a.statusLine(); st != "" {
		a.banner(st, White)
	} else if a.finishedNote != "" {
		a.banner(a.finishedNote, Yellow)
	} else {
		a.banner("▲▼ choose · ENTER play · type a page number to jump", Yellow)
	}
	a.fastext([4]fastItem{
		{"/", "Search", Red},
		{"m", "Picture: " + a.effectiveMode().String(), Green},
		{"?", "Help", Yellow},
		{"q", "Back", Cyan},
	})
}

func (a *App) thumbRect(area Rect) Rect {
	h := int(math.Round(float64(area.W) * a.size.CellW() * 9 / 16 / a.size.CellH()))
	w := area.W
	if maxH := area.H * 6 / 10; h > maxH {
		h = maxH
		w = int(math.Round(float64(h) * a.size.CellH() * 16 / 9 / a.size.CellW()))
	}
	return Rect{area.X, area.Y, w, h}
}

func (a *App) drawPreview(area Rect, v Video) {
	s := a.scr
	tr := a.thumbRect(area)
	a.drawPicture(tr, v.Key())
	y := tr.Y + tr.H + 1
	for _, line := range wrap(v.Title, area.W, 3) {
		s.Text(area.X, y, line, White, Black, area.W)
		y++
	}
	y++
	s.Text(area.X, y, v.Channel, Cyan, Black, area.W)
	y++
	if vw := fmtViews(v.Views, v.Live); vw != "" {
		s.Text(area.X, y, vw, Green, Black, area.W)
		y++
	}
	if v.Live {
		s.Text(area.X, y, "● LIVE NOW", Red, Black, -1)
	} else if d := fmtDur(v.Duration); d != "" {
		s.Text(area.X, y, "Running time "+d, Yellow, Black, area.W)
	}
}

// drawPicture shows a thumbnail: real pixels via kitty graphics in HD mode,
// otherwise a mosaic in the current picture style.
func (a *App) drawPicture(r Rect, key string) {
	s := a.scr
	pic := a.thumbs[key]
	if pic == nil {
		s.Fill(r.X, r.Y, r.W, r.H, Black)
		s.TextCenter(r.X, r.Y+r.H/2, r.W, "· · ·", Blue, Black)
		return
	}
	mode := a.effectiveMode()
	if mode == ModeHD {
		a.wantThumb = thumbState{key, r}
		return
	}
	if a.poster.key != key || a.poster.rect != r || a.poster.mode != mode {
		cells := make([]Cell, r.W*r.H)
		switch mode {
		case ModeBlocks:
			HalfCells(pic.Resize(r.W, r.H*2).RGB, r.W, r.H, cells)
		case ModeCeefax:
			CeefaxCells(pic.Resize(r.W*2, r.H*3).RGB, r.W, r.H, cells)
		default:
			SextantCells(pic.Resize(r.W*2, r.H*3).RGB, r.W, r.H, cells)
		}
		a.poster = posterCache{key, r, mode, cells}
	}
	s.DrawCells(r, a.poster.cells)
}

func wrap(text string, w, maxLines int) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(text) {
		cand := word
		if cur != "" {
			cand = cur + " " + word
		}
		if StrWidth(cand) <= w || cur == "" {
			cur = cand
			continue
		}
		lines = append(lines, cur)
		cur = word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] += " …"
	}
	return lines
}

func (a *App) effectiveMode() RenderMode {
	if a.mode == ModeHD && a.gfx.Mode == GfxNone {
		return ModeMosaic
	}
	return a.mode
}

func fitRect(area Rect, aspect, cw, ch float64) Rect {
	if area.W <= 0 || area.H <= 0 {
		return Rect{}
	}
	w := area.W
	h := int(math.Round(float64(w) * cw / aspect / ch))
	if h > area.H {
		h = area.H
		w = int(math.Round(float64(h) * ch * aspect / cw))
	}
	w = max(1, min(w, area.W))
	h = max(1, h)
	return Rect{area.X + (area.W-w)/2, area.Y + (area.H-h)/2, w, h}
}

func (a *App) videoRect() Rect {
	top, bottom := 1, a.scr.H-4
	if a.fullscreen {
		top, bottom = 0, a.scr.H
	}
	aspect := 16.0 / 9
	if a.st != nil && a.st.W > 0 && a.st.H > 0 {
		aspect = float64(a.st.W) / float64(a.st.H)
	}
	return fitRect(Rect{0, top, a.scr.W, bottom - top}, aspect, a.size.CellW(), a.size.CellH())
}

func (a *App) playerLayout() Layout {
	r := a.videoRect()
	m := a.effectiveMode()
	lay := Layout{Mode: m, Rect: r, FPS: 30}
	if a.st != nil && a.st.FPS > 0 && a.st.FPS < 30 {
		lay.FPS = a.st.FPS
	}
	switch m {
	case ModeHD:
		maxW := 1280
		if a.gfx.Mode == GfxDirect {
			maxW = 640
		}
		if v, err := strconv.Atoi(os.Getenv("TELLY_MAXW")); err == nil && v > 0 {
			maxW = v
		}
		w := min(int(float64(r.W)*a.size.CellW()), maxW)
		if a.st != nil && a.st.W > 0 {
			w = min(w, a.st.W)
		}
		aspect := float64(r.W) * a.size.CellW() / (float64(r.H) * a.size.CellH())
		lay.DecW = max(2, w&^1)
		lay.DecH = max(2, int(math.Round(float64(w)/aspect))&^1)
	case ModeMosaic, ModeCeefax:
		lay.DecW, lay.DecH = r.W*2, r.H*3
	case ModeBlocks:
		lay.DecW, lay.DecH = r.W, r.H*2
	}
	return lay
}

func fmtClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	t := int(sec)
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%02d:%02d", t/60, t%60)
}

func (a *App) drawPlayer() {
	s := a.scr
	if !a.fullscreen {
		a.header(a.nowPage)
	}
	r := a.videoRect()
	if a.pl != nil && a.lastFrame.Cells != nil && a.lastFrame.Gen == a.pl.Gen() {
		s.DrawCells(a.lastFrame.Rect, a.lastFrame.Cells)
	} else if !a.gotFrame && a.now != nil {
		a.drawPicture(r, a.now.Key())
	}
	if a.fullscreen {
		return
	}
	W, H := s.W, s.H
	title, channel := "", ""
	if a.now != nil {
		title, channel = a.now.Title, a.now.Channel
	}
	if a.st != nil {
		if a.st.Title != "" {
			title = a.st.Title
		}
		if a.st.Channel != "" {
			channel = a.st.Channel
		}
	}
	cw := StrWidth(channel)
	s.TextRight(W-1, H-4, channel, Cyan, Black)
	s.Text(1, H-4, title, Yellow, Black, W-4-cw)

	// progress: a mosaic bar, red for watched, blue for to come
	pos := 0.0
	if a.pl != nil {
		pos = a.pl.Clock()
	}
	if a.seekPending {
		pos = a.seekTarget
	}
	y := H - 3
	if a.st != nil && a.st.Live {
		s.Text(1, y, "● LIVE", Red, Black, -1)
		s.Text(9, y, "on air for "+fmtClock(pos), White, Black, -1)
	} else {
		dur := 0.0
		if a.st != nil {
			dur = a.st.Duration
		} else if a.now != nil {
			dur = a.now.Duration
		}
		cur, total := fmtClock(pos), fmtClock(dur)
		s.Text(1, y, cur, White, Black, -1)
		s.TextRight(W-1, y, total, White, Black)
		bx := 2 + len(cur)
		bw := W - 2 - len(total) - bx
		frac := 0.0
		if dur > 0 {
			frac = math.Max(0, math.Min(1, pos/dur))
		}
		half := int(frac * float64(bw*2))
		for i := 0; i < bw; i++ {
			c := Cell{S: Sextant(12), FG: Blue, BG: Black, W: 1}
			switch {
			case half >= (i+1)*2:
				c.FG = Red
			case half == i*2+1:
				c = Cell{S: Sextant(4), FG: Red, BG: Black, W: 1}
			}
			s.Set(bx+i, y, c)
		}
	}

	state := "▶ PLAYING"
	switch {
	case a.resolving:
		state = "TUNING IN…"
	case a.resolveErr != "":
		state = "PAGE NOT AVAILABLE"
	case a.pl != nil && a.pl.Paused():
		state = "❚❚ PAUSED"
	case a.pl != nil && !a.gotFrame && a.st != nil && a.st.Video != "":
		state = "BUFFERING…"
	case a.st != nil && a.st.Video == "":
		state = "♪ SOUND ONLY"
	}
	if a.seekPending {
		state = "» " + fmtClock(a.seekTarget)
	}
	info := fmt.Sprintf("%s   VOL %d%%   PICTURE %s", state, int(math.Round(a.volume*100)), a.effectiveMode())
	if a.st != nil && a.st.H > 0 {
		info += fmt.Sprintf(" %dp", a.st.H)
	}
	if a.effectiveMode() == ModeHD {
		info += " (" + a.gfx.Mode.String() + ")"
	}
	if st := a.statusLine(); st != "" {
		a.banner(st, White)
	} else if a.resolveErr != "" {
		a.banner(a.resolveErr, White)
	} else {
		a.banner(info, Yellow)
	}
	pause := "Pause"
	if a.pl != nil && a.pl.Paused() {
		pause = "Play"
	}
	a.fastext([4]fastItem{
		{"SPACE", pause, Red},
		{"←→", "Seek", Green},
		{"m", "Picture", Yellow},
		{"q", "Back", Cyan},
	})
}

var helpEntries = [][2]string{
	{"SEARCH", "/"},
	{"PLAY", "ENTER"},
	{"PAUSE / PLAY", "SPACE"},
	{"SEEK 5 SECONDS", "← →"},
	{"SEEK 10 SECONDS", "J L"},
	{"SEEK 1 MINUTE", "PGUP PGDN"},
	{"JUMP TO 0-90%", "0 - 9"},
	{"VOLUME", "↑ ↓"},
	{"PICTURE STYLE", "M"},
	{"VIDEO QUALITY", "V"},
	{"FULL SCREEN", "F"},
	{"NEXT / PREVIOUS", "N P"},
	{"GO TO PAGE", "3 DIGITS"},
	{"BACK", "Q  ESC"},
	{"QUIT", "CTRL-C"},
}

func (a *App) drawHelp() {
	s := a.scr
	a.header(199)
	y := 1
	if a.showMast() {
		a.masthead(1)
		y = 5
	}
	y = a.titleBlock(y, "A-Z Index", Yellow, "HOW TO WATCH", 199)
	colW := (s.W - 3) / 2
	half := (len(helpEntries) + 1) / 2
	for i, e := range helpEntries {
		x, yy := 1, y+i
		if i >= half {
			x, yy = 2+colW, y+i-half
		}
		s.Text(x+1, yy, e[0], Yellow, Black, colW-14)
		s.TextRight(x+colW-1, yy, e[1], White, Black)
	}
	y += half + 1
	s.Text(2, y, "PICTURE STYLES", Cyan, Black, -1)
	y++
	modes := [][2]string{
		{"HD", "real pixels through the kitty graphics protocol (" + a.gfx.Mode.String() + ")"},
		{"MOSAIC", "teletext 2×3 sextant mosaics in full colour"},
		{"BLOCKS", "half-block pixels, works in nearly any terminal"},
		{"CEEFAX", "sextants in the eight broadcast colours of 1985"},
	}
	for _, m := range modes {
		if y >= s.H-3 {
			break
		}
		s.Text(3, y, m[0], Yellow, Black, -1)
		s.Text(11, y, m[1], White, Black, s.W-13)
		y++
	}
	if y < s.H-3 {
		s.Text(2, y+1, "Pages: 100 home · 101+ results · 2xx recently watched · 3xx tune in", Green, Black, s.W-4)
	}
	a.banner("Press any key to go back", Yellow)
	a.fastext([4]fastItem{{"", "Index", Red}, {"", "Search", Green}, {"", "Watch", Yellow}, {"", "Help", Cyan}})
}

func (a *App) drawCookies() {
	s := a.scr
	a.header(403)
	y := 1
	if a.showMast() {
		a.masthead(1)
		y = 5
	}
	y = a.titleBlock(y, "YouTube wants a signed-in session", Red, "SIGN IN NEEDED", 403)
	lines := []struct {
		t  string
		fg RGB
	}{
		{"Your connection reaches YouTube from a datacenter address (a VPN or company network),", White},
		{"so YouTube asks for a signed-in session before it hands out the video stream.", White},
		{"", White},
		{"1  Let telly borrow Chrome's YouTube sign-in   (recommended)", Yellow},
		{"   System Settings › Privacy & Security › Full Disk Access › switch on cmux (or Ghostty).", White},
		{"   Then press C here. macOS asks once for \"Chrome Safe Storage\": choose Always Allow.", White},
		{"", White},
		{"2  Or export a cookies.txt from YouTube (e.g. the Get cookies.txt LOCALLY extension)", Yellow},
		{"   save it as ~/.config/telly/cookies.txt and press R.", White},
		{"", White},
		{"3  Or switch off the VPN and press R.", Yellow},
	}
	for _, l := range lines {
		if y >= s.H-3 {
			break
		}
		s.Text(2, y, l.t, l.fg, Black, s.W-4)
		y++
	}
	if y+1 < s.H-3 && a.resolveErr != "" {
		s.Text(2, y+1, a.resolveErr, Red, Black, s.W-4)
	}
	a.banner("C retry with Chrome · R retry · Q back", Yellow)
	a.fastext([4]fastItem{{"C", "Chrome", Red}, {"R", "Retry", Green}, {"?", "Help", Yellow}, {"q", "Back", Cyan}})
}

// --- actions -------------------------------------------------------------------

func (a *App) search(q string) {
	q = strings.TrimSpace(q)
	if q == "" {
		return
	}
	a.query = q
	a.view = ViewResults
	a.editing = false
	a.searching = true
	a.searchErr = ""
	a.results = nil
	a.sel, a.top = 0, 0
	a.loadStart = time.Now()
	a.searchGen++
	gen := a.searchGen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		vs, err := a.yt.Search(ctx, q, 30)
		a.events <- SearchDone{gen, vs, err}
	}()
}

func (a *App) fetchThumb(v Video) {
	key := v.Key()
	if v.ID == "" || a.thumbs[key] != nil || a.thumbLoading[key] {
		return
	}
	a.thumbLoading[key] = true
	go func() {
		a.thumbSem <- struct{}{}
		defer func() { <-a.thumbSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pic, _ := FetchThumb(ctx, v.ID)
		a.events <- ThumbDone{key, pic}
	}()
}

func (a *App) play(v Video, page int) {
	a.stopPlayer()
	if a.view == ViewHome || a.view == ViewResults {
		a.returnTo = a.view
	}
	a.now = &v
	a.nowPage = page
	a.view = ViewPlayer
	a.st = nil
	a.startAt = 0
	a.resolving = true
	a.resolveErr = ""
	a.gotFrame = false
	a.lastFrame = FrameEvent{}
	a.endRetries = 0
	a.fullscreen = false
	a.seekPending = false
	a.finishedNote = ""
	a.loadStart = time.Now()
	a.fetchThumb(v)
	a.pushHistory(v)
	a.resolveGen++
	gen := a.resolveGen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		st, err := a.yt.Resolve(ctx, v)
		a.events <- ResolveDone{gen, st, err}
	}()
}

func (a *App) stopPlayer() {
	if a.pl != nil {
		a.pl.Close()
		a.pl = nil
	}
	a.pre.WriteString(gfxDelete(1) + gfxDelete(2))
	a.lastFrame = FrameEvent{}
}

func (a *App) leavePlayer(note string) {
	a.stopPlayer()
	a.resolveGen++
	a.resolving = false
	a.view = a.returnTo
	a.finishedNote = note
}

func (a *App) startPlayback() {
	lay := a.playerLayout()
	a.pl = NewPlayer(a.ff, a.st, a.gfx, a.events, lay, a.volume)
	a.pl.Start(a.startAt)
	a.startAt = 0
}

func (a *App) commitSeek() {
	if !a.seekPending || a.pl == nil {
		return
	}
	a.seekPending = false
	a.endRetries = 0
	a.pl.Start(a.seekTarget)
}

func (a *App) seekBy(d float64) {
	if a.pl == nil || a.st == nil || a.st.Live {
		return
	}
	base := a.pl.Clock()
	if a.seekPending {
		base = a.seekTarget
	}
	a.seekTo(base + d)
}

func (a *App) seekTo(t float64) {
	if a.pl == nil || a.st == nil || a.st.Live {
		return
	}
	dur := a.st.Duration
	if dur > 0 {
		t = math.Min(t, dur-1)
	}
	a.seekTarget = math.Max(0, t)
	a.seekPending = true
	a.seekAt = time.Now()
}

func (a *App) cycleMode() {
	for {
		a.mode = (a.mode + 1) % 4
		if a.mode != ModeHD || a.gfx.Mode != GfxNone {
			break
		}
	}
	saveConfigKey("mode", strings.ToLower(a.mode.String()))
	a.pre.WriteString(gfxDelete(1) + gfxDelete(2))
	a.lastFrame = FrameEvent{}
	a.gotFrame = false
	if a.pl != nil {
		a.pl.SetLayout(a.playerLayout())
	}
	a.setStatus("Picture: %s", a.effectiveMode())
}

// cycleQuality steps 360p → 480p → 720p → 1080p and re-tunes the current
// video at the same position.
func (a *App) cycleQuality() {
	cur := a.yt.Quality()
	next := Qualities[0]
	for i, q := range Qualities {
		if q == cur {
			next = Qualities[(i+1)%len(Qualities)]
		}
	}
	a.yt.SetQuality(next)
	saveConfigKey("quality", strconv.Itoa(next))
	if a.view == ViewPlayer && a.now != nil && a.now.ID != "" && a.st != nil && a.pl != nil {
		pos, live := a.pl.Clock(), a.st.Live
		a.play(*a.now, a.nowPage)
		if !live {
			a.startAt = pos
		}
	}
	a.setStatus("Quality: up to %dp", next)
}

func (a *App) playIndex(i int) {
	if i < 0 || i >= len(a.results) {
		return
	}
	a.sel = i
	a.play(a.results[i], 101+i)
}

func (a *App) gotoPage(n int) {
	switch {
	case n == 100:
		a.stopPlayer()
		a.view = ViewHome
	case n == 199:
		a.openHelp()
	case n >= 101 && n < 101+len(a.results):
		a.playIndex(n - 101)
	case n >= 201 && n < 201+len(a.history):
		a.play(a.history[n-201], n)
	case n >= 301 && n < 301+len(suggestions):
		a.search(suggestions[n-301])
	default:
		a.setStatus("P%d not found: try 100, 101-%d or 199", n, 100+max(1, len(a.results)))
	}
}

func (a *App) openHelp() {
	if a.view != ViewHelp {
		a.helpFrom = a.view
	}
	a.view = ViewHelp
}

// --- input ---------------------------------------------------------------------

func (a *App) onKey(k Key) {
	if k.Code == KCtrlC {
		a.quit = true
		return
	}
	if k.Code == KCtrlL {
		a.scr.Invalidate()
		a.pre.WriteString("\x1b[2J")
		a.shownThumb = thumbState{}
		return
	}
	switch a.view {
	case ViewHome:
		a.keyHome(k)
	case ViewResults:
		a.keyResults(k)
	case ViewPlayer:
		a.keyPlayer(k)
	case ViewHelp:
		a.view = a.helpFrom
		if a.view == ViewPlayer && a.pl == nil {
			a.view = ViewHome
		}
	case ViewCookies:
		a.keyCookies(k)
	}
}

// pageKey handles teletext page entry: three digits jump to a page.
func (a *App) pageKey(k Key) bool {
	if k.Code != KRune || k.Rune < '0' || k.Rune > '9' {
		return false
	}
	if a.pageDigits == "" && k.Rune == '0' {
		return true
	}
	a.pageDigits += string(k.Rune)
	a.pageDigitsAt = time.Now()
	if len(a.pageDigits) == 3 {
		n, _ := strconv.Atoi(a.pageDigits)
		a.pageDigits = ""
		a.gotoPage(n)
	}
	return true
}

// editKey applies line editing to a.input; returns true if consumed.
func (a *App) editKey(k Key) bool {
	switch k.Code {
	case KRune:
		if unicode.IsPrint(k.Rune) {
			a.input = append(a.input, k.Rune)
			return true
		}
	case KBackspace:
		if len(a.input) > 0 {
			a.input = a.input[:len(a.input)-1]
		}
		return true
	case KCtrlU:
		a.input = nil
		return true
	case KCtrlW:
		s := strings.TrimRight(string(a.input), " ")
		if i := strings.LastIndex(s, " "); i >= 0 {
			a.input = []rune(s[:i+1])
		} else {
			a.input = nil
		}
		return true
	}
	return false
}

func (a *App) keyHome(k Key) {
	if a.homeFocus == 0 {
		switch k.Code {
		case KEnter:
			if q := strings.TrimSpace(string(a.input)); q != "" {
				a.search(q)
			}
			return
		case KDown, KTab:
			a.homeFocus, a.homeSel = 1, 0
			if len(a.history) == 0 {
				a.homeFocus = 2
			}
			return
		case KEsc:
			a.input = nil
			return
		}
		if k.Code == KRune && k.Rune == '?' && len(a.input) == 0 {
			a.openHelp()
			return
		}
		a.editKey(k)
		return
	}
	n := len(a.history)
	if a.homeFocus == 2 {
		n = len(suggestions)
	}
	if a.pageKey(k) {
		return
	}
	switch k.Code {
	case KUp:
		if a.homeSel == 0 {
			a.homeFocus = 0
		} else {
			a.homeSel--
		}
	case KDown:
		if a.homeSel < n-1 {
			a.homeSel++
		}
	case KLeft, KRight, KTab:
		if a.homeFocus == 1 {
			a.homeFocus = 2
		} else if len(a.history) > 0 {
			a.homeFocus = 1
		} else if k.Code == KTab {
			a.homeFocus = 0
		}
		if a.homeFocus == 1 {
			a.homeSel = min(a.homeSel, len(a.history)-1)
		} else if a.homeFocus == 2 {
			a.homeSel = min(a.homeSel, len(suggestions)-1)
		}
	case KEnter:
		if a.homeFocus == 1 && a.homeSel < len(a.history) {
			a.play(a.history[a.homeSel], 201+a.homeSel)
		} else if a.homeFocus == 2 && a.homeSel < len(suggestions) {
			a.search(suggestions[a.homeSel])
		}
	case KEsc:
		a.homeFocus = 0
	case KRune:
		switch k.Rune {
		case '?':
			a.openHelp()
		case 'm':
			a.cycleMode()
		case '/':
			a.homeFocus = 0
		default:
			a.homeFocus = 0
			a.editKey(k)
		}
	}
}

func (a *App) keyResults(k Key) {
	if a.editing {
		switch k.Code {
		case KEnter:
			if q := strings.TrimSpace(string(a.input)); q != "" {
				a.search(q)
			}
			a.editing = false
		case KEsc:
			a.editing = false
		default:
			a.editKey(k)
		}
		return
	}
	if a.pageKey(k) {
		return
	}
	_, rows, _, _ := a.listGeometry()
	switch k.Code {
	case KUp:
		a.sel = max(0, a.sel-1)
	case KDown:
		a.sel = min(len(a.results)-1, a.sel+1)
	case KPgUp:
		a.sel = max(0, a.sel-max(1, rows))
	case KPgDn:
		a.sel = min(len(a.results)-1, a.sel+max(1, rows))
	case KHome:
		a.sel = 0
	case KEnd:
		a.sel = len(a.results) - 1
	case KEnter:
		a.playIndex(a.sel)
	case KEsc, KBackspace:
		a.view = ViewHome
	case KRune:
		switch k.Rune {
		case 'k':
			a.sel = max(0, a.sel-1)
		case 'j':
			a.sel = min(len(a.results)-1, a.sel+1)
		case ' ':
			a.playIndex(a.sel)
		case '/', 's':
			a.editing = true
			a.input = []rune(a.query)
		case 'q':
			a.view = ViewHome
		case 'm':
			a.cycleMode()
		case '?':
			a.openHelp()
		case 'r':
			a.search(a.query)
		}
	}
	a.sel = max(0, a.sel)
}

func (a *App) keyPlayer(k Key) {
	switch k.Code {
	case KEsc, KBackspace:
		a.leavePlayer("")
		return
	case KLeft:
		a.seekBy(-5)
	case KRight:
		a.seekBy(5)
	case KPgUp:
		a.seekBy(-60)
	case KPgDn:
		a.seekBy(60)
	case KUp:
		a.changeVolume(0.1)
	case KDown:
		a.changeVolume(-0.1)
	case KHome:
		a.seekTo(0)
	case KRune:
		r := unicode.ToLower(k.Rune)
		switch {
		case r == ' ' || r == 'k':
			if a.pl != nil {
				a.pl.TogglePause()
			}
		case r == 'j':
			a.seekBy(-10)
		case r == 'l':
			a.seekBy(10)
		case r == 'm':
			a.cycleMode()
		case r == 'v':
			a.cycleQuality()
		case r == 'f':
			a.fullscreen = !a.fullscreen
			a.pre.WriteString(gfxDelete(1) + gfxDelete(2) + gfxDelete(50))
			a.shownThumb = thumbState{}
			a.gotFrame = false
			a.lastFrame = FrameEvent{}
			if a.pl != nil {
				a.pl.SetLayout(a.playerLayout())
			}
		case r == 'n':
			if a.nowPage >= 101 && a.nowPage < 101+len(a.results) && a.nowPage-101+1 < len(a.results) {
				a.playIndex(a.nowPage - 101 + 1)
			}
		case r == 'p':
			if a.nowPage > 101 && a.nowPage < 101+len(a.results) {
				a.playIndex(a.nowPage - 101 - 1)
			}
		case r == 'q':
			a.leavePlayer("")
		case r >= '0' && r <= '9':
			if a.st != nil && a.st.Duration > 0 {
				a.seekTo(a.st.Duration * float64(r-'0') / 10)
			}
		case r == '+' || r == '=':
			a.changeVolume(0.1)
		case r == '-':
			a.changeVolume(-0.1)
		}
	}
}

func (a *App) changeVolume(d float64) {
	a.volume = math.Max(0, math.Min(1, math.Round((a.volume+d)*10)/10))
	if a.pl != nil {
		a.pl.SetVolume(a.volume)
	}
	a.setStatus("VOLUME %d%%", int(math.Round(a.volume*100)))
}

func (a *App) keyCookies(k Key) {
	if k.Code == KEsc || k.Code == KBackspace {
		a.view = a.returnTo
		return
	}
	if k.Code != KRune {
		return
	}
	switch unicode.ToLower(k.Rune) {
	case 'c':
		a.yt.RetryBrowser()
		if a.now != nil {
			a.play(*a.now, a.nowPage)
		}
	case 'r':
		a.yt.Reload()
		if a.now != nil {
			a.play(*a.now, a.nowPage)
		}
	case 'q':
		a.view = a.returnTo
	case '?':
		a.openHelp()
	}
}

// --- events --------------------------------------------------------------------

func (a *App) onEvent(ev any) {
	switch e := ev.(type) {
	case resizeEvent:
		a.resizePending = false
		a.size = a.tty.Size()
		a.scr.Resize(a.size.Cols, a.size.Rows)
		a.pre.WriteString("\x1b[0m\x1b[2J" + gfxDeleteAll)
		a.shownThumb = thumbState{}
		a.lastFrame = FrameEvent{}
		if a.pl != nil {
			a.pl.SetLayout(a.playerLayout())
		}
	case SearchDone:
		if e.Gen != a.searchGen {
			return
		}
		a.searching = false
		if e.Err != nil {
			a.searchErr = e.Err.Error()
			return
		}
		a.results = e.Results
		a.sel, a.top = 0, 0
		for i, v := range a.results {
			if i < 12 {
				a.fetchThumb(v)
			}
		}
	case ThumbDone:
		delete(a.thumbLoading, e.Key)
		if e.Pic != nil {
			a.thumbs[e.Key] = e.Pic
		}
	case ResolveDone:
		if e.Gen != a.resolveGen || a.view != ViewPlayer {
			return
		}
		a.resolving = false
		if e.Err != nil {
			a.resolveErr = e.Err.Error()
			if errors.Is(e.Err, ErrBotCheck) {
				a.view = ViewCookies
			}
			return
		}
		a.st = e.St
		if a.now != nil {
			if a.now.Duration == 0 {
				a.now.Duration = e.St.Duration
			}
			if e.St.Title != "" && (a.now.Title == "" || strings.HasPrefix(a.now.Title, "YouTube ") || a.now.Title == a.now.URL) {
				a.now.Title, a.now.Channel = e.St.Title, e.St.Channel
				a.pushHistory(*a.now)
			}
		}
		a.startPlayback()
	case FrameEvent:
		if a.pl == nil || a.view != ViewPlayer || e.Gen != a.pl.Gen() {
			if e.Tmp != "" {
				os.Remove(e.Tmp)
			}
			return
		}
		if !a.gotFrame {
			a.gotFrame = true
		}
		if e.Kitty != "" {
			a.post.WriteString(e.Kitty)
			if e.Acked {
				gfxSent.Add(1)
			}
		} else {
			a.lastFrame = e
		}
	case VideoEnded:
		if a.pl == nil || e.Gen != a.pl.Gen() || a.st == nil {
			return
		}
		c := a.pl.Clock()
		if !a.st.Live && a.st.Duration > 0 && c >= a.st.Duration-3 {
			a.finish()
			return
		}
		if a.endRetries < 3 {
			a.endRetries++
			a.setStatus("Signal lost, retuning…")
			a.pl.Start(c)
			return
		}
		msg := e.Err
		if msg == "" {
			msg = "the stream stopped"
		}
		a.leavePlayer("Playback stopped: " + msg)
	}
}

func (a *App) finish() {
	title := ""
	if a.now != nil {
		title = a.now.Title
	}
	a.leavePlayer("Finished: " + title)
}

func (a *App) onTick() {
	if a.pageDigits != "" && time.Since(a.pageDigitsAt) > 4*time.Second {
		a.pageDigits = ""
	}
	if a.view == ViewPlayer && a.pl != nil {
		if a.seekPending && time.Since(a.seekAt) > 350*time.Millisecond {
			a.commitSeek()
		}
		if a.st != nil && a.st.Video == "" && a.pl.AudioDone() {
			a.finish()
		}
	}
	if a.view == ViewResults {
		_, rows, _, _ := a.listGeometry()
		for i := a.top; i < len(a.results) && i < a.top+rows+2; i++ {
			a.fetchThumb(a.results[i])
		}
	}
}

// --- history -------------------------------------------------------------------

func historyPath() string { return filepath.Join(stateDir(), "history.json") }

func loadHistory() []Video {
	b, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var vs []Video
	json.Unmarshal(b, &vs)
	return vs
}

func (a *App) pushHistory(v Video) {
	out := []Video{v}
	for _, h := range a.history {
		if h.Key() != v.Key() {
			out = append(out, h)
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	a.history = out
	os.MkdirAll(stateDir(), 0o755)
	if b, err := json.MarshalIndent(out, "", "  "); err == nil {
		os.WriteFile(historyPath(), b, 0o644)
	}
}
