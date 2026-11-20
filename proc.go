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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hinshun/vt10x"
)

const (
	maxRunning = 16  // instances
	maxLogins  = 3   // sign-ins in progress; kept apart so they cannot crowd out instances
	termCols   = 200 // wide, so Claude never breaks a link or path across lines
	termRows   = 50
)

// pollEvery is how often a process's output and state are read from its
// holder. startGrace is how long a new process gets before a missing holder
// is believed. Tests shorten both.
var (
	pollEvery  = 400 * time.Millisecond
	startGrace = 5 * time.Second
)

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
// paths have a short length limit, so a long state path falls back to a
// directory under the home directory. It is not put in a temporary directory:
// those get cleaned, and a holder whose socket has been cleaned away can no
// longer be reached.
func runDir(state string) string {
	dir := filepath.Join(state, "run")
	if len(dir) > 70 {
		sum := sha256.Sum256([]byte(state))
		name := "superdeck-" + hex.EncodeToString(sum[:6])
		if home, err := os.UserHomeDir(); err == nil && len(home) < 50 {
			return filepath.Join(home, ".superdeck-run", name)
		}
		return filepath.Join(os.TempDir(), name)
	}
	return dir
}

// ensureRunDir creates the socket directory and refuses one that somebody
// else could have set up: it must be a real directory, owned by this user,
// and closed to everyone else.
func ensureRunDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory owned by you; refusing to use it", dir)
	}
	return os.Chmod(dir, 0o700)
}

func processAlive(pid int) bool { return pid > 1 && syscall.Kill(pid, 0) == nil }

// holderAlive reports whether the process with this id is the holder for
// this socket. The id alone is not trusted: after a reboot it may belong to
// something else entirely.
func holderAlive(hpid int, sock string) bool {
	if !processAlive(hpid) {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(hpid)).Output()
	return err == nil && strings.Contains(string(out), sock)
}

// parentOf returns the parent process id of pid, or 0.
func parentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
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

	readMu sync.Mutex // one reader at a time; see read

	mu        sync.Mutex
	slow      int       // polls in a row that got no answer at all
	orphan    time.Time // when its holder was found dead with the process still alive
	stopGen   int       // changes when a stop is taken back
	final     template.HTML
	finalSet  bool
	urls      string    // recent output with terminal codes removed and lines unwrapped
	fails     int       // polls in a row that found nothing listening
	hpid      int       // the holder's own process id
	ended     time.Time // when it was found to have exited
	restartAt time.Time // when an automatic restart is due, if one is
	next      string    // id of the run that replaced this one
	vt        vt10x.Terminal
	vtBroken  bool   // the emulator choked on this output; fall back to plain text
	raw       []byte // recent raw output, for that fallback
	seq       int64  // how much of the holder's output has been read
	pid       int
	text      string // what is on the screen, as plain text
	link      string // where Claude says this instance can be opened
	stopped   bool   // someone asked for this process to stop
	done      bool
	exitCode  int
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
	Ended    time.Time
	// RestartAt is when an automatic restart is due; Next is the run that
	// replaced this one once it happened.
	RestartAt time.Time
	Next      string
	// Lost: the process is believed alive but its holder is not answering.
	Lost bool
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
	v := RunView{Run: r, Done: r.done, ExitCode: r.exitCode, Stopped: r.stopped,
		Ended: r.ended, RestartAt: r.restartAt, Next: r.next,
		Lost: !r.done && (r.fails >= 8 || r.slow >= 8)}
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
		// Read from the raw output, not the screen: a long address wraps
		// across several screen lines and would be cut at the first.
		v.URL = signInURL(r.urls)
	}
	if !r.done {
		v.Link = r.link
	}
	return v
}

// signInURL finds Anthropic's sign-in address in a process's output. Only an
// https address on one of Anthropic's own domains counts: the page offers it
// as a button, so it must not be whatever link happened to be printed.
func signInURL(text string) string {
	found := ""
	for _, raw := range urlRe.FindAllString(text, -1) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		for _, d := range []string{"claude.ai", "claude.com", "anthropic.com"} {
			if host == d || strings.HasSuffix(host, "."+d) {
				found = raw
			}
		}
	}
	return found
}

