package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"html/template"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxRunning = 16
	termCols   = "200" // wide, so Claude never breaks a link or path across lines
	termRows   = "50"
)

// pollEvery is how often a process's screen and state are read from tmux.
var pollEvery = 400 * time.Millisecond

var (
	urlRe     = regexp.MustCompile(`https://[^\s"'<>]+`)
	linkRe    = regexp.MustCompile(`https://claude\.ai/code\?environment=[A-Za-z0-9_]+`)
	trustRe   = regexp.MustCompile(`(?i)^\s*(do you )?trust .+\?`)
	loginRe   = regexp.MustCompile(`(?i)must be logged in|claude auth login|please run /login`)
	consentRe = regexp.MustCompile(`(?i)enable remote control\?`)
	askRe     = regexp.MustCompile(`(?i)(\[y/n\]|\(y/n\))\s*:?\s*\S{0,3}\s*$`)
	deadRe    = regexp.MustCompile(`(?m)^Pane is dead \(.*\)\s*$`)
)

// childEnv is the whole environment a claude process gets. Nothing is
// inherited implicitly, so one account's variables (or the launcher's own
// CLAUDE_* / ANTHROPIC_* variables) cannot leak into another account's process.
func childEnv(configDir string) []string {
	env := []string{"TERM=screen-256color"}
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

// Run is one claude process. It lives in its own tmux session rather than as
// a child of Superdeck, so it keeps running while Superdeck restarts. Superdeck
// reads its screen from tmux and sends it keys through tmux.
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
	dir      string
	mgr      *Manager
	onExit   func(*Run)

	mu       sync.Mutex
	text     string // the screen and recent scrollback, as plain text
	final    string // the screen as it was when the process exited, with colour codes
	link     string // where Claude says this instance can be opened
	stopped  bool   // someone asked for this process to stop
	done     bool
	exitCode int
}

func (r *Run) session() string { return "sd-" + r.ID }

// RunView is a snapshot of a Run's changing state.
type RunView struct {
	*Run
	Done     bool
	ExitCode int
	Stopped  bool
	Hint     string // "needs-trust", "needs-consent", "needs-answer", "needs-login" or ""
	Question string // Claude's own words, for a yes/no question this program does not know
	URL      string // sign-in page, for a login that is waiting
	Link     string // claude.ai address of a connected instance
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

func (r *Run) View() RunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := RunView{Run: r, Done: r.done, ExitCode: r.exitCode, Stopped: r.stopped}
	// What matters is what is on the screen now, at the bottom: a question is
	// only a question while it is the last thing Claude printed.
	// An earlier, answered question may still be visible above it, so the
	// kind of question is read from that last line alone.
	recent := lastLines(r.text, 12)
	last := lastLines(r.text, 1)
	switch {
	case !r.done && askRe.MatchString(last) && trustRe.MatchString(last):
		v.Hint = "needs-trust"
	case !r.done && askRe.MatchString(last) && consentRe.MatchString(last):
		v.Hint = "needs-consent"
	case !r.done && askRe.MatchString(last):
		v.Hint, v.Question = "needs-answer", lastLines(r.text, 10)
	case r.done && strings.Contains(strings.ToLower(recent), "workspace not trusted"):
		v.Hint = "needs-trust"
	case r.Kind == "server" && loginRe.MatchString(recent):
		v.Hint = "needs-login"
	}
	if r.Kind == "login" && !r.done {
		if m := urlRe.FindAllString(r.text, -1); len(m) > 0 {
			v.URL = m[len(m)-1]
		}
	}
	if !r.done {
		v.Link = r.link
	}
	return v
}

// Tail is the screen and recent scrollback as plain text.
func (r *Run) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.text
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

// Type sends literal text to the process, as if typed.
func (r *Run) Type(text string) error {
	if !r.alive() {
		return errors.New("that process has already exited")
	}
	_, err := r.mgr.tmux("send-keys", "-t", r.session(), "-l", "--", text)
	return err
}

// Press sends one named key: Enter, Up, Down or Escape.
func (r *Run) Press(key string) error {
	if !r.alive() {
		return errors.New("that process has already exited")
	}
	_, err := r.mgr.tmux("send-keys", "-t", r.session(), key)
	return err
}

