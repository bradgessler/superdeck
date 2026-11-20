package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The browser cannot type into a Claude process. It can press one of these
// named keys, or, for a sign-in only, submit a code in a narrow alphabet.
var (
	keys = map[string]string{
		"enter": "\r", "y": "y", "n": "n", "1": "1", "2": "2", "3": "3",
		"up": "\x1b[A", "down": "\x1b[B", "esc": "\x1b",
	}
	codeRe = regexp.MustCompile(`^[A-Za-z0-9_.~#+/=-]{4,1000}$`)
)

// Restart tuning; tests shorten these.
var (
	minLife     = 30 * time.Second // shorter-lived exits are startup failures, not crashes
	restartBase = 5 * time.Second
	maxRestarts = 8
)

// claudeVersion is the installed CLI version, re-read at most once a minute
// so a background update of the CLI is noticed without constant polling.
func (a *App) claudeVersion() string {
	a.verMu.Lock()
	defer a.verMu.Unlock()
	if a.ver != "" && time.Since(a.verAt) < time.Minute {
		return a.ver
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.claude, "--version")
	cmd.Env = childEnv("")
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) > 0 {
			a.ver = f[0]
		}
	}
	a.verAt = time.Now()
	return a.ver
}

// startServer launches `claude remote-control` for one account in one folder.
// Every argument is built here from validated values.
func (a *App) startServer(account, project, spawn string, capacity, restarts int) (*Run, error) {
	cfg, err := a.accountDir(account)
	if err != nil {
		return nil, err
	}
	dir, err := resolveProject(a.root, project)
	if err != nil {
		return nil, err
	}
	if spawn == "" {
		spawn = "worktree"
	}
	if spawn != "worktree" && spawn != "same-dir" {
		return nil, errors.New("spawn must be worktree or same-dir")
	}
	if spawn == "worktree" && !isGitRepo(dir) {
		return nil, errors.New("this folder is not a Git repository")
	}
	if capacity == 0 {
		capacity = 4
	}
	if capacity < 1 || capacity > 32 {
		return nil, errors.New("capacity must be between 1 and 32")
	}
	name := filepath.Base(dir) + " (" + account + ")"
	if strings.HasPrefix(name, "-") {
		name = "project " + name // never let a folder name read as a flag
	}
	argv := []string{a.claude, "remote-control", "--name", name,
		"--spawn", spawn, "--capacity", strconv.Itoa(capacity)}
	run := &Run{Kind: "server", Account: account, Project: project, Name: name,
		Version: a.claudeVersion(), Restarts: restarts, dir: dir, spawn: spawn, capacity: capacity}
	if err := a.mgr.Start(run, argv, childEnv(cfg), a.serverExited); err != nil {
		return nil, err
	}
	return run, nil
}

// serverExited restarts an instance that died on its own, for example after
// Claude gives up on a long network outage. It leaves alone anything a person
// stopped, anything that failed at startup, and anything waiting on a sign-in
// or trust decision, since restarting those would only loop.
func (a *App) serverExited(r *Run) {
	if a.closing.Load() || r.wasStopped() || r.View().Hint != "" {
		return
	}
	life := time.Since(r.Started)
	if life < minLife {
		return
	}
	n := r.Restarts + 1
	if life > 10*time.Minute {
		n = 1
	}
	if n > maxRestarts {
		log.Printf("%s: giving up after %d restarts in a row", r.Name, maxRestarts)
		return
	}
	delay := restartBase << (n - 1)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	log.Printf("%s exited on its own after %s; restarting in %s (attempt %d)", r.Name, life.Round(time.Second), delay, n)
	time.AfterFunc(delay, func() {
		if a.closing.Load() {
			return
		}
		if _, err := a.startServer(r.Account, r.Project, r.spawn, r.capacity, n); err != nil {
			log.Printf("%s: restart failed: %v", r.Name, err)
		}
	})
}

func (a *App) handleStartServer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account, Project, Spawn string
		Capacity                int
	}
	if !readJSON(w, r, &in) {
		return
	}
	run, err := a.startServer(in.Account, in.Project, in.Spawn, in.Capacity, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	writeJSON(w, run.View())
}

// handleRelaunch stops an instance and starts it again with the same account
// and folder, which is how it picks up a newer Claude Code.
func (a *App) handleRelaunch(w http.ResponseWriter, r *http.Request) {
	old := a.run(w, r)
	if old == nil {
		return
	}
	if old.Kind != "server" {
		http.Error(w, "only instances can be relaunched", http.StatusBadRequest)
		return
	}
	old.Stop()
	for deadline := time.Now().Add(8 * time.Second); old.alive(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			http.Error(w, "the instance did not stop in time; try again", http.StatusConflict)
			return
		}
	}
	run, err := a.startServer(old.Account, old.Project, old.spawn, old.capacity, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, run.View())
}

// handleUpdate runs Anthropic's own `claude update`. It never runs unasked.
func (a *App) handleUpdate(w http.ResponseWriter, r *http.Request) {
	run := &Run{Kind: "update", Name: "Update Claude Code", Version: a.claudeVersion(), dir: a.state}
	err := a.mgr.Start(run, []string{a.claude, "update"}, childEnv(""), func(*Run) {
		a.verMu.Lock()
		a.verAt = time.Time{}
		a.verMu.Unlock()
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, run.View())
}

func (a *App) handleInput(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	var in struct{ Key, Code string }
	if !readJSON(w, r, &in) {
		return
	}
	var data string
	switch {
	case in.Key != "":
		k, ok := keys[in.Key]
		if !ok {
			http.Error(w, "that key is not allowed", http.StatusBadRequest)
			return
		}
		data = k
	case in.Code != "":
		code := strings.TrimSpace(in.Code)
		if run.Kind != "login" || !codeRe.MatchString(code) {
			http.Error(w, "that does not look like a sign-in code", http.StatusBadRequest)
			return
		}
		data = code + "\r"
	default:
		http.Error(w, "send a key or a code", http.StatusBadRequest)
		return
	}
	if err := run.Input(data); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
