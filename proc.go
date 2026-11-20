package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hinshun/vt10x"
)

const (
	maxRunning = 16
	termCols   = 200 // wide, so Claude never breaks a link or path across lines
	termRows   = 50
)

// pollEvery is how often a process's output and state are read from its holder.
var pollEvery = 400 * time.Millisecond

var (
	urlRe     = regexp.MustCompile(`https://[^\s"'<>]+`)
	linkRe    = regexp.MustCompile(`https://claude\.ai/code\?environment=[A-Za-z0-9_]+`)
	trustRe   = regexp.MustCompile(`(?i)^\s*(do you )?trust .+\?`)
	loginRe   = regexp.MustCompile(`(?i)must be logged in|claude auth login|please run /login`)
	consentRe = regexp.MustCompile(`(?i)enable remote control\?`)
	askRe     = regexp.MustCompile(`(?i)(\[y/n\]|\(y/n\))\s*:?\s*\S{0,3}\s*$`)
	escRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
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

// runDir is where holders keep their sockets and exit records. Unix socket
// paths have a short length limit, so a long state path falls back to /tmp.
func runDir(state string) string {
	dir := filepath.Join(state, "run")
	if len(dir) > 70 {
		sum := sha256.Sum256([]byte(state))
		dir = "/tmp/superdeck-" + hex.EncodeToString(sum[:6])
	}
	return dir
}

// Run is one claude process. It is kept alive by a holder (see hold.go), not
// by Superdeck, so it carries on while Superdeck restarts. Superdeck reads its
// output from the holder and replays it through a terminal emulator to know
// what is on its screen.
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
	vt       vt10x.Terminal
	vtBroken bool   // the emulator choked on this output; fall back to plain text
	raw      []byte // recent raw output, for that fallback
	seq      int64  // how much of the holder's output has been read
	pid      int
	text     string // what is on the screen, as plain text
	link     string // where Claude says this instance can be opened
	stopped  bool   // someone asked for this process to stop
	done     bool
	exitCode int
}

func (r *Run) sock() string { return filepath.Join(r.mgr.dir, r.ID+".sock") }

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
	// only a question while it is the last thing Claude printed. An earlier,
	// answered question may still be visible above it, so the kind of
	// question is read from that last line alone.
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

// Tail is what is on the process's screen, as plain text.
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

// Pid is the process id of the Claude process.
func (r *Run) Pid() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pid
}

// Type sends literal text to the process, as if typed.
func (r *Run) Type(text string) error {
	if !r.alive() {
		return errors.New("that process has already exited")
	}
	return holdSay(r.sock(), "in "+base64.StdEncoding.EncodeToString([]byte(text)))
}

var namedKeys = map[string]string{"Enter": "\r", "Up": "\x1b[A", "Down": "\x1b[B", "Escape": "\x1b"}

// Press sends one named key: Enter, Up, Down or Escape.
func (r *Run) Press(key string) error {
	seq, ok := namedKeys[key]
	if !ok {
		return errors.New("that key is not allowed")
	}
	return r.Type(seq)
}

// Stop asks the process to exit. The holder sends the signals and escalates
// to a kill if the process lingers.
func (r *Run) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.mgr.onStop != nil {
		r.mgr.onStop(r)
	}
	if r.alive() {
		holdSay(r.sock(), "stop")
	}
}

// feed passes output to the terminal emulator. The emulator is third-party
// code parsing whatever a process prints, so a panic in it must never take
// Superdeck down. If it does panic, this run falls back to plain text.
// Call with r.mu held.
func (r *Run) feed(b []byte, reset bool) {
	defer func() {
		if recover() != nil {
			r.vtBroken = true
		}
	}()
	if reset || r.vt == nil {
		r.vt = vt10x.New(vt10x.WithSize(termCols, termRows), vt10x.WithWriter(io.Discard))
		r.raw = nil
	}
	r.raw = append(r.raw, b...)
	if len(r.raw) > 64<<10 {
		r.raw = append([]byte(nil), r.raw[len(r.raw)-(64<<10):]...)
	}
	if !r.vtBroken {
		r.vt.Write(b)
	}
}

