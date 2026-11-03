package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Video struct {
	ID       string  `json:"id,omitempty"`
	Title    string  `json:"title"`
	Channel  string  `json:"channel,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	Views    int64   `json:"views,omitempty"`
	Live     bool    `json:"live,omitempty"`
	URL      string  `json:"url,omitempty"` // non-YouTube links and local files
}

func (v Video) Key() string {
	if v.ID != "" {
		return v.ID
	}
	return v.URL
}

func (v Video) PageURL() string {
	if v.URL != "" {
		return v.URL
	}
	return "https://www.youtube.com/watch?v=" + v.ID
}

type Streams struct {
	Video, Audio string
	Headers      map[string]string
	W, H         int
	FPS          float64
	Duration     float64
	Live         bool
	Title        string
	Channel      string
}

var ErrBotCheck = errors.New("YouTube wants a signed-in session (\"confirm you're not a bot\")")

type YT struct {
	bin, ffprobe string
	mu           sync.Mutex
	cookies      []string // extra yt-dlp args, e.g. --cookies-from-browser chrome
	cookieNote   string
	triedBrowser bool
	quality      int // max video height
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "telly")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "telly")
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "telly")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "telly")
}

// loadConfig reads ~/.config/telly/config ("key = value" lines).
func loadConfig() map[string]string {
	cfg := map[string]string{}
	f, err := os.Open(filepath.Join(configDir(), "config"))
	if err != nil {
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return cfg
}

func saveConfigKey(key, val string) {
	cfg := loadConfig()
	cfg[key] = val
	os.MkdirAll(configDir(), 0o755)
	var b strings.Builder
	b.WriteString("# telly settings\n")
	for k, v := range cfg {
		fmt.Fprintf(&b, "%s = %s\n", k, v)
	}
	os.WriteFile(filepath.Join(configDir(), "config"), []byte(b.String()), 0o644)
}

func NewYT(bin, ffprobe string) *YT {
	y := &YT{bin: bin, ffprobe: ffprobe, quality: 480}
	cfg := loadConfig()
	if q, err := strconv.Atoi(strings.TrimSuffix(cfg["quality"], "p")); err == nil && q >= 144 {
		y.quality = q
	}
	cookieFile := os.Getenv("TELLY_COOKIES")
	if cookieFile == "" {
		cookieFile = cfg["cookies"]
	}
	if cookieFile == "" {
		if p := filepath.Join(configDir(), "cookies.txt"); fileExists(p) {
			cookieFile = p
		}
	}
	browser := os.Getenv("TELLY_BROWSER")
	if browser == "" {
		browser = cfg["browser"]
	}
	switch {
	case cookieFile != "":
		y.cookies = []string{"--cookies", expandHome(cookieFile)}
		y.cookieNote = "cookies.txt"
	case browser != "" && browser != "none":
		y.cookies = []string{"--cookies-from-browser", browser}
		y.cookieNote = browser + " cookies"
		y.triedBrowser = true
	}
	return y
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

type ytError struct{ msg string }

func (e *ytError) Error() string { return e.msg }

func (y *YT) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--ignore-config", "--no-warnings", "--no-progress"}, args...)
	cmd := exec.CommandContext(ctx, y.bin, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "ERROR:"); i >= 0 {
			msg = strings.TrimSpace(msg[i+6:])
		}
		if strings.Contains(msg, "not a bot") {
			return nil, ErrBotCheck
		}
		if msg == "" {
			msg = err.Error()
		}
		if nl := strings.IndexByte(msg, '\n'); nl > 0 {
			msg = msg[:nl]
		}
		return nil, &ytError{msg}
	}
	return out, nil
}

type flatEntry struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Channel     string   `json:"channel"`
	Uploader    string   `json:"uploader"`
	Duration    *float64 `json:"duration"`
	ViewCount   *int64   `json:"view_count"`
	Concurrent  *int64   `json:"concurrent_view_count"`
	LiveStatus  string   `json:"live_status"`
	IEKey       string   `json:"ie_key"`
	URL         string   `json:"url"`
	Description string   `json:"description"`
}

var ytIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func (y *YT) Search(ctx context.Context, q string, n int) ([]Video, error) {
	out, err := y.run(ctx, "--flat-playlist", "-J", fmt.Sprintf("ytsearch%d:%s", n, q))
	if err != nil {
		return nil, err
	}
	var r struct {
		Entries []flatEntry `json:"entries"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	var vs []Video
	for _, e := range r.Entries {
		if !ytIDRe.MatchString(e.ID) {
			continue
		}
		v := Video{ID: e.ID, Title: e.Title, Channel: e.Channel}
		if v.Channel == "" {
			v.Channel = e.Uploader
		}
		if e.Duration != nil {
			v.Duration = *e.Duration
		}
		if e.ViewCount != nil {
			v.Views = *e.ViewCount
		}
		if e.LiveStatus == "is_live" {
			v.Live = true
			if e.Concurrent != nil {
				v.Views = *e.Concurrent
			}
		}
		vs = append(vs, v)
	}
	return vs, nil
}