// Stop asks the whole process group to exit, then kills it if it lingers.
func (r *Run) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.mgr.onStop != nil {
		r.mgr.onStop(r)
	}
	if !r.alive() {
		return
	}
	pid := r.pid()
	if pid <= 1 {
		r.mgr.tmux("kill-session", "-t", r.session())
		return
	}
	syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		if r.alive() {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

func (r *Run) pid() int {
	out, err := r.mgr.tmux("display-message", "-p", "-t", r.session(), "#{pane_pid}")
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(out))
	return pid
}

// read refreshes this run's state from tmux. It returns false once the
// process has exited.
func (r *Run) read() bool {
	state, err := r.mgr.tmux("display-message", "-p", "-t", r.session(), "#{pane_dead}|#{pane_dead_status}")
	gone := err != nil // the session vanished, for example because tmux itself was killed
	dead := gone || strings.HasPrefix(state, "1|")
	var text string
	if !gone {
		text, _ = r.mgr.tmux("capture-pane", "-p", "-J", "-t", r.session(), "-S", "-300")
		text = deadRe.ReplaceAllString(text, "")
	}
	var final string
	code := -1
	if dead && !gone {
		// Include scrollback: when a pane dies tmux pushes its top line off
		// the screen to make room for its own "Pane is dead" notice.
		final, _ = r.mgr.tmux("capture-pane", "-p", "-e", "-J", "-t", r.session(), "-S", "-300")
		final = deadRe.ReplaceAllString(final, "")
		if f := strings.SplitN(strings.TrimSpace(state), "|", 2); len(f) == 2 {
			if n, err := strconv.Atoi(f[1]); err == nil {
				code = n
			}
		}
	}
	r.mu.Lock()
	if !gone {
		r.text = text
	}
	if r.Kind == "server" {
		if m := linkRe.FindAllString(r.text, -1); len(m) > 0 {
			r.link = m[len(m)-1]
		}
	}
	if dead {
		r.done, r.exitCode, r.final = true, code, final
	}
	r.mu.Unlock()
	return !dead
}

// watch follows a process until it exits.
func (r *Run) watch() {
	for r.read() {
		time.Sleep(pollEvery)
	}
	r.mgr.tmux("kill-session", "-t", r.session())
	if r.mgr.onEnd != nil {
		r.mgr.onEnd(r)
	}
	if r.onExit != nil {
		r.onExit(r)
	}
}

// Screen renders what the process is showing as HTML. Every character from the
// process is escaped; the only markup is colour spans whose values are
// generated here.
func (r *Run) Screen() template.HTML {
	r.mu.Lock()
	done, raw := r.done, r.final
	r.mu.Unlock()
	if !done {
		raw, _ = r.mgr.tmux("capture-pane", "-p", "-e", "-J", "-t", r.session())
	}
	return ansiToHTML(strings.TrimRight(raw, "\n "))
}

// palette is the 16 base terminal colours, tuned for the page's dark theme.
var palette = [16]string{
	"#3a312a", "#ff6b5e", "#6fdc8c", "#ffb224", "#7aa7ff", "#d58cff", "#5fd3d3", "#d9d0c5",
	"#75695e", "#ff8a80", "#93e8a8", "#ffc85c", "#9dbdff", "#e2adff", "#8be3e3", "#ffffff",
}

func colour256(n int) string {
	switch {
	case n < 0 || n > 255:
		return ""
	case n < 16:
		return palette[n]
	case n < 232:
		n -= 16
		lv := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + 40*v
		}
		return fmt.Sprintf("#%02x%02x%02x", lv(n/36), lv(n/6%6), lv(n%6))
	}
	g := 8 + 10*(n-232)
	return fmt.Sprintf("#%02x%02x%02x", g, g, g)
}

