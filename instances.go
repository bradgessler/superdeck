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
	"sync"
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
	minLife     = 30 * time.Second // an instance that has lived this long has proved it works
	restartBase = 5 * time.Second
	restartCap  = 5 * time.Minute

	loginTimeout = 15 * time.Minute
	bootRetry    = 30 * time.Second
	answerWait   = 700 * time.Millisecond
	maxAccounts  = 12
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
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	if !accountNameRe.MatchString(name) {
		return "", errors.New("account names use lowercase letters, digits and dashes")
	}
	if name == "new" { // /accounts/new is the screen for adding one
		return "", errors.New("that name is one Superdeck uses itself; pick another")
	}
	if a.nameTaken(name, "") {
		return "", errors.New("an account already has that name")
	}
	if len(a.accountList()) >= maxAccounts {
		return "", errors.New("that is as many accounts as Superdeck keeps; remove one first")
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
	ver, fresh, known := a.ver, time.Since(a.verAt) < time.Minute, !a.verAt.IsZero()
	if !fresh {
		a.verAt = time.Now() // one check at a time, successful or not
	}
	a.verMu.Unlock()
	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.claude, "--version")
		cmd.Env = childEnv("")
		v := ""
		if out, err := cmd.Output(); err == nil {
			if f := strings.Fields(string(out)); len(f) > 0 {
				v = f[0]
			}
		}
		a.verMu.Lock()
		if v != "" {
			a.ver = v
		}
		a.verMu.Unlock()
	}
	switch {
	case fresh:
	case known:
		go check() // show what is known; refresh behind the scenes
	default:
		check() // the very first time, wait for it
		a.verMu.Lock()
		ver = a.ver
		a.verMu.Unlock()
	}
	return ver
}

// backoff is how long to wait before restart attempt n: doubling from
// restartBase, never more than restartCap.
func backoff(n int) time.Duration {
	d := restartBase
	for i := 1; i < n && d < restartCap; i++ {
		d *= 2
	}
	if d > restartCap {
		d = restartCap
	}
	return d
}

// folderLock serialises everything that starts or stops instances in one
// folder, so that a double tap, a stale tab and a restart coming due cannot
// between them leave two running.
func (a *App) folderLock(folder string) func() {
	a.opMu.Lock()
	m := a.ops[folder]
	if m == nil {
		m = &sync.Mutex{}
		a.ops[folder] = m
	}
	a.opMu.Unlock()
	m.Lock()
	return m.Unlock
}

// current follows a run to whatever replaced it. Pages and tabs can be
// holding the address of a run that has since been restarted; what the
// person means is the instance, not that one process.
func (a *App) current(r *Run) *Run {
	for i := 0; i < 50; i++ {
		next := a.mgr.Get(r.View().Next)
		if next == nil {
			break
		}
		r = next
	}
	return a.liveTwin(r)
}

