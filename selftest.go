package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// runSelftest exercises everything except the terminal: search, stream
// resolving, video + audio decoding speed and the mosaic encoders.
func runSelftest(yt *YT, deps Deps, args []string) int {
	fmt.Printf("telly %s selftest\n  yt-dlp  %s\n  ffmpeg  %s\n", version, deps.YtDlp, deps.FFmpeg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	arg := strings.Join(args, " ")
	var target Video
	if v, ok := linkVideo(arg); ok {
		target = v
	} else {
		q := arg
		if q == "" {
			q = "big buck bunny"
		}
		t0 := time.Now()
		vs, err := yt.Search(ctx, q, 10)
		if err != nil {
			fmt.Printf("FAIL search %q: %v\n", q, err)
			return 1
		}
		fmt.Printf("ok   search %q: %d results in %.1fs\n", q, len(vs), time.Since(t0).Seconds())
		for _, v := range vs {
			if !v.Live {
				target = v
				break
			}
		}
		if target.Key() == "" {
			fmt.Println("FAIL no video to try")
			return 1
		}
		t1 := time.Now()
		pic, err := FetchThumb(ctx, target.ID)
		if err != nil {
			fmt.Printf("FAIL thumbnail: %v\n", err)
		} else {
			fmt.Printf("ok   thumbnail %dx%d in %.1fs\n", pic.W, pic.H, time.Since(t1).Seconds())
		}
	}
	t0 := time.Now()
	st, err := yt.Resolve(ctx, target)
	if err != nil {
		fmt.Printf("FAIL resolve %s: %v\n", target.PageURL(), err)
		if errors.Is(err, ErrBotCheck) {
			fmt.Println("     → YouTube wants cookies; see the P403 page in the app")
		}
		return 1
	}
	fmt.Printf("ok   resolve %q (%s) in %.1fs: %dx%d @%.0ffps, %s, audio=%v, cookies=%q\n",
		st.Title, fmtDur(st.Duration), time.Since(t0).Seconds(), st.W, st.H, st.FPS,
		map[bool]string{true: "live", false: "vod"}[st.Live], st.Audio != "", yt.cookieNote)

	p := &Player{ff: deps.FFmpeg, st: st}
	start := 0.0
	if st.Duration > 60 {
		start = 30 // also proves seeking works
	}
	if st.Video != "" {
		w, h := 1280, 720
		args := append([]string{"-hwaccel", "videotoolbox"}, p.inputArgs(st.Video, start)...)
		args = append(args, "-i", st.Video, "-t", "6", "-an", "-vf", fmt.Sprintf("fps=30,scale=%d:%d:flags=bilinear", w, h), "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
		n, first, el, err := countFrames(deps.FFmpeg, args, w*h*3)
		if err != nil || n == 0 {
			fmt.Printf("FAIL video decode: %v\n", err)
			return 1
		}
		fmt.Printf("ok   video %d frames of 6s @30fps (%dx%d) in %.1fs, first frame after %.2fs → %.0f fps decode\n",
			n, w, h, el.Seconds(), first.Seconds(), float64(n)/(el-first).Seconds())
	}
	if st.Audio != "" {
		args := append(p.inputArgs(st.Audio, start), "-i", st.Audio, "-t", "4", "-vn", "-ac", "2", "-ar", "48000", "-f", "s16le", "pipe:1")
		t := time.Now()
		out, err := exec.CommandContext(ctx, deps.FFmpeg, args...).Output()
		if err != nil || len(out) == 0 {
			fmt.Printf("FAIL audio decode: %v\n", err)
			return 1
		}
		fmt.Printf("ok   audio %.1fs of PCM in %.1fs\n", float64(len(out))/bytesPerSec, time.Since(t).Seconds())
	}
	// encoder throughput at a large terminal size
	cols, rows := 200, 50
	img := make([]byte, cols*2*rows*3*3)
	for i := range img {
		img[i] = byte(i * 7)
	}
	cells := make([]Cell, cols*rows)
	t := time.Now()
	for range 30 {
		SextantCells(img, cols, rows, cells)
	}
	fmt.Printf("ok   mosaic encode %dx%d: %.1f ms/frame\n", cols, rows, float64(time.Since(t).Microseconds())/30/1000)
	t = time.Now()
	for range 30 {
		CeefaxCells(img, cols, rows, cells)
	}
	fmt.Printf("ok   ceefax encode %dx%d: %.1f ms/frame\n", cols, rows, float64(time.Since(t).Microseconds())/30/1000)
	fmt.Println("all good")
	return 0
}

func countFrames(ff string, args []string, size int) (int, time.Duration, time.Duration, error) {
	cmd := exec.Command(ff, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, 0, err
	}
	t := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, 0, 0, err
	}
	br := bufio.NewReaderSize(out, 1<<20)
	buf := make([]byte, size)
	n := 0
	var first time.Duration
	for {
		if _, err := io.ReadFull(br, buf); err != nil {
			break
		}
		if n == 0 {
			first = time.Since(t)
		}
		n++
	}
	werr := cmd.Wait()
	if n == 0 && werr != nil {
		return 0, 0, 0, fmt.Errorf("%v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return n, first, time.Since(t), nil
}
