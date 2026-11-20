package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

const (
	maxTail    = 8 << 10
	maxRunning = 16
)

var (
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	urlRe     = regexp.MustCompile(`https://[^\s"'<>]+`)
	trustRe   = regexp.MustCompile(`(?im)workspace not trusted|do you trust|trust this folder|trust the files|one you trust|^trust .+\? \[y/n\]`)
	askRe     = regexp.MustCompile(`(?i)(\[y/n\]|\(y/n\))\s*:?\s*$`)
	loginRe   = regexp.MustCompile(`(?i)must be logged in|claude auth login|please run /login`)
	consentRe = regexp.MustCompile(`(?i)enable remote control\?`)
)

// childEnv is the whole environment a claude process gets. Nothing is
// inherited implicitly, so one account's variables (or the launcher's own
// CLAUDE_* / ANTHROPIC_* variables) cannot leak into another account's process.
func childEnv(configDir string) []string {
	env := []string{"TERM=xterm-256color"}
	if configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR", "SSH_AUTH_SOCK"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}
	return append(env, "LANG="+lang)
}

// Run is one supervised claude process attached to a pseudo-terminal. Its
// output feeds a terminal emulator held here on the server; pages show that
// emulator's screen as plain HTML.
type Run struct {
	ID      string
	Kind    string // "server", "login" or "update"
	Account string
	Project string
	Name    string
	Started time.Time
	// Version is the Claude Code version this process was started with.
	Version string
	// Restarts counts automatic restarts in a row that led to this process.
	Restarts int

	spawn    string
	capacity int

	dir    string
	cmd    *exec.Cmd
	tty    *os.File
	vt     vt10x.Terminal
	onExit func(*Run)

	mu       sync.Mutex
	tail     string // recent output as plain text
	stopped  bool   // someone asked for this process to stop
	done     bool
	exitCode int
}

// RunView is a snapshot of a Run's changing state.
type RunView struct {
	*Run
	Done     bool
	ExitCode int
	Stopped  bool
	Hint     string // "needs-trust", "needs-consent", "needs-answer", "needs-login" or ""
	Question string // Claude's own words, for a yes/no question this program does not know
	URL      string // sign-in page, for a login that is waiting
}

func (r *Run) View() RunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := RunView{Run: r, Done: r.done, ExitCode: r.exitCode, Stopped: r.stopped}
	recent := r.tail
	if len(recent) > 2048 {
		recent = recent[len(recent)-2048:]
	}
	switch {
	case trustRe.MatchString(recent):
		v.Hint = "needs-trust"
	case consentRe.MatchString(recent):
		v.Hint = "needs-consent"
	case !r.done && askRe.MatchString(strings.TrimRight(recent, " \r\n")):
		v.Hint, v.Question = "needs-answer", lastLines(recent, 10)
	case r.Kind == "server" && loginRe.MatchString(recent):
		v.Hint = "needs-login"
	}
	if r.Kind == "login" && !r.done {
		if m := urlRe.FindAllString(r.tail, -1); len(m) > 0 {
			v.URL = m[len(m)-1]
		}
	}
	return v
}

func (r *Run) append(b []byte) {
	r.vt.Write(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	// A cleared screen means whatever prompt was showing is gone.
	text := string(b)
	if i := strings.LastIndex(text, "\x1b[2J"); i >= 0 {
		r.tail, text = "", text[i:]
	}
	r.tail += ansiRe.ReplaceAllString(text, "")
	if len(r.tail) > maxTail {
		r.tail = r.tail[len(r.tail)-maxTail:]
	}
}

// lastLines returns up to n trailing non-empty lines of text.
func lastLines(text string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, strings.TrimRight(l, " "))
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

// Tail is the recent output as plain text.
func (r *Run) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tail
}

// palette is the 16 base terminal colours, tuned for the page's dark theme.
var palette = [16]string{
	"#3a312a", "#ff6b5e", "#6fdc8c", "#ffb224", "#7aa7ff", "#d58cff", "#5fd3d3", "#d9d0c5",
	"#75695e", "#ff8a80", "#93e8a8", "#ffc85c", "#9dbdff", "#e2adff", "#8be3e3", "#ffffff",
}

func cssColor(c vt10x.Color) string {
	switch {
	case c < 16:
		return palette[c]
	case c < 232:
		n := int(c) - 16
		lv := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + 40*v
		}
		return fmt.Sprintf("#%02x%02x%02x", lv(n/36), lv(n/6%6), lv(n%6))
	case c < 256:
		g := 8 + 10*(int(c)-232)
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	case c < 1<<24:
		return fmt.Sprintf("#%06x", uint32(c))
	}
	return ""
}