var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// ansiToHTML turns text with terminal colour codes into escaped HTML with
// colour spans. Any other escape sequence is dropped.
func ansiToHTML(s string) template.HTML {
	var b strings.Builder
	fg, bg, bold, open := "", "", false, false
	emit := func() {
		if open {
			b.WriteString("</span>")
			open = false
		}
		st := ""
		if fg != "" {
			st += "color:" + fg + ";"
		}
		if bg != "" {
			st += "background:" + bg + ";"
		}
		if bold {
			st += "font-weight:700;"
		}
		if st != "" {
			b.WriteString(`<span style="` + st + `">`)
			open = true
		}
	}
	pos := 0
	for _, m := range sgrRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(html.EscapeString(s[pos:m[0]]))
		pos = m[1]
		if m[2] < 0 { // not a colour code
			continue
		}
		var codes []int
		for _, f := range strings.Split(s[m[2]:m[3]], ";") {
			n, _ := strconv.Atoi(f)
			codes = append(codes, n)
		}
		for i := 0; i < len(codes); i++ {
			c := codes[i]
			switch {
			case c == 0:
				fg, bg, bold = "", "", false
			case c == 1:
				bold = true
			case c == 22:
				bold = false
			case c >= 30 && c <= 37:
				fg = palette[c-30]
			case c >= 90 && c <= 97:
				fg = palette[c-90+8]
			case c == 39:
				fg = ""
			case c >= 40 && c <= 47:
				bg = palette[c-40]
			case c >= 100 && c <= 107:
				bg = palette[c-100+8]
			case c == 49:
				bg = ""
			case (c == 38 || c == 48) && i+2 < len(codes) && codes[i+1] == 5:
				if col := colour256(codes[i+2]); c == 38 {
					fg = col
				} else {
					bg = col
				}
				i += 2
			case (c == 38 || c == 48) && i+4 < len(codes) && codes[i+1] == 2:
				col := fmt.Sprintf("#%02x%02x%02x", codes[i+2]&255, codes[i+3]&255, codes[i+4]&255)
				if c == 38 {
					fg = col
				} else {
					bg = col
				}
				i += 4
			}
		}
		emit()
	}
	b.WriteString(html.EscapeString(s[pos:]))
	if open {
		b.WriteString("</span>")
	}
	return template.HTML(b.String())
}

// Manager owns every Run and the tmux server that holds their processes. It
// only ever starts the fixed command shapes built in instances.go; no request
// can hand it a command.
type Manager struct {
	bin, socket string
	onStart     func(*Run) // record a new process
	onStop      func(*Run) // record that a person stopped one
	onEnd       func(*Run) // record that one exited

	mu   sync.Mutex
	runs map[string]*Run
}

// NewManager uses the tmux binary at bin and a private tmux server reached
// through socket, separate from any tmux the person uses themselves.
func NewManager(bin, socket string) *Manager {
	// A Unix socket path has a short length limit.
	if len(socket) > 90 {
		sum := sha256.Sum256([]byte(socket))
		socket = "/tmp/superdeck-" + hex.EncodeToString(sum[:6]) + ".sock"
	}
	return &Manager{bin: bin, socket: socket, runs: map[string]*Run{}}
}

func (m *Manager) tmux(args ...string) (string, error) {
	cmd := exec.Command(m.bin, append([]string{"-S", m.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Holding reports whether tmux still has a session for this run.
func (m *Manager) Holding(r *Run) bool {
	_, err := m.tmux("has-session", "-t", r.session())
	return err == nil
}

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

// check refuses a second live run of the same kind for the same account and
// directory, so one account cannot run two servers over one working tree.
func (m *Manager) check(r *Run) error {
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
	return nil
}

// Start launches argv in a new tmux session. The arguments are passed to tmux
// as separate arguments, which it executes directly: no shell is involved.
// `env -i` gives the process exactly the environment in env and nothing else.
func (m *Manager) Start(r *Run, argv, env []string, onExit func(*Run)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(r); err != nil {
		return err
	}
	idb := make([]byte, 6)
	rand.Read(idb)
	r.ID = hex.EncodeToString(idb)
	r.Started = time.Now()
	r.mgr, r.onExit = m, onExit
	args := []string{
		"start-server", ";",
		"set", "-g", "remain-on-exit", "on", ";", // keep the screen and exit status of a finished process
		"set", "-g", "status", "off", ";",
		"set", "-g", "history-limit", "2000", ";",
		"new-session", "-d", "-s", r.session(), "-x", termCols, "-y", termRows, "-c", r.dir, "--",
		"/usr/bin/env", "-i",
	}
	args = append(args, env...)
	args = append(args, argv...)
	if _, err := m.tmux(args...); err != nil {
		return err
	}
	m.runs[r.ID] = r
	if m.onStart != nil {
		m.onStart(r)
	}
	go r.watch()
	return nil
}

// Adopt takes over a run that was started before Superdeck restarted and is
// still held by tmux.
func (m *Manager) Adopt(r *Run, onExit func(*Run)) {
	m.mu.Lock()
	r.mgr, r.onExit = m, onExit
	m.runs[r.ID] = r
	m.mu.Unlock()
	r.read()
	go r.watch()
}

// StopAll stops every process and the tmux server. Superdeck does not call
// this when it exits: leaving processes running is the point. Tests do.
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
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.tmux("kill-server")
}
