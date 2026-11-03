# telly

**YouTube in your terminal, dressed up as a 1980s Ceefax page.**

Search YouTube, flick through results with thumbnails, and watch videos with sound,
all without leaving the terminal. In terminals that can show images (cmux, Ghostty,
kitty, WezTerm) you get a real, sharp picture. Everywhere else you get a chunky
teletext-style one, which is half the fun.

Made for macOS.

---

## Install with an AI agent (easiest)

Copy this prompt into Claude Code, Codex, or any coding agent and let it do the work:

```text
Install telly (https://github.com/pratikaman/telly) on my Mac:

1. Make sure Homebrew is installed, then run: brew install go yt-dlp ffmpeg
2. Install telly: go install github.com/pratikaman/telly@latest
3. Make sure Go's bin folder (usually ~/go/bin) is on my PATH in my shell config,
   so I can type `telly` from anywhere.
4. Run `telly --selftest` and tell me whether everything passed.
   If it says YouTube wants a signed-in session, read the "Sign in needed"
   section of the README and walk me through it.
```

## Install by hand

You need [Homebrew](https://brew.sh). Then, in a terminal:

```sh
brew install go yt-dlp ffmpeg
go install github.com/pratikaman/telly@latest
```

Type `telly` to start. If your terminal says "command not found", add Go's folder to your path:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

---

## How to use it

| Type this | What happens |
| --- | --- |
| `telly` | Opens the front page (page 100) |
| `telly lofi hip hop` | Searches YouTube |
| `telly https://youtu.be/...` | Plays that video straight away |

On the front page, just start typing and press **Enter** to search.

### Keys

| Key | Does |
| --- | --- |
| **↑ ↓** then **Enter** | Pick a video and play it |
| **Space** | Pause / play |
| **← →** | Skip back / forward 5 seconds (**J L** for 10, **PgUp PgDn** for a minute) |
| **↑ ↓** while watching | Volume |
| **0 – 9** | Jump to 0 %, 10 % … 90 % of the video |
| **v** | Video quality: 360p, 480p, 720p, 1080p |
| **m** | Picture style (see below) |
| **f** | Full screen |
| **n p** | Next / previous video in your results |
| **/** | Search again |
| **q** or **Esc** | Go back |
| **Ctrl-C** | Quit |

Like real teletext, every page has a number. Type three digits to jump to one:
**100** front page, **101+** search results, **2xx** recently watched, **199** help.

### Picture styles

Press **m** to cycle through them:

- **HD**: a real picture. Needs cmux, Ghostty, kitty or WezTerm.
- **MOSAIC**: teletext-style blocks in full colour.
- **BLOCKS**: simple half-blocks that work in almost any terminal.
- **CEEFAX**: blocks in the eight colours of 1985 TV.

### Video quality

Videos play at **480p** by default. That's smooth even on a slow connection and
still looks sharp in a terminal. Press **v** while watching to change it; telly
remembers your choice.

---

## "Sign in needed"? (page 403)

On a VPN or office network, YouTube sometimes refuses to play videos until it
knows you're a real person. telly fixes this by borrowing your Chrome sign-in:

1. Make sure you're signed in to YouTube in **Google Chrome**.
2. Open **System Settings → Privacy & Security → Full Disk Access** and switch on
   your terminal app (cmux, Ghostty, Terminal…).
3. Back in telly, press **C**. If macOS asks about "Chrome Safe Storage", choose
   **Always Allow**.

Prefer not to do that? Export a `cookies.txt` file for youtube.com (for example with
the "Get cookies.txt LOCALLY" browser extension), save it as
`~/.config/telly/cookies.txt`, and press **R**.

---

## Settings

telly saves your choices in `~/.config/telly/config`:

```text
quality = 480      # 360, 480, 720 or 1080
mode = hd          # hd, mosaic, blocks or ceefax
browser = chrome   # whose YouTube sign-in to borrow
```

Something not working? Run `telly --selftest` to check search, streaming and decoding.

## Thanks

telly stands on [yt-dlp](https://github.com/yt-dlp/yt-dlp) and [FFmpeg](https://ffmpeg.org),
and its look on BBC Ceefax (1974–2012).
