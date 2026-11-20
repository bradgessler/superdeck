package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
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

	loginTimeout = 15 * time.Minute
	bootRetry    = 30 * time.Second
)

// An account is a Claude config directory plus the name shown for it. Claude
// keys each login to the directory's path, so the directory never moves once
// created; renaming an account only changes the name.
type account struct{ ID, Name string }

func (a *App) namesPath() string { return filepath.Join(a.state, "accounts.json") }

func (a *App) names() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(a.namesPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (a *App) accountList() []account {
	names := a.names()
	out := []account{}
	for _, id := range a.accounts() {
		n := names[id]
		if !accountNameRe.MatchString(n) {
			n = id
		}
		out = append(out, account{id, n})
	}
	return out
}

func (a *App) accountName(id string) string {
	for _, ac := range a.accountList() {
		if ac.ID == id {
			return ac.Name
		}
	}
	return id
}

func (a *App) nameTaken(name, exceptID string) bool {
	for _, ac := range a.accountList() {
		if ac.Name == name && ac.ID != exceptID {
			return true
		}
	}
	return false
}

func (a *App) setName(id, name string) error {
	if !accountNameRe.MatchString(name) {
		return errors.New("account names use lowercase letters, digits and dashes")
	}
	if _, err := a.accountDir(id); err != nil {
		return err
	}
	if a.nameTaken(name, id) {
		return errors.New("another account already has that name")
	}
	a.wantMu.Lock()
	defer a.wantMu.Unlock()
	m := a.names()
	m[id] = name
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := a.namesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.namesPath())
}

// addAccount creates a config directory for a new account and returns its id.
func (a *App) addAccount(name string) (string, error) {
	if !accountNameRe.MatchString(name) {
		return "", errors.New("account names use lowercase letters, digits and dashes")
	}
	if a.nameTaken(name, "") {
		return "", errors.New("an account already has that name")
	}
	id := name
	if _, err := os.Stat(filepath.Join(a.state, "accounts", id)); err == nil {
		b := make([]byte, 3)
		rand.Read(b)
		id = name + "-" + hex.EncodeToString(b)
	}
	if err := os.Mkdir(filepath.Join(a.state, "accounts", id), 0o700); err != nil {
		return "", err
	}
	if id != name {
		if err := a.setName(id, name); err != nil {
			return "", err
		}
	}
	return id, nil
}

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
func (a *App) startServer(accountID, project, spawn string, capacity, restarts int) (*Run, error) {
	cfg, err := a.accountDir(accountID)
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
		return nil, errors.New("that folder is not a Git repository")
	}
	if capacity == 0 {
		capacity = 4
	}
	if capacity < 1 || capacity > 32 {
		return nil, errors.New("capacity must be between 1 and 32")
	}
	name := filepath.Base(dir) + " (" + a.accountName(accountID) + ")"
	if strings.HasPrefix(name, "-") {
		name = "project " + name // never let a folder name read as a flag
	}
	argv := []string{a.claude, "remote-control", "--name", name,
		"--spawn", spawn, "--capacity", strconv.Itoa(capacity)}
	run := &Run{Kind: "server", Account: accountID, Project: project, Name: name,
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
	if a.closing.Load() || r.wasStopped() {
		return
	}
	hint := r.View().Hint
	// Right after the Mac starts, the Keychain may not be readable yet and
	// Claude reports the account as signed out. Try again a few times before
	// believing it.
	if hint == "needs-login" && time.Since(a.started) < 10*time.Minute && r.Restarts < 3 {
		n := r.Restarts + 1
		log.Printf("%s reported signed out soon after startup; retrying in 30s (attempt %d)", r.Name, n)
		time.AfterFunc(bootRetry, func() { a.restart(r, n) })
		return
	}
	if hint != "" {
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
	time.AfterFunc(delay, func() { a.restart(r, n) })
}

func (a *App) restart(r *Run, n int) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("%s: restart panicked: %v", r.Name, p)
		}
	}()
	if a.closing.Load() {
		return
	}
	if _, err := a.startServer(r.Account, r.Project, r.spawn, r.capacity, n); err != nil {
		log.Printf("%s: restart failed: %v", r.Name, err)
	}
}

// relaunch stops an instance and starts it again with the same account and
// folder, which is how it picks up a newer Claude Code.
func (a *App) relaunch(old *Run) (*Run, error) {
	if old.Kind != "server" {
		return nil, errors.New("only instances can be relaunched")
	}
	old.Stop()
	for deadline := time.Now().Add(8 * time.Second); old.alive(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return nil, errors.New("the instance did not stop in time; try again")
		}
	}
	return a.startServer(old.Account, old.Project, old.spawn, old.capacity, 0)
}

// startLogin runs Anthropic's own `claude auth login` for one account. The
// sign-in itself happens on Anthropic's page.
func (a *App) startLogin(accountID string) (*Run, error) {
	dir, err := a.accountDir(accountID)
	if err != nil {
		return nil, err
	}
	run := &Run{Kind: "login", Account: accountID, Name: "Sign in " + a.accountName(accountID), dir: dir}
	err = a.mgr.Start(run, []string{a.claude, "auth", "login", "--claudeai"}, childEnv(dir),
		func(*Run) { a.refreshStatus(accountID) })
	if err != nil {
		return nil, err
	}
	// An abandoned sign-in should not sit there forever.
	time.AfterFunc(loginTimeout, func() {
		if run.alive() {
			run.Stop()
		}
	})
	return run, nil
}

// startUpdate runs Anthropic's own `claude update`. It never runs unasked.
func (a *App) startUpdate() (*Run, error) {
	run := &Run{Kind: "update", Name: "Update Claude Code", Version: a.claudeVersion(), dir: a.state}
	err := a.mgr.Start(run, []string{a.claude, "update"}, childEnv(""), func(*Run) {
		a.verMu.Lock()
		a.verAt = time.Time{}
		a.verMu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// answer replies yes or no to a question Claude is asking. Some prompts take
// a single keypress and some want Enter after it, so Enter is sent only if
// nothing but the echoed letter has appeared since.
func answer(run *Run, yes bool) error {
	k := "n"
	if yes {
		k = "y"
	}
	if err := run.Input(k); err != nil {
		return err
	}
	time.Sleep(700 * time.Millisecond)
	if t := strings.TrimSpace(run.Tail()); run.alive() && (t == "" || strings.EqualFold(t, k)) {
		return run.Input("\r")
	}
	return nil
}

// pressKey sends one of the allowed named keys to a process.
func pressKey(run *Run, key string) error {
	k, ok := keys[key]
	if !ok {
		return errors.New("that key is not allowed")
	}
	return run.Input(k)
}

// submitCode hands a sign-in code to a waiting `claude auth login`.
func submitCode(run *Run, code string) error {
	code = strings.TrimSpace(code)
	if run.Kind != "login" || !codeRe.MatchString(code) {
		return errors.New("that does not look like a sign-in code")
	}
	return run.Input(code + "\r")
}