// liveTwin covers a run that ended and was not replaced through Superdeck's
// own chain: someone stopped it and later launched the same account in the
// same folder afresh. A stale page for the old run then means the new one;
// acting on the old one's record would forget the instance that is running.
func (a *App) liveTwin(r *Run) *Run {
	if r.Kind != "server" || r.alive() {
		return r
	}
	for _, v := range a.mgr.List() {
		if v.Kind == "server" && !v.Done && v.Account == r.Account && v.Project == r.Project {
			if live := a.mgr.Get(v.ID); live != nil {
				return live
			}
		}
	}
	return r
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
	if !a.offered(dir) {
		return nil, errors.New("that folder is not one Superdeck can launch in")
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
	a.proveLater(run)
	return run, nil
}

// proveLater marks an instance as proven once it has lived a while. From
// then on it is retried however often it fails.
func (a *App) proveLater(run *Run) {
	left := minLife - time.Since(run.Started)
	if left < 0 {
		left = 0
	}
	time.AfterFunc(left, func() {
		if run.alive() {
			a.data().SetProven(run.Account, run.Project)
		}
	})
}

// serverExited decides what happens when an instance exits.
//
// An instance a person launched stays theirs until they stop it. If it dies
// it is restarted, with a delay that doubles up to a few minutes, for as long
// as it takes: after a long network outage or a reboot the first attempts are
// expected to fail, and giving up would silently end supervision.
//
// The exceptions are things only a person can fix. A signed-out account is
// not retried (it would only loop) but is kept, and comes back when the
// account is signed in. A folder they declined to trust is forgotten. And an
// instance that has never once run properly is dropped after its first
// failure rather than retried for ever.
func (a *App) serverExited(r *Run) {
	if a.closing.Load() || r.wasStopped() || !a.isWanted(r.Account, r.Project) {
		return
	}
	v := r.View()
	w := wanted{Account: r.Account, Project: r.Project}
	switch v.Hint {
	case "needs-login":
		// Right after Superdeck starts, the Keychain may not be readable
		// yet. Try a few times before believing it.
		if time.Since(a.started) < 10*time.Minute && r.Restarts < 3 {
			log.Printf("%s reported signed out soon after startup; retrying in %s (attempt %d)", r.Name, bootRetry, r.Restarts+1)
			a.scheduleRestart(r, r.Restarts+1, bootRetry)
			return
		}
		go a.refreshStatus(r.Account) // the account screen should say so too
		return
	case "needs-trust":
		a.remember(w, false)
		return
	}
	life := time.Since(r.Started)
	if life >= minLife {
		a.data().SetProven(r.Account, r.Project)
	} else if !a.data().Proven(r.Account, r.Project) {
		log.Printf("%s failed on its first start; not retrying", r.Name)
		a.remember(w, false)
		return
	}
	n := r.Restarts + 1
	if life > 10*time.Minute {
		n = 1
	}
	log.Printf("%s exited on its own after %s; restarting in %s (attempt %d)", r.Name, life.Round(time.Second), backoff(n), n)
	a.scheduleRestart(r, n, backoff(n))
}

func (a *App) scheduleRestart(r *Run, n int, delay time.Duration) {
	r.setRestart(time.Now().Add(delay))
	time.AfterFunc(delay, func() { a.restart(r, n) })
}

// restart is a scheduled restart coming due. Things may have changed while
// it waited: the instance may have been stopped, moved or relaunched by a
// person, in which case it must not come back.
func (a *App) restart(r *Run, n int) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("%s: restart panicked: %v; trying again in %s", r.Name, p, backoff(n+1))
			a.scheduleRestart(r, n+1, backoff(n+1))
		}
	}()
	unlock := a.folderLock(r.Project)
	defer unlock()
	if a.closing.Load() || r.wasStopped() || !a.isWanted(r.Account, r.Project) || a.running(r.Account, r.Project) {
		r.setRestart(time.Time{})
		return
	}
	next, err := a.startServer(r.Account, r.Project, r.spawn, r.capacity, n)
	if err != nil {
		// Could not even start it (the limit was reached, a disk is not
		// mounted). Keep trying, more slowly, rather than dropping it.
		log.Printf("%s: restart failed (%v); trying again in %s", r.Name, err, backoff(n+1))
		a.scheduleRestart(r, n+1, backoff(n+1))
		return
	}
	r.setNext(next.ID)
}

// bringBack starts a remembered instance that is not running, and keeps
// trying if it cannot be started yet: a folder on a disk that is not mounted,
// say. Used when Superdeck starts and when an account is signed in.
func (a *App) bringBack(w wanted, n int) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("bringing back %s panicked: %v", w.Project, p)
		}
	}()
	if a.closing.Load() || !a.isWanted(w.Account, w.Project) {
		return
	}
	unlock := a.folderLock(w.Project)
	if a.running(w.Account, w.Project) {
		unlock()
		return
	}
	_, err := a.startServer(w.Account, w.Project, w.Spawn, w.Capacity, 0)
	unlock()
	if err != nil {
		log.Printf("could not bring back %s as %s (%v); trying again in %s", w.Project, a.accountName(w.Account), err, backoff(n))
		time.AfterFunc(backoff(n), func() { a.bringBack(w, n+1) })
	}
}