// Screen renders the terminal emulator's current screen as HTML. Every
// character from the process is escaped; the only markup is colour spans whose
// values are generated here.
func (r *Run) Screen() template.HTML {
	r.vt.Lock()
	defer r.vt.Unlock()
	cols, rows := r.vt.Size()
	lines := make([]string, 0, rows)
	for y := 0; y < rows; y++ {
		last := -1
		for x := 0; x < cols; x++ {
			g := r.vt.Cell(x, y)
			if (g.Char != ' ' && g.Char != 0) || g.BG != vt10x.DefaultBG {
				last = x
			}
		}
		var b strings.Builder
		cur := ""
		for x := 0; x <= last; x++ {
			g := r.vt.Cell(x, y)
			st := ""
			if c := cssColor(g.FG); c != "" {
				st += "color:" + c + ";"
			}
			if c := cssColor(g.BG); c != "" {
				st += "background:" + c + ";"
			}
			if g.Mode&4 != 0 { // bold
				st += "font-weight:700;"
			}
			if st != cur {
				if cur != "" {
					b.WriteString("</span>")
				}
				if st != "" {
					b.WriteString(`<span style="` + st + `">`)
				}
				cur = st
			}
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			b.WriteString(html.EscapeString(string(ch)))
		}
		if cur != "" {
			b.WriteString("</span>")
		}
		lines = append(lines, b.String())
	}
	for len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return template.HTML(strings.Join(lines, "\n"))
}

func (r *Run) alive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.done
}

func (r *Run) wasStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *Run) Input(data string) error {
	if !r.alive() {
		return errors.New("that process has already exited")
	}
	// Once a prompt has been answered it no longer describes the process.
	r.mu.Lock()
	r.tail = ""
	r.mu.Unlock()
	_, err := r.tty.WriteString(data)
	return err
}

// Resize sets the terminal size, for a phone or a desktop viewer.
func (r *Run) Resize(cols, rows int) {
	if !r.alive() {
		return
	}
	if c, rw := r.vt.Size(); c == cols && rw == rows {
		return
	}
	r.vt.Resize(cols, rows)
	pty.Setsize(r.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Stop asks the whole process group to exit, then kills it if it lingers.
func (r *Run) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if !r.alive() {
		return
	}
	pid := r.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		if r.alive() {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

func (r *Run) pump() {
	b := make([]byte, 8192)
	for {
		n, err := r.tty.Read(b)
		if n > 0 {
			r.append(b[:n])
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := r.cmd.Wait(); err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	r.tty.Close()
	r.mu.Lock()
	r.done, r.exitCode = true, code
	r.mu.Unlock()
	if r.onExit != nil {
		r.onExit(r)
	}
}

// Manager owns every Run. It only ever starts the fixed command shapes built
// in instances.go and web.go; no request can hand it a command.
type Manager struct {
	mu   sync.Mutex
	runs map[string]*Run
}

func NewManager() *Manager { return &Manager{runs: map[string]*Run{}} }

func (m *Manager) Get(id string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// List returns every run, newest first.
func (m *Manager) List() []RunView {
	m.mu.Lock()
	rs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		rs = append(rs, r)
	}
	m.mu.Unlock()
	sort.Slice(rs, func(i, j int) bool { return rs[i].Started.After(rs[j].Started) })
	out := make([]RunView, len(rs))
	for i, r := range rs {
		out[i] = r.View()
	}
	return out
}

// Start launches argv under a pseudo-terminal. A second live run with the same
// kind, account and directory is refused, so one account cannot run two
// servers over the same working tree.
func (m *Manager) Start(r *Run, argv, env []string, onExit func(*Run)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := 0
	for id, o := range m.runs {
		if !o.alive() {
			if time.Since(o.Started) > 24*time.Hour {
				delete(m.runs, id)
			}
			continue
		}
		live++
		if o.Kind == r.Kind && o.Account == r.Account && o.dir == r.dir {
			return errors.New("that is already running")
		}
	}
	if live >= maxRunning {
		return errors.New("too many things are running; stop one first")
	}
	idb := make([]byte, 6)
	rand.Read(idb)
	r.ID = hex.EncodeToString(idb)
	r.Started = time.Now()
	r.onExit = onExit
	r.vt = vt10x.New(vt10x.WithSize(100, 30), vt10x.WithWriter(io.Discard))
	r.cmd = exec.Command(argv[0], argv[1:]...)
	r.cmd.Dir = r.dir
	r.cmd.Env = env
	tty, err := pty.StartWithSize(r.cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		return err
	}
	r.tty = tty
	m.runs[r.ID] = r
	go r.pump()
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	rs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		rs = append(rs, r)
	}
	m.mu.Unlock()
	for _, r := range rs {
		r.Stop()
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, r := range rs {
			busy = busy || r.alive()
		}
		if !busy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