// formatFor picks video up to q lines at ≤30 fps (H.264 first, it decodes in
// hardware) plus an AAC track, falling back to whatever exists.
func formatFor(q int) string {
	return fmt.Sprintf("bv*[height<=%[1]d][fps<=30][vcodec^=avc1]+ba[acodec^=mp4a]/bv*[height<=%[1]d][fps<=30]+ba/bv*[height<=%[1]d]+ba/b[height<=%[1]d]/b", q)
}

var Qualities = []int{360, 480, 720, 1080}

func (y *YT) Quality() int {
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.quality
}

func (y *YT) SetQuality(q int) {
	y.mu.Lock()
	y.quality = q
	y.mu.Unlock()
}

type fmtInfo struct {
	URL     string            `json:"url"`
	Width   int               `json:"width"`
	Height  int               `json:"height"`
	FPS     float64           `json:"fps"`
	VCodec  string            `json:"vcodec"`
	ACodec  string            `json:"acodec"`
	Headers map[string]string `json:"http_headers"`
}

func (y *YT) Resolve(ctx context.Context, v Video) (*Streams, error) {
	if v.URL != "" && fileExists(v.URL) {
		return y.probeFile(ctx, v.URL)
	}
	y.mu.Lock()
	cookies := y.cookies
	tried := y.triedBrowser
	y.mu.Unlock()
	st, err := y.resolve(ctx, v, cookies)
	if errors.Is(err, ErrBotCheck) && !tried {
		// First time YouTube balks: borrow the signed-in Chrome session.
		y.mu.Lock()
		y.triedBrowser = true
		y.mu.Unlock()
		alt := []string{"--cookies-from-browser", "chrome"}
		st2, err2 := y.resolve(ctx, v, alt)
		if err2 == nil {
			y.mu.Lock()
			y.cookies, y.cookieNote = alt, "chrome cookies"
			y.mu.Unlock()
			saveConfigKey("browser", "chrome")
			return st2, nil
		}
		return nil, fmt.Errorf("%w; Chrome cookies: %v", ErrBotCheck, err2)
	}
	return st, err
}

// Reload re-reads cookie settings (after the user drops in a cookies.txt).
func (y *YT) Reload() {
	n := NewYT(y.bin, y.ffprobe)
	n.quality = y.Quality()
	y.mu.Lock()
	y.cookies, y.cookieNote, y.triedBrowser = n.cookies, n.cookieNote, n.triedBrowser
	y.mu.Unlock()
}

func (y *YT) RetryBrowser() {
	y.mu.Lock()
	y.triedBrowser = false
	y.mu.Unlock()
}

