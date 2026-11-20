package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	maxBuffer  = 256 << 10
	maxTail    = 4 << 10
	maxRunning = 16
)

var (
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	urlRe     = regexp.MustCompile(`https://[^\s"'<>]+`)
	trustRe   = regexp.MustCompile(`(?i)workspace not trusted|do you trust|trust this folder|trust the files`)
	loginRe   = regexp.MustCompile(`(?i)must be logged in|claude auth login|please run /login`)
	consentRe = regexp.MustCompile(`(?i)enable remote control\?`)
)

// childEnv is the whole environment a claude process gets. Nothing is
// inherited implicitly, so one account's variables (or the launcher's own
// CLAUDE_* / ANTHROPIC_* variables) cannot leak into another account's process.
func childEnv(configDir string) []string {
	env := []string{"TERM=xterm-256color", "CLAUDE_CONFIG_DIR=" + configDir}
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

// Run is one supervised claude process attached to a pseudo-terminal.
type Run struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // "login" or "server"
	Account string    `json:"account"`
	Project string    `json:"project,omitempty"`
	Name    string    `json:"name"`
	Started time.Time `json:"started"`

	dir    string
	cmd    *exec.Cmd
	tty    *os.File
	onExit func(*Run)

	mu       sync.Mutex
	buf      []byte
	tail     string
	subs     map[chan []byte]struct{}
	done     bool
	exitCode int
}

// RunView is the JSON shape of a Run.
type RunView struct {
	*Run
	Done     bool   `json:"done"`
	ExitCode int    `json:"exitCode"`
	Hint     string `json:"hint,omitempty"`
	URL      string `json:"url,omitempty"`
}

func (r *Run) View() RunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := RunView{Run: r, Done: r.done, ExitCode: r.exitCode}
	recent := r.tail
	if len(recent) > 2048 {
		recent = recent[len(recent)-2048:]
	}
	switch {
	case trustRe.MatchString(recent):
		v.Hint = "needs-trust"
	case consentRe.MatchString(recent):
		v.Hint = "needs-consent"
	case r.Kind == "server" && loginRe.MatchString(recent):
		v.Hint = "needs-login"
	}
	if r.Kind == "login" {
		if m := urlRe.FindAllString(r.tail, -1); len(m) > 0 {
			v.URL = m[len(m)-1]
		}
	}
	return v
}

func (r *Run) append(b []byte) {
	chunk := append([]byte(nil), b...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, chunk...)
	if len(r.buf) > maxBuffer {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-maxBuffer:]...)
	}
	r.tail += ansiRe.ReplaceAllString(string(chunk), "")
	if len(r.tail) > maxTail {
		r.tail = r.tail[len(r.tail)-maxTail:]
	}
	for ch := range r.subs {
		select {
		case ch <- chunk:
		default: // slow reader: drop rather than stall the process
		}
	}
}

// Subscribe returns everything buffered so far and a channel of later output.
// The channel is nil if the process has already exited.
func (r *Run) Subscribe() ([]byte, chan []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := append([]byte(nil), r.buf...)
	if r.done {
		return snap, nil
	}
	ch := make(chan []byte, 256)
	r.subs[ch] = struct{}{}
	return snap, ch
}

func (r *Run) Unsubscribe(ch chan []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, ch)
}

func (r *Run) alive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.done
}

func (r *Run) Input(data string) error {
	if !r.alive() {
		return errors.New("process has exited")
	}
	_, err := r.tty.WriteString(data)
	return err
}

func (r *Run) Resize(cols, rows int) error {
	if !r.alive() {
		return errors.New("process has exited")
	}
	return pty.Setsize(r.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Stop asks the whole process group to exit, then kills it if it lingers.
func (r *Run) Stop() {
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
	for ch := range r.subs {
		close(ch)
	}
	r.subs = nil
	r.mu.Unlock()
	if r.onExit != nil {
		r.onExit(r)
	}
}

// Manager owns every Run. It only ever starts the two fixed command shapes
// built in server.go; there is no way to pass it an arbitrary command.
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
			return errors.New("already running for this account and directory")
		}
	}
	if live >= maxRunning {
		return errors.New("too many running processes")
	}
	idb := make([]byte, 6)
	rand.Read(idb)
	r.ID = hex.EncodeToString(idb)
	r.Started = time.Now()
	r.subs = map[chan []byte]struct{}{}
	r.onExit = onExit
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
