package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	keys = map[string]struct{ text, named string }{
		"enter": {named: "Enter"}, "up": {named: "Up"}, "down": {named: "Down"}, "esc": {named: "Escape"},
		"y": {text: "y"}, "n": {text: "n"}, "1": {text: "1"}, "2": {text: "2"}, "3": {text: "3"},
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
	answerWait   = 700 * time.Millisecond
)

// An account is a Claude config directory plus the name shown for it. Claude
// keys each login to the directory's path, so the directory never moves once
// created; renaming an account only changes the name.
type account struct{ ID, Name string }

// accountList is every account, by name. A config directory that exists on
// disk without a row (made by hand, or by a version that kept no database) is
// adopted, so a login is never orphaned.
func (a *App) accountList() []account {
	list, err := a.data().Accounts()
	if err != nil {
		log.Printf("reading accounts: %v", err)
	}
	known := map[string]bool{}
	names := map[string]bool{}
	for _, ac := range list {
		known[ac.ID], names[ac.Name] = true, true
	}
	adopted := false
	for _, id := range a.accountDirs() {
		if known[id] {
			continue
		}
		name := id
		for n := 2; names[name]; n++ {
			name = fmt.Sprintf("%s-%d", id, n)
		}
		if err := a.data().AddAccount(id, name); err == nil {
			names[name], adopted = true, true
		}
	}
	if adopted {
		list, _ = a.data().Accounts()
	}
	// Only accounts whose directory still exists can be used.
	out := []account{}
	for _, ac := range list {
		if _, err := a.accountDir(ac.ID); err == nil {
			out = append(out, ac)
		}
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
	a.accountList() // make sure the directory has a row
	if a.nameTaken(name, id) {
		return errors.New("another account already has that name")
	}
	return a.data().RenameAccount(id, name)
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
	dir := filepath.Join(a.state, "accounts", id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := a.data().AddAccount(id, name); err != nil {
		os.Remove(dir)
		return "", err
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
	dir, err := resolveProject(a.roots, project)
	if err != nil {
		return nil, err
	}
	if spawn == "" {
		// A repository gives each session its own worktree. A folder that only
		// holds repositories cannot have worktrees, so sessions share it.
		spawn = "same-dir"
		if isGitRepo(dir) {
			spawn = "worktree"
		}
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
	run := &Run{Kind: "server", Account: accountID, Project: dir, Name: name,
		Version: a.claudeVersion(), Restarts: restarts, dir: dir, spawn: spawn, capacity: capacity}
	if err := a.mgr.Start(run, argv, childEnv(cfg), a.exitHook(run)); err != nil {
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

// move stops an instance and launches the same folder under another account,
// for when one account has run out of usage. The conversation does not carry
// over; everything on disk does.
func (a *App) move(old *Run, to string) (*Run, error) {
	if old.Kind != "server" {
		return nil, errors.New("only instances can be moved")
	}
	if to == old.Account {
		return nil, errors.New("it is already running under that account")
	}
	if _, err := a.accountDir(to); err != nil {
		return nil, err
	}
	if in, _ := a.accountStatus(to)["loggedIn"].(bool); !in {
		return nil, fmt.Errorf("%s is signed out; sign it in first", a.accountName(to))
	}
	old.Stop()
	for deadline := time.Now().Add(8 * time.Second); old.alive(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return nil, errors.New("the instance did not stop in time; try again")
		}
	}
	return a.startServer(to, old.Project, old.spawn, old.capacity, 0)
}

// startLogin runs Anthropic's own `claude auth login` for one account. The
// sign-in itself happens on Anthropic's page.
func (a *App) startLogin(accountID string) (*Run, error) {
	dir, err := a.accountDir(accountID)
	if err != nil {
		return nil, err
	}
	run := &Run{Kind: "login", Account: accountID, Name: "Sign in " + a.accountName(accountID), dir: dir}
	if err := a.mgr.Start(run, []string{a.claude, "auth", "login", "--claudeai"}, childEnv(dir), a.exitHook(run)); err != nil {
		return nil, err
	}
	a.expire(run)
	return run, nil
}

// startUpdate runs Anthropic's own `claude update`. It never runs unasked.
func (a *App) startUpdate() (*Run, error) {
	run := &Run{Kind: "update", Name: "Update Claude Code", Version: a.claudeVersion(), dir: a.state}
	if err := a.mgr.Start(run, []string{a.claude, "update"}, childEnv(""), a.exitHook(run)); err != nil {
		return nil, err
	}
	return run, nil
}

// answer replies yes or no to a question Claude is asking. Some prompts take
// a single keypress and some want Enter after it, so Enter is sent only if the
// question is still the last thing on the screen a moment later.
func answer(run *Run, yes bool) error {
	k := "n"
	if yes {
		k = "y"
	}
	if err := run.Type(k); err != nil {
		return err
	}
	time.Sleep(answerWait)
	run.read()
	if run.alive() && askRe.MatchString(lastLines(run.Tail(), 1)) {
		return run.Press("Enter")
	}
	return nil
}

// pressKey sends one of the allowed named keys to a process.
func pressKey(run *Run, key string) error {
	k, ok := keys[key]
	if !ok {
		return errors.New("that key is not allowed")
	}
	if k.named != "" {
		return run.Press(k.named)
	}
	return run.Type(k.text)
}

// submitCode hands a sign-in code to a waiting `claude auth login`.
func submitCode(run *Run, code string) error {
	code = strings.TrimSpace(code)
	if run.Kind != "login" || !codeRe.MatchString(code) {
		return errors.New("that does not look like a sign-in code")
	}
	if err := run.Type(code); err != nil {
		return err
	}
	return run.Press("Enter")
}

// exitHook is what to do when a run of this kind exits. It is chosen by kind
// so that a run adopted after a restart behaves the same as a fresh one.
func (a *App) exitHook(r *Run) func(*Run) {
	switch r.Kind {
	case "server":
		return a.serverExited
	case "login":
		account := r.Account
		return func(*Run) { a.refreshStatus(account) }
	case "update":
		return func(*Run) {
			a.verMu.Lock()
			a.verAt = time.Time{}
			a.verMu.Unlock()
		}
	}
	return nil
}

// expire cancels a sign-in that has been left waiting too long.
func (a *App) expire(run *Run) {
	left := loginTimeout - time.Since(run.Started)
	if left < 0 {
		left = 0
	}
	time.AfterFunc(left, func() {
		if run.alive() {
			run.Stop()
		}
	})
}

// adopt picks up the processes that were running when Superdeck last exited.
// The ones still being held carry on untouched; the ones that are gone are
// recorded as ended.
func (a *App) adopt() {
	runs, err := a.data().LiveRuns()
	if err != nil {
		log.Printf("reading running processes: %v", err)
		return
	}
	for _, r := range runs {
		if !a.mgr.Holding(r) {
			a.data().EndRun(r.ID, -1)
			continue
		}
		a.mgr.Adopt(r, a.exitHook(r))
		if r.Kind == "login" {
			a.expire(r)
		}
		log.Printf("still running: %s", r.Name)
	}
	a.data().Prune()
}