// isWanted reports whether a person launched this instance and has not
// stopped it since.
func (a *App) isWanted(accountID, folder string) bool {
	list, err := a.data().Wanted()
	if err != nil {
		// Not being able to read the list is no reason to stop looking
		// after an instance.
		return true
	}
	for _, w := range list {
		if w.Account == accountID && w.Project == folder {
			return true
		}
	}
	return false
}

// canStart checks, without changing anything, that an instance for this
// account and folder could be launched now.
func (a *App) canStart(accountID, folder string, replacing *Run) error {
	if _, err := a.accountDir(accountID); err != nil {
		return err
	}
	if in, _ := a.accountStatusNow(accountID)["loggedIn"].(bool); !in {
		return fmt.Errorf("sign in to %s first", a.accountName(accountID))
	}
	dir, err := resolveProject(a.roots, folder)
	if err != nil {
		return err
	}
	if !a.offered(dir) {
		return errors.New("that folder is not one Superdeck can launch in")
	}
	servers := 0
	for _, v := range a.mgr.List() {
		if v.Kind != "server" || v.Done || (replacing != nil && v.ID == replacing.ID) {
			continue
		}
		servers++
		if v.Account == accountID && v.Run.dir == dir {
			return fmt.Errorf("%s is already running this folder", a.accountName(accountID))
		}
	}
	if servers >= maxRunning {
		return errors.New("too many instances are running; stop one first")
	}
	return nil
}

// stopAndWait stops a run and waits for it to be gone. If it will not go in
// time the stop is taken back, so the run is still looked after.
func (a *App) stopAndWait(r *Run) error {
	r.Stop()
	for deadline := time.Now().Add(8 * time.Second); r.alive(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.unstop()
			a.data().ClearStopped(r.ID)
			return errors.New("the instance did not stop in time, so nothing was changed; try again")
		}
	}
	return nil
}

// relaunch stops an instance and starts it again with the same account and
// folder, which is how it picks up a newer Claude Code. It also brings back
// one that has stopped.
func (a *App) relaunch(old *Run) (*Run, error) {
	if old.Kind != "server" {
		return nil, errors.New("only instances can be relaunched")
	}
	unlock := a.folderLock(old.Project)
	defer unlock()
	old = a.current(old)
	// Check it can come back before taking it down.
	if err := a.canStart(old.Account, old.Project, old); err != nil {
		return nil, err
	}
	if err := a.stopAndWait(old); err != nil {
		return nil, err
	}
	a.remember(wanted{old.Account, old.Project, old.spawn, old.capacity}, true)
	run, err := a.startServer(old.Account, old.Project, old.spawn, old.capacity, 0)
	if err != nil {
		// It is down and its replacement did not start. Do not leave it at
		// that: have it retried like any instance that died.
		old.unstop()
		a.scheduleRestart(old, 1, restartBase)
		return nil, fmt.Errorf("it could not be started again just now (%w); Superdeck will keep trying", err)
	}
	old.setNext(run.ID)
	return run, nil
}