// setRestart records that an automatic restart is due at t (zero clears it).
func (r *Run) setRestart(t time.Time) {
	r.mu.Lock()
	r.restartAt = t
	r.mu.Unlock()
}

// setNext records the run that replaced this one.
func (r *Run) setNext(id string) {
	r.mu.Lock()
	r.next, r.restartAt = id, time.Time{}
	r.mu.Unlock()
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
	r.restartAt = time.Time{}
	gen := r.stopGen
	r.mu.Unlock()
	if r.mgr.onStop != nil {
		r.mgr.onStop(r)
	}
	// A stop must not be lost because the holder was busy, or had not quite
	// started yet. Keep asking until it has heard or the process is gone.
	go func() {
		for i := 0; i < 60 && r.alive(); i++ {
			r.mu.Lock()
			cancelled := r.stopGen != gen
			hpid := r.hpid
			r.mu.Unlock()
			if cancelled {
				return
			}
			if holdSay(r.sock(), "stop") == nil {
				return
			}
			// A person asked for this, so if the holder cannot be reached
			// over its socket, tell it directly. It stops its process and
			// leaves a record.
			if i == 8 && holderAlive(hpid, r.sock()) {
				syscall.Kill(hpid, syscall.SIGTERM)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
}

// unstop takes back a stop that could not be carried out, so the run is
// supervised again.
func (r *Run) unstop() {
	r.mu.Lock()
	r.stopped = false
	r.stopGen++
	r.mu.Unlock()
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

// end marks the run as exited and lets go of everything only a live run
// needs. Call with r.mu held.
func (r *Run) end(code int) {
	r.done, r.exitCode, r.ended = true, code, time.Now()
	r.final, r.finalSet = r.render(), true
	r.vt, r.raw = nil, nil
}

// read refreshes this run's state from its holder. It returns false once the
// process has exited. Only one read runs at a time: two interleaved readers
// would each replay the same output into the emulator.
func (r *Run) read() bool {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	r.mu.Lock()
	since, done := r.seq, r.done
	r.mu.Unlock()
	if done {
		return false
	}
	rep, data, err := holdSnap(r.sock(), since)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		return r.unreachable(err)
	}
	r.fails, r.slow, r.orphan = 0, 0, time.Time{}
	r.feed(data, rep.Reset)
	r.seq, r.pid = rep.Seq, rep.Pid
	if rep.Hpid > 0 && rep.Hpid != r.hpid {
		r.hpid = rep.Hpid
		if r.mgr.onHolder != nil {
			go r.mgr.onHolder(r.ID, rep.Hpid)
		}
	}
	r.text = r.screenText()
	r.urls = strings.NewReplacer("\r\n", "\n", "\r", "").Replace(escRe.ReplaceAllString(string(r.raw), ""))
	if r.Kind == "server" {
		if m := linkRe.FindAllString(r.urls, -1); len(m) > 0 {
			r.link = m[len(m)-1]
		}
	}
	if !rep.Alive {
		r.end(rep.Code)
		return false
	}
	return true
}

// unreachable decides what a failed poll means. The rule is simple and
// one-sided: a process is never signalled while its holder is alive. A holder
// that is slow, frozen, swamped or cut off from its socket is left alone, for
// as long as it takes; killing a healthy Claude over a holder that would have
// answered is the worst thing Superdeck can do. Call with r.mu held.
func (r *Run) unreachable(err error) bool {
	base := strings.TrimSuffix(r.sock(), ".sock")
	// An exit record means it ended in an orderly way and says how.
	if b, ferr := os.ReadFile(base + ".exit"); ferr == nil {
		code := -1
		var e holdExit
		if json.Unmarshal(b, &e) == nil {
			code = e.Code
		}
		if out, oerr := os.ReadFile(base + ".out"); oerr == nil {
			r.feed(out, true)
			r.text = r.screenText()
		}
		r.end(code)
		return false
	}
	// A timeout or any other error says nothing about whether it is alive.
	if !holdGone(err) {
		r.slow++
		return true
	}
	// Nothing answered on its socket. That alone proves little: a frozen
	// holder with a full queue refuses connections too.
	r.fails++
	if r.fails < 4 || time.Since(r.Started) < startGrace {
		return true
	}
	// Holders started by an older Superdeck do not say who they are; the
	// process's parent is its holder.
	if r.hpid == 0 && processAlive(r.pid) {
		r.hpid = parentOf(r.pid)
	}
	if holderAlive(r.hpid, r.sock()) {
		return true // alive but not answering: leave it be
	}
	// Nothing at all is known about it (never reached since Superdeck
	// started). Give it a good while before believing it is gone.
	if r.hpid == 0 && r.pid == 0 && r.fails < 150 {
		return true
	}
	// The holder is gone and left no record: it crashed, or the Mac
	// restarted. If the process it held is somehow still alive it must not
	// live on unsupervised beside its replacement, so it is ended, firmly if
	// need be, and the run is not over until it is.
	if processAlive(r.pid) {
		switch {
		case r.orphan.IsZero():
			r.orphan = time.Now()
			syscall.Kill(-r.pid, syscall.SIGTERM)
		case time.Since(r.orphan) > 5*time.Second:
			syscall.Kill(-r.pid, syscall.SIGKILL)
		}
		return true
	}
	r.end(-1)
	return false
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
func (r *Run) Screen() template.HTML {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalSet {
		return r.final
	}
	return r.render()
}

// render draws the emulator's screen. Call with r.mu held.
func (r *Run) render() (out template.HTML) {
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
	onHolder func(id string, hpid int) // record which process is holding a run

	self    string           // this program, re-run as `superdeck hold`
	dir     string           // where holders keep their sockets
	log     string           // where holders write their own errors
	onStart func(*Run) error // record a new process; an error cancels the launch
	onStop  func(*Run)       // record that a person stopped one
	onEnd   func(*Run)       // record that one exited

	mu   sync.Mutex
	runs map[string]*Run
}

func NewManager(self, state string) *Manager {
	return &Manager{self: self, dir: runDir(state), log: filepath.Join(state, "holders.log"), runs: map[string]*Run{}}
}

// Probe asks about a run that was started before Superdeck restarted:
// "held" (still being kept alive), "ended" (it exited and left a record, with
// its exit code) or "gone" (nothing is there). It takes a lot to be believed
// gone: no exit record, nothing listening after several tries, and no holder
// process. Anything short of that counts as held.
func (m *Manager) Probe(r *Run) (state string, code int) {
	r.mgr = m
	base := strings.TrimSuffix(r.sock(), ".sock")
	for try := 0; try < 5; try++ {
		_, _, err := holdSnap(r.sock(), 1<<62)
		if err == nil {
			return "held", 0
		}
		if b, ferr := os.ReadFile(base + ".exit"); ferr == nil {
			var e holdExit
			code = -1
			if json.Unmarshal(b, &e) == nil {
				code = e.Code
			}
			return "ended", code
		}
		if !holdGone(err) {
			return "held", 0
		}
		time.Sleep(150 * time.Millisecond)
	}
	if holderAlive(r.hpid, r.sock()) || m.someoneHolds(r.sock()) {
		return "held", 0
	}
	return "gone", -1
}

// someoneHolds reports whether any process is a holder for this socket.
func (m *Manager) someoneHolds(sock string) bool {
	return exec.Command("pgrep", "-f", "hold -socket "+sock).Run() == nil
}

// Forget removes whatever a finished holder left on disk. It never removes
// the socket of a holder that is still running: that would cut it off.
func (m *Manager) Forget(id string) {
	base := filepath.Join(m.dir, id)
	for _, ext := range []string{".exit", ".out", ".exit.tmp", ".out.tmp"} {
		os.Remove(base + ext)
	}
	if !m.someoneHolds(base + ".sock") {
		os.Remove(base + ".sock")
	}
}

// Leftovers lists the ids of everything in the socket directory.
func (m *Manager) Leftovers() []string {
	es, _ := os.ReadDir(m.dir)
	seen := map[string]bool{}
	var ids []string
	for _, e := range es {
		id, _, _ := strings.Cut(e.Name(), ".")
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
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
// directory, so one account cannot run two servers over one working tree, and
// enforces the limits. Call with m.mu held.
func (m *Manager) check(r *Run) error {
	servers, logins := 0, 0
	var dead []RunView
	for id, o := range m.runs {
		v := o.View()
		if v.Done {
			if v.RestartAt.IsZero() {
				dead = append(dead, v)
			}
			// Forget finished runs a day after they finished, not a day
			// after they started: a long-lived instance that just crashed
			// must stay visible.
			if !v.Ended.IsZero() && time.Since(v.Ended) > 24*time.Hour && v.RestartAt.IsZero() {
				delete(m.runs, id)
			}
			continue
		}
		switch o.Kind {
		case "server":
			servers++
		case "login":
			logins++
		}
		if o.Kind == r.Kind && o.Account == r.Account && o.dir == r.dir {
			return errors.New("that is already running")
		}
	}
	// However they got there, only so many finished runs are kept.
	if len(dead) > 40 {
		sort.Slice(dead, func(i, j int) bool { return dead[i].Ended.Before(dead[j].Ended) })
		for _, v := range dead[:len(dead)-40] {
			delete(m.runs, v.ID)
		}
	}
	if r.Kind == "server" && servers >= maxRunning {
		return errors.New("too many instances are running; stop one first")
	}
	if r.Kind == "login" && logins >= maxLogins {
		return errors.New("too many sign-ins are in progress; finish or cancel one first")
	}
	return nil
}

// Start launches argv under a new holder. The holder is this same program,
// started in its own session so that it does not die with Superdeck. It is
// given argv as separate arguments and env as its whole environment; no shell
// is involved.
func (m *Manager) Start(r *Run, argv, env []string, onExit func(*Run)) error {
	idb := make([]byte, 6)
	rand.Read(idb)
	r.ID = hex.EncodeToString(idb)
	r.Started = time.Now()
	r.mgr, r.onExit = m, onExit

	// Reserve its place under the lock, then do the slow part without it, so
	// that pages keep loading while a process starts.
	m.mu.Lock()
	err := m.check(r)
	if err == nil {
		err = ensureRunDir(m.dir)
	}
	if err == nil {
		m.runs[r.ID] = r
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	fail := func(err error) error {
		m.mu.Lock()
		delete(m.runs, r.ID)
		m.mu.Unlock()
		holdSay(r.sock(), "stop")
		time.AfterFunc(10*time.Second, func() { m.Forget(r.ID) }) // once it has had time to go
		return err
	}

	args := append([]string{"hold", "-socket", r.sock(), "-dir", r.dir, "-log", m.log,
		"-cols", fmt.Sprint(termCols), "-rows", fmt.Sprint(termRows), "--"}, argv...)
	cmd := exec.Command(m.self, args...)
	cmd.Env = env
	cmd.Dir = r.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
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
			return fail(errors.New("the process could not be started"))
		}
	}
	// A process Superdeck has no record of could never be found again after
	// a restart, so one that cannot be recorded is not allowed to run.
	if m.onStart != nil {
		if err := m.onStart(r); err != nil {
			return fail(fmt.Errorf("could not record the new process: %w", err))
		}
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
// leaving processes running is the point. Tests do. It keeps going until
// nothing is left, because a restart that was already under way can add a run
// after the first pass.
func (m *Manager) StopAll() {
	asked := map[string]bool{}
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		m.mu.Lock()
		var live []*Run
		for _, r := range m.runs {
			if r.alive() {
				live = append(live, r)
			}
		}
		m.mu.Unlock()
		if len(live) == 0 {
			return
		}
		for _, r := range live {
			if !asked[r.ID] {
				asked[r.ID] = true
				r.Stop()
			}
		}
	}
}
