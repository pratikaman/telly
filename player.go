package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/oto/v3"
)

type RenderMode int

const (
	ModeHD RenderMode = iota
	ModeMosaic
	ModeBlocks
	ModeCeefax
)

func (m RenderMode) String() string {
	return [...]string{"HD", "MOSAIC", "BLOCKS", "CEEFAX"}[m]
}

func parseMode(s string) (RenderMode, bool) {
	switch strings.ToLower(s) {
	case "hd", "kitty":
		return ModeHD, true
	case "mosaic", "sextant":
		return ModeMosaic, true
	case "blocks", "half":
		return ModeBlocks, true
	case "ceefax", "teletext":
		return ModeCeefax, true
	}
	return 0, false
}

// Layout says where the picture goes and at what size ffmpeg should decode it.
type Layout struct {
	Mode       RenderMode
	Rect       Rect
	DecW, DecH int
	FPS        float64
}

type FrameEvent struct {
	Gen   int
	Kitty string // escape sequence (HD)
	Tmp   string // temp file behind Kitty, removed if the frame is discarded
	Cells []Cell // mosaic modes
	Rect  Rect
	Acked bool // the terminal will reply to this frame
}

type VideoEnded struct {
	Gen int
	Err string
}

const (
	sampleRate  = 48000
	bytesPerSec = sampleRate * 2 * 2
	maxInFlight = 2
)

var flowOff atomic.Bool

var (
	otoOnce sync.Once
	otoCtx  *oto.Context
	otoErr  error
)

func audioContext() (*oto.Context, error) {
	otoOnce.Do(func() {
		c, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:   sampleRate,
			ChannelCount: 2,
			Format:       oto.FormatSignedInt16LE,
		})
		if err != nil {
			otoErr = err
			return
		}
		<-ready
		otoCtx = c
	})
	return otoCtx, otoErr
}

type countReader struct {
	r   io.Reader
	n   atomic.Int64
	eof atomic.Bool
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	if err != nil {
		c.eof.Store(true)
	}
	return n, err
}

type audioSess struct {
	cmd    *exec.Cmd
	cr     *countReader
	pl     *oto.Player
	start  float64
	failed atomic.Bool
}

type videoSess struct {
	gen    int
	cmd    *exec.Cmd
	lay    Layout
	start  float64
	frames chan []byte
	quit   chan struct{}
	pool   sync.Pool
	flip   int
	errBuf *bytes.Buffer
}

// Player runs two ffmpeg processes (audio → CoreAudio, video → raw RGB) and
// schedules video frames against the audio clock.
type Player struct {
	ff   string
	st   *Streams
	gfx  *Gfx
	post chan<- any

	mu          sync.Mutex
	paused      bool
	vol         float64
	audio       *audioSess
	video       *videoSess
	useWall     bool
	wallStarted bool
	clockBase   float64
	clockT0     time.Time
	lay         Layout
	gen         int
	closed      bool
	audioErr    string
}

func NewPlayer(ff string, st *Streams, gfx *Gfx, post chan<- any, lay Layout, vol float64) *Player {
	return &Player{ff: ff, st: st, gfx: gfx, post: post, lay: lay, vol: vol}
}

