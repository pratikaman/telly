package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

type Deps struct{ YtDlp, FFmpeg, FFprobe string }

// findBin returns the first candidate that actually runs: PATH can hold
// stale shims (a pyenv yt-dlp shim, an ffmpeg with a broken dylib).
func findBin(name, versionArg string, extra ...string) string {
	var cands []string
	home, _ := os.UserHomeDir()
	shim := ""
	if p, err := exec.LookPath(name); err == nil {
		if strings.Contains(p, "/shims/") {
			shim = p // version-manager shims are slow and often dead ends
		} else {
			cands = append(cands, p)
		}
	}
	extra = append(extra, "/opt/homebrew/bin/"+name, "/usr/local/bin/"+name, filepath.Join(home, ".local/bin", name))
	cands = append(cands, extra...)
	if shim != "" {
		cands = append(cands, shim)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c] {
			continue
		}
		seen[c] = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := exec.CommandContext(ctx, c, versionArg).Run()
		cancel()
		if err == nil {
			return c
		}
	}
	return ""
}

func findDeps() (Deps, error) {
	var d Deps
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); d.YtDlp = findBin("yt-dlp", "--version") }()
	go func() { defer wg.Done(); d.FFmpeg = findBin("ffmpeg", "-version") }()
	go func() { defer wg.Done(); d.FFprobe = findBin("ffprobe", "-version") }()
	wg.Wait()
	var missing []string
	if d.YtDlp == "" {
		missing = append(missing, "yt-dlp")
	}
	if d.FFmpeg == "" {
		missing = append(missing, "ffmpeg")
	}
	if len(missing) > 0 {
		return d, fmt.Errorf("telly needs %s that runs. Install with:\n\n  brew install %s\n", strings.Join(missing, " and "), strings.Join(missing, " "))
	}
	return d, nil
}

func main() {
	modeFlag := flag.String("mode", "", "picture style: hd, mosaic, blocks or ceefax")
	qualityFlag := flag.Int("quality", 0, "max video height: 360, 480 (default), 720 or 1080")
	selftest := flag.Bool("selftest", false, "check search, stream resolving and decoding, then exit")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `telly %s: YouTube on a teletext telly, in your terminal.

usage:
  telly                      open the index page (P100)
  telly <search words>       search YouTube
  telly <youtube url|file>   play a link or a local video

options:
`, version)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
environment:
  TELLY_GFX=none|direct      force mosaic pictures / inline image transfer
  TELLY_COOKIES=<file>       cookies.txt for YouTube (or ~/.config/telly/cookies.txt)
  TELLY_BROWSER=chrome       borrow a browser's YouTube sign-in
  TELLY_MAXW=<px>            cap the HD decode width (default 1280)
`)
	}
	flag.Parse()
	initDebug()
	if *showVersion {
		fmt.Println("telly", version)
		return
	}
	deps, err := findDeps()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	yt := NewYT(deps.YtDlp, deps.FFprobe)
	if *qualityFlag > 0 {
		yt.SetQuality(*qualityFlag)
	}
	if *selftest {
		os.Exit(runSelftest(yt, deps, flag.Args()))
	}

	mode := ModeHD
	if m, ok := parseMode(loadConfig()["mode"]); ok {
		mode = m
	}
	if *modeFlag != "" {
		m, ok := parseMode(*modeFlag)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown --mode %q (hd, mosaic, blocks, ceefax)\n", *modeFlag)
			os.Exit(2)
		}
		mode = m
	}

	tty, err := OpenTTY()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gfx := ProbeGfx(tty)
	tty.StartKeys()
	app := NewApp(tty, gfx, yt, deps.FFmpeg, mode)
	func() {
		defer func() {
			if r := recover(); r != nil {
				app.stopPlayer()
				tty.Close()
				gfx.Cleanup()
				panic(r)
			}
		}()
		app.Run(strings.Join(flag.Args(), " "))
	}()
	tty.Close()
	gfx.Cleanup()
}