// screenText reads the emulator's screen as plain text. Call with r.mu held.
func (r *Run) screenText() (text string) {
	plain := func() string {
		return lastLines(strings.ReplaceAll(escRe.ReplaceAllString(string(r.raw), ""), "\r", ""), termRows)
	}
	if r.vtBroken || r.vt == nil {
		return plain()
	}
	defer func() {
		if recover() != nil {
			r.vtBroken = true
			text = plain()
		}
	}()
	r.vt.Lock()
	defer r.vt.Unlock()
	cols, rows := r.vt.Size()
	lines := make([]string, 0, rows)
	for y := 0; y < rows; y++ {
		var b strings.Builder
		for x := 0; x < cols; x++ {
			ch := r.vt.Cell(x, y).Char
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// read refreshes this run's state from its holder. It returns false once the
// process has exited.
func (r *Run) read() bool {
	r.mu.Lock()
	since := r.seq
	r.mu.Unlock()
	rep, data, err := holdSnap(r.sock(), since)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return false
	}
	if err != nil {
		// The holder is not answering. If it left an exit record, the process
		// ended normally; otherwise holder and process are simply gone, as
		// after a reboot.
		code := -1
		base := strings.TrimSuffix(r.sock(), ".sock")
		if b, ferr := os.ReadFile(base + ".exit"); ferr == nil {
			var e holdExit
			if json.Unmarshal(b, &e) == nil {
				code = e.Code
			}
			if out, oerr := os.ReadFile(base + ".out"); oerr == nil {
				r.feed(out, true)
				r.text = r.screenText()
			}
		} else if time.Since(r.Started) < 3*time.Second {
			return true // still starting up; its socket is not there yet
		} else if r.pid > 1 {
			// The holder died without a word. Make sure the process it was
			// holding does not live on unsupervised beside its replacement.
			syscall.Kill(-r.pid, syscall.SIGTERM)
		}
		r.done, r.exitCode = true, code
		return false
	}
	r.feed(data, rep.Reset)
	r.seq, r.pid = rep.Seq, rep.Pid
	r.text = r.screenText()
	if r.Kind == "server" {
		if m := linkRe.FindAllString(r.text, -1); len(m) > 0 {
			r.link = m[len(m)-1]
		}
	}
	if !rep.Alive {
		r.done, r.exitCode = true, rep.Code
		return false
	}
	return true
}

// watch follows a process until it exits.
func (r *Run) watch() {
	for r.read() {
		time.Sleep(pollEvery)
	}
	base := strings.TrimSuffix(r.sock(), ".sock")
	for _, ext := range []string{".exit", ".out", ".sock"} {
		os.Remove(base + ext)
	}
	if r.mgr.onEnd != nil {
		r.mgr.onEnd(r)
	}
	if r.onExit != nil {
		r.onExit(r)
	}
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

// Screen renders what the process is showing as HTML. Every character from the
// process is escaped; the only markup is colour spans whose values are
// generated here.
func (r *Run) Screen() (out template.HTML) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plain := func() template.HTML { return template.HTML(html.EscapeString(r.screenText())) }
	if r.vtBroken || r.vt == nil {
		return plain()
	}
	defer func() {
		if recover() != nil {
			r.vtBroken = true
			out = plain()
		}
	}()
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

// Manager owns every Run. It only ever starts the fixed command shapes built
// in instances.go; no request can hand it a command.
type Manager struct {
	self    string     // this program, re-run as `superdeck hold`
	dir     string     // where holders keep their sockets
	onStart func(*Run) // record a new process
	onStop  func(*Run) // record that a person stopped one
	onEnd   func(*Run) // record that one exited

	mu   sync.Mutex
	runs map[string]*Run
}

func NewManager(self, state string) *Manager {
	return &Manager{self: self, dir: runDir(state), runs: map[string]*Run{}}
}

// Holding reports whether a holder is still answering for this run.
func (m *Manager) Holding(r *Run) bool {
	r.mgr = m
	_, _, err := holdSnap(r.sock(), 1<<62)
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

// Start launches argv under a new holder. The holder is this same program,
// started in its own session so that it does not die with Superdeck. It is
// given argv as separate arguments and env as its whole environment; no shell
// is involved.
func (m *Manager) Start(r *Run, argv, env []string, onExit func(*Run)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(r); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	idb := make([]byte, 6)
	rand.Read(idb)
	r.ID = hex.EncodeToString(idb)
	r.Started = time.Now()
	r.mgr, r.onExit = m, onExit

	args := append([]string{"hold", "-socket", r.sock(), "-dir", r.dir,
		"-cols", fmt.Sprint(termCols), "-rows", fmt.Sprint(termRows), "--"}, argv...)
	cmd := exec.Command(m.self, args...)
	cmd.Env = env
	cmd.Dir = r.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it ends while Superdeck is still here

	// Wait for the holder to be reachable, or to have already finished.
	base := strings.TrimSuffix(r.sock(), ".sock")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, _, err := holdSnap(r.sock(), 1<<62); err == nil {
			break
		}
		if _, err := os.Stat(base + ".exit"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			return errors.New("the process could not be started")
		}
	}
	m.runs[r.ID] = r
	if m.onStart != nil {
		m.onStart(r)
	}
	go r.watch()
	return nil
}

// Adopt takes over a run that was started before Superdeck restarted and is
// still held.
func (m *Manager) Adopt(r *Run, onExit func(*Run)) {
	m.mu.Lock()
	r.mgr, r.onExit = m, onExit
	m.runs[r.ID] = r
	m.mu.Unlock()
	r.read()
	go r.watch()
}

// StopAll stops every process. Superdeck does not call this when it exits:
// leaving processes running is the point. Tests do.
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
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, r := range rs {
			busy = busy || r.alive()
		}
		if !busy {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