func (y *YT) resolve(ctx context.Context, v Video, cookies []string) (*Streams, error) {
	args := append([]string{}, cookies...)
	args = append(args, "-J", "--no-playlist", "-f", formatFor(y.Quality()), v.PageURL())
	out, err := y.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var r struct {
		fmtInfo
		Title     string    `json:"title"`
		Channel   string    `json:"channel"`
		Uploader  string    `json:"uploader"`
		Duration  float64   `json:"duration"`
		IsLive    bool      `json:"is_live"`
		Requested []fmtInfo `json:"requested_formats"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	st := &Streams{Title: r.Title, Channel: r.Channel, Duration: r.Duration, Live: r.IsLive}
	if st.Channel == "" {
		st.Channel = r.Uploader
	}
	pick := func(f fmtInfo) {
		hasV := f.VCodec != "none" && (f.VCodec != "" || f.Width > 0)
		hasA := f.ACodec != "none"
		if hasV && st.Video == "" {
			st.Video, st.W, st.H, st.FPS = f.URL, f.Width, f.Height, f.FPS
		}
		if hasA && st.Audio == "" {
			st.Audio = f.URL
		}
		if st.Headers == nil {
			st.Headers = f.Headers
		}
	}
	if len(r.Requested) > 0 {
		for _, f := range r.Requested {
			pick(f)
		}
	} else {
		pick(r.fmtInfo)
	}
	if st.Video == "" && st.Audio == "" {
		return nil, fmt.Errorf("no playable stream found")
	}
	return st, nil
}

func (y *YT) probeFile(ctx context.Context, path string) (*Streams, error) {
	out, err := exec.CommandContext(ctx, y.ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %v", err)
	}
	var r struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Rate      string `json:"avg_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	json.Unmarshal(out, &r)
	st := &Streams{Title: filepath.Base(path), Channel: "local file"}
	st.Duration, _ = strconv.ParseFloat(r.Format.Duration, 64)
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			if st.Video == "" {
				st.Video, st.W, st.H = path, s.Width, s.Height
				if a, b, ok := strings.Cut(s.Rate, "/"); ok {
					n, _ := strconv.ParseFloat(a, 64)
					d, _ := strconv.ParseFloat(b, 64)
					if d > 0 {
						st.FPS = n / d
					}
				}
			}
		case "audio":
			st.Audio = path
		}
	}
	if st.Video == "" && st.Audio == "" {
		return nil, fmt.Errorf("no audio or video in %s", path)
	}
	return st, nil
}

// --- thumbnails ------------------------------------------------------------

var httpClient = &http.Client{Timeout: 12 * time.Second}

func FetchThumb(ctx context.Context, id string) (*Pic, error) {
	var lastErr error
	for _, name := range []string{"hq720.jpg", "mqdefault.jpg", "hqdefault.jpg"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://i.ytimg.com/vi/"+id+"/"+name, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("thumbnail: %s", resp.Status)
			continue
		}
		img, err := jpeg.Decode(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return picFromImage(img, 640), nil
	}
	return nil, lastErr
}

// picFromImage converts to RGB24, shrinking to at most maxW wide.
func picFromImage(img image.Image, maxW int) *Pic {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	p := &Pic{W: w, H: h, RGB: make([]byte, w*h*3)}
	if yc, ok := img.(*image.YCbCr); ok {
		for y := range h {
			for x := range w {
				c := yc.YCbCrAt(b.Min.X+x, b.Min.Y+y)
				r, g, bb := ycbcrToRGB(c.Y, c.Cb, c.Cr)
				o := (y*w + x) * 3
				p.RGB[o], p.RGB[o+1], p.RGB[o+2] = r, g, bb
			}
		}
	} else {
		for y := range h {
			for x := range w {
				r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				o := (y*w + x) * 3
				p.RGB[o], p.RGB[o+1], p.RGB[o+2] = uint8(r>>8), uint8(g>>8), uint8(bb>>8)
			}
		}
	}
	if w > maxW {
		p = p.Resize(maxW, h*maxW/w)
	}
	return p
}

func ycbcrToRGB(y, cb, cr uint8) (uint8, uint8, uint8) {
	yy := int32(y) * 0x10101
	cb1 := int32(cb) - 128
	cr1 := int32(cr) - 128
	r := yy + 91881*cr1
	g := yy - 22554*cb1 - 46802*cr1
	b := yy + 116130*cb1
	clamp := func(v int32) uint8 {
		v >>= 16
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return clamp(r), clamp(g), clamp(b)
}