// move stops an instance and launches the same folder under another account,
// for when one account has run out of usage. The conversation does not carry
// over; everything on disk does.
func (a *App) move(old *Run, to string) (*Run, error) {
	if old.Kind != "server" {
		return nil, errors.New("only instances can be moved")
	}
	unlock := a.folderLock(old.Project)
	defer unlock()
	// Whatever tab this came from, act on the instance as it is now.
	old = a.current(old)
	if to == old.Account {
		return nil, errors.New("it is already under that account")
	}
	// Everything that could stop the new one from starting is checked first,
	// so a failed move leaves the old one running.
	if err := a.canStart(to, old.Project, old); err != nil {
		return nil, err
	}
	if err := a.stopAndWait(old); err != nil {
		return nil, err
	}
	// It keeps its standing: an instance that had proved itself is retried
	// under the new account too, not dropped if its first start there fails.
	proven := a.data().Proven(old.Account, old.Project)
	a.remember(wanted{Account: old.Account, Project: old.Project}, false)
	run, err := a.startServer(to, old.Project, old.spawn, old.capacity, 0)
	if err != nil {
		// It checked out a moment ago and still failed. Put the old one back
		// rather than leave nothing running.
		a.remember(wanted{old.Account, old.Project, old.spawn, old.capacity}, true)
		back, berr := a.startServer(old.Account, old.Project, old.spawn, old.capacity, 0)
		if berr != nil {
			old.unstop()
			a.scheduleRestart(old, 1, restartBase)
			return nil, fmt.Errorf("could not start it under %s (%w); Superdeck is bringing it back under %s", a.accountName(to), err, a.accountName(old.Account))
		}
		old.setNext(back.ID)
		return nil, fmt.Errorf("could not start it under %s (%w), so it is running under %s again", a.accountName(to), err, a.accountName(old.Account))
	}
	a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	if proven {
		a.data().SetProven(run.Account, run.Project)
	}
	old.setNext(run.ID)
	return run, nil
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
	// A second tap while the first is still being typed is dropped, so a
	// double tap cannot send two letters.
	if !run.answering.TryLock() {
		return nil
	}
	defer run.answering.Unlock()
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
		return func(done *Run) {
			a.refreshStatus(account)
			// Signing in is what its waiting instances were waiting for.
			if in, _ := a.accountStatusNow(account)["loggedIn"].(bool); in {
				for _, w := range a.loadWanted() {
					if w.Account == account {
						go a.bringBack(w, 1)
					}
				}
			}
		}
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
// recorded as ended. If it cannot read the record it says so, and nothing
// may be launched: every held instance would get a duplicate.
func (a *App) adopt() error {
	runs, err := a.data().LiveRuns()
	if err != nil {
		return fmt.Errorf("reading running processes: %w", err)
	}
	known := map[string]bool{}
	for _, r := range runs {
		known[r.ID] = true
		a.adoptOne(r)
	}
	// A holder with no record cannot be supervised or shown. Ask it to stop
	// rather than let it run unseen beside whatever is launched in its place.
	// Its socket is only removed once nothing is holding it.
	for _, id := range a.mgr.Leftovers() {
		if known[id] {
			continue
		}
		if holdSay(filepath.Join(a.mgr.dir, id+".sock"), "stop") == nil {
			log.Printf("stopped a process Superdeck had no record of (%s)", id)
		}
		a.mgr.Forget(id)
	}
	a.data().Prune()
	return nil
}

// adoptOne picks up one recorded process. Whatever is wrong with one record
// must not keep Superdeck from starting: a Superdeck that cannot start cannot
// be reached to put anything right. A record that cannot be dealt with is
// left as it is, with its process untouched.
func (a *App) adoptOne(r *Run) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("could not pick up %s (%s): %v; leaving it alone", r.Name, r.ID, p)
		}
	}()
	{
		switch state, code := a.mgr.Probe(r); state {
		case "held":
			a.mgr.Adopt(r, a.exitHook(r))
			switch r.Kind {
			case "login":
				a.expire(r)
			case "server":
				a.proveLater(r)
				// Superdeck went down in the middle of relaunching or
				// moving it: marked to stop, never stopped, still wanted.
				if r.wasStopped() && a.isWanted(r.Account, r.Project) {
					r.unstop()
					a.data().ClearStopped(r.ID)
				}
			}
			log.Printf("still running: %s", r.Name)
		default: // it ended, or vanished, while Superdeck was away
			a.data().EndRun(r.ID, code)
			a.mgr.Forget(r.ID)
		}
	}
}

// removeAccount signs an account out with Anthropic's own command, then
// deletes its config directory and forgets it. It refuses while anything is
// running under the account.
func (a *App) removeAccount(id string) error {
	dir, err := a.accountDir(id)
	if err != nil {
		return err
	}
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	for _, v := range a.mgr.List() {
		if v.Account == id && (!v.Done || !v.RestartAt.IsZero()) {
			return errors.New("stop what is running under this account first")
		}
	}
	// Signing out removes the login from the Keychain; deleting the directory
	// alone would leave it there with nothing pointing at it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.claude, "auth", "logout")
	cmd.Env = childEnv(dir)
	cmd.Dir = dir
	cmd.Run() // best effort: an account that was never signed in has nothing to sign out of
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	a.statusMu.Lock()
	delete(a.status, id)
	delete(a.statusAt, id)
	a.statusMu.Unlock()
	return a.data().RemoveAccount(id)
}