func isHTTP(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

func (p *Player) inputArgs(url string, pos float64) []string {
	a := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if isHTTP(url) {
		a = append(a, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5")
		if strings.Contains(url, "googlevideo.com/videoplayback") && !p.st.Live {
			// One long GET is throttled to about real time; 1 MB range
			// requests are served at full speed (measured 0.45 vs 7.5 MB/s).
			a = append(a, "-request_size", "1048576", "-multiple_requests", "1")
		}
		if ua := p.st.Headers["User-Agent"]; ua != "" {
			a = append(a, "-user_agent", ua)
		}
		var hb strings.Builder
		for k, v := range p.st.Headers {
			if k != "User-Agent" {
				hb.WriteString(k + ": " + v + "\r\n")
			}
		}
		if hb.Len() > 0 {
			a = append(a, "-headers", hb.String())
		}
	}
	if pos > 0.05 && !p.st.Live {
		a = append(a, "-ss", fmt.Sprintf("%.3f", pos))
	}
	return a
}

// Start (re)starts playback at pos seconds.
func (p *Player) Start(pos float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.stopAudioLocked()
	if p.st.Duration > 0 && pos > p.st.Duration-1 {
		pos = math.Max(0, p.st.Duration-1)
	}
	if pos < 0 {
		pos = 0
	}
	p.clockBase, p.clockT0 = pos, time.Now()
	p.wallStarted = false
	p.useWall = true
	if p.st.Audio != "" {
		if a, err := p.startAudio(pos); err == nil {
			p.audio = a
			p.useWall = false
		} else {
			p.audioErr = err.Error()
		}
	}
	p.startVideoLocked(pos)
}

func (p *Player) startAudio(pos float64) (*audioSess, error) {
	ctx, err := audioContext()
	if err != nil {
		return nil, err
	}
	args := p.inputArgs(p.st.Audio, pos)
	args = append(args, "-i", p.st.Audio, "-vn", "-sn", "-dn", "-ac", "2", "-ar", fmt.Sprint(sampleRate), "-f", "s16le", "pipe:1")
	cmd := exec.Command(p.ff, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{b: &errBuf, max: 4096}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &audioSess{cmd: cmd, start: pos}
	a.cr = &countReader{r: bufio.NewReaderSize(stdout, 1<<16)}
	a.pl = ctx.NewPlayer(a.cr)
	a.pl.SetBufferSize(bytesPerSec / 5)
	a.pl.SetVolume(p.vol)
	if !p.paused {
		a.pl.Play()
	}
	dbg("audio start at %.2f", pos)
	go func() {
		cmd.Wait()
		dbg("audio ffmpeg exited after %d bytes: %s", a.cr.n.Load(), strings.TrimSpace(errBuf.String()))
		if a.cr.n.Load() == 0 {
			a.failed.Store(true)
			p.mu.Lock()
			if msg := strings.TrimSpace(errBuf.String()); msg != "" {
				p.audioErr = msg
			}
			p.mu.Unlock()
		}
	}()
	return a, nil
}

func (p *Player) stopAudioLocked() {
	if a := p.audio; a != nil {
		a.pl.Pause()
		if a.cmd.Process != nil {
			a.cmd.Process.Kill()
		}
		go a.pl.Close()
		p.audio = nil
	}
}

func (p *Player) startVideoLocked(pos float64) {
	if v := p.video; v != nil {
		v.stop()
		p.video = nil
	}
	p.gen++
	lay := p.lay
	if p.st.Video == "" || lay.Rect.W <= 0 || lay.Rect.H <= 0 || lay.DecW <= 0 || lay.DecH <= 0 {
		return
	}
	if lay.FPS <= 0 {
		lay.FPS = 30
	}
	vs := &videoSess{
		gen: p.gen, lay: lay, start: pos,
		frames: make(chan []byte, 3),
		quit:   make(chan struct{}),
		errBuf: &bytes.Buffer{},
	}
	size := lay.DecW * lay.DecH * 3
	vs.pool.New = func() any { return make([]byte, size) }
	args := []string{}
	if runtime.GOOS == "darwin" {
		args = append(args, "-hwaccel", "videotoolbox")
	}
	args = append(args, p.inputArgs(p.st.Video, pos)...)
	flags := "bilinear"
	if lay.Mode != ModeHD {
		flags = "area"
	}
	vf := fmt.Sprintf("fps=%g,scale=%d:%d:flags=%s", lay.FPS, lay.DecW, lay.DecH, flags)
	args = append(args, "-i", p.st.Video, "-an", "-sn", "-dn", "-vf", vf, "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
	vs.cmd = exec.Command(p.ff, args...)
	vs.cmd.Stderr = &limitedWriter{b: vs.errBuf, max: 4096}
	stdout, err := vs.cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := vs.cmd.Start(); err != nil {
		return
	}
	p.video = vs
	dbg("video start gen=%d at %.2f: %q", vs.gen, pos, args)
	go vs.readLoop(stdout, size)
	go p.schedule(vs)
}

func (vs *videoSess) stop() {
	select {
	case <-vs.quit:
	default:
		close(vs.quit)
	}
	if vs.cmd != nil && vs.cmd.Process != nil {
		vs.cmd.Process.Kill()
	}
}

func (vs *videoSess) readLoop(r io.Reader, size int) {
	defer close(vs.frames)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		buf := vs.pool.Get().([]byte)
		if _, err := io.ReadFull(br, buf); err != nil {
			vs.cmd.Wait()
			return
		}
		select {
		case vs.frames <- buf:
		case <-vs.quit:
			return
		}
	}
}

func (p *Player) schedule(vs *videoSess) {
	n, dropped := 0, 0
	fps := vs.lay.FPS
	shown, skipped, behind := 0, 0, 0
	statT := time.Now()
	lastAcks, lastAckT := gfxAcks.Load(), time.Now()
	for {
		var buf []byte
		var ok bool
		select {
		case buf, ok = <-vs.frames:
		case <-vs.quit:
			return
		}
		if !ok {
			break
		}
		pts := vs.start + float64(n)/fps
		n++
		var now float64
		for {
			select {
			case <-vs.quit:
				return
			default:
			}
			p.mu.Lock()
			if p.useWall && !p.wallStarted {
				p.wallStarted = true
				p.clockBase, p.clockT0 = pts, time.Now()
			}
			now = p.clock()
			paused := p.paused
			p.mu.Unlock()
			if paused {
				time.Sleep(15 * time.Millisecond)
				continue
			}
			d := pts - now
			if d > 0.003 {
				time.Sleep(time.Duration(math.Min(d, 0.02) * float64(time.Second)))
				continue
			}
			break
		}
		if dbgOn && time.Since(statT) > 2*time.Second {
			p.mu.Lock()
			src, ab := "audio", int64(0)
			if p.useWall {
				src = "wall"
			}
			if p.audio != nil {
				ab = p.audio.cr.n.Load()
			}
			p.mu.Unlock()
			dbg("video gen=%d %s %dx%d: %.1f fps shown, %d late, %d skipped for terminal (in flight %d), pts=%.2f clock=%.2f (%s, %d audio bytes)",
				vs.gen, vs.lay.Mode, vs.lay.DecW, vs.lay.DecH, float64(shown)/time.Since(statT).Seconds(), skipped, behind,
				gfxSent.Load()-gfxAcks.Load(), pts, now, src, ab)
			shown, skipped, behind, statT = 0, 0, 0, time.Now()
		}
		if now-pts > 1.5/fps && dropped < 8 && len(vs.frames) > 0 {
			dropped++
			skipped++
			vs.pool.Put(buf)
			continue
		}
		// Never queue pictures in the terminal: if it hasn't taken in the
		// previous frames yet, skip this one so video can't drift behind sound.
		if vs.lay.Mode == ModeHD && !flowOff.Load() {
			if a := gfxAcks.Load(); a != lastAcks {
				lastAcks, lastAckT = a, time.Now()
			}
			if gfxSent.Load()-gfxAcks.Load() >= maxInFlight {
				if time.Since(lastAckT) > 3*time.Second {
					flowOff.Store(true) // terminal never answers: stop waiting for it
					dbg("terminal sends no graphics replies; flow control off")
				} else {
					behind++
					vs.pool.Put(buf)
					continue
				}
			}
		}
		dropped = 0
		shown++
		ev := p.renderFrame(vs, buf)
		vs.pool.Put(buf)
		select {
		case p.post <- ev:
		case <-vs.quit:
			if ev.Tmp != "" {
				os.Remove(ev.Tmp)
			}
			return
		}
	}
	select {
	case <-vs.quit:
		return
	default:
	}
	msg := strings.TrimSpace(vs.errBuf.String())
	select {
	case p.post <- VideoEnded{Gen: vs.gen, Err: msg}:
	case <-vs.quit:
	}
}

func (p *Player) renderFrame(vs *videoSess, buf []byte) FrameEvent {
	lay := vs.lay
	ev := FrameEvent{Gen: vs.gen, Rect: lay.Rect}
	switch lay.Mode {
	case ModeHD:
		// Double-buffer between image ids 1 and 2 so a frame is never blank.
		vs.flip ^= 1
		id := 1 + vs.flip
		ack := !flowOff.Load()
		cmd, tmp, err := p.gfx.Place(id, buf, lay.DecW, lay.DecH, lay.Rect, ack)
		if err == nil {
			ev.Kitty = cmd + gfxDelete(3-id)
			ev.Tmp = tmp
			ev.Acked = ack
		}
	case ModeMosaic:
		ev.Cells = make([]Cell, lay.Rect.W*lay.Rect.H)
		SextantCells(buf, lay.Rect.W, lay.Rect.H, ev.Cells)
	case ModeCeefax:
		ev.Cells = make([]Cell, lay.Rect.W*lay.Rect.H)
		CeefaxCells(buf, lay.Rect.W, lay.Rect.H, ev.Cells)
	case ModeBlocks:
		ev.Cells = make([]Cell, lay.Rect.W*lay.Rect.H)
		HalfCells(buf, lay.Rect.W, lay.Rect.H, ev.Cells)
	}
	return ev
}

// clock returns the playback position. Audio is the master: bytes handed to
// CoreAudio minus what is still buffered. Without audio it is wall time.
func (p *Player) clock() float64 {
	if a := p.audio; a != nil && !p.useWall {
		played := a.cr.n.Load() - int64(a.pl.BufferedSize())
		if played < 0 {
			played = 0
		}
		t := a.start + float64(played)/bytesPerSec
		if (a.cr.eof.Load() && a.pl.BufferedSize() == 0) || a.failed.Load() {
			p.useWall, p.wallStarted = true, true
			p.clockBase, p.clockT0 = t, time.Now()
		}
		return t
	}
	if p.paused || !p.wallStarted {
		return p.clockBase
	}
	return p.clockBase + time.Since(p.clockT0).Seconds()
}

func (p *Player) Clock() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clock()
}

func (p *Player) Gen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

func (p *Player) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *Player) AudioDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.audio
	return a != nil && a.cr.eof.Load() && a.pl.BufferedSize() == 0
}

func (p *Player) AudioErr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.audioErr
}

func (p *Player) TogglePause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		p.paused = false
		if p.audio != nil && !p.useWall {
			p.audio.pl.Play()
		} else {
			p.clockT0 = time.Now()
		}
		return
	}
	t := p.clock()
	p.paused = true
	if p.audio != nil {
		p.audio.pl.Pause()
	}
	if p.useWall {
		p.clockBase = t
	}
}

func (p *Player) Volume() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vol
}

func (p *Player) SetVolume(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vol = math.Max(0, math.Min(1, v))
	if p.audio != nil {
		p.audio.pl.SetVolume(p.vol)
	}
}

// SetLayout moves or re-sizes the picture; the video decoder restarts at the
// current position while audio keeps playing.
func (p *Player) SetLayout(lay Layout) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lay == p.lay || p.closed {
		return
	}
	p.lay = lay
	p.startVideoLocked(p.clock())
}

func (p *Player) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.stopAudioLocked()
	if p.video != nil {
		p.video.stop()
		p.video = nil
	}
}

type limitedWriter struct {
	b   *bytes.Buffer
	max int
	mu  sync.Mutex
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - w.b.Len(); room > 0 {
		if len(p) > room {
			w.b.Write(p[:room])
		} else {
			w.b.Write(p)
		}
	}
	return len(p), nil
}
