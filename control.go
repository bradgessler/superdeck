package main

// The way a Claude session reaches Superdeck. Every instance is started with
// the path of this binary in SUPERDECK and a skill that explains two things a
// session can do with it: move itself to another account, and read the notes
// the session before it left.
//
// The session runs `superdeck move NAME`, which talks to the running
// Superdeck over a Unix socket in its private run directory. Nothing is
// registered in Claude's settings, there is no extra server process, and the
// socket is closed to other users of the Mac by the directory it is in. A
// process that can reach it is already running as the owner.
//
// What can be asked for is what the page offers, less the things that destroy:
// see what is running, launch, make a folder, stop, relaunch, move, answer a
// question Claude asks as it starts, add an account and sign it in. Removing
// an account stays on the page. Every request goes through the same checks as
// the page's buttons: names are matched against the accounts and folders that
// exist, and nothing a session sends is passed to a command line unchecked.

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed skill/SKILL.md
var skillFS embed.FS

const maxNotes = 64 << 10

// moveDelay is how long after agreeing to a move Superdeck waits before
// stopping the instance, so the session that asked can hear the answer.
var moveDelay = 5 * time.Second

func (a *App) controlSock() string { return filepath.Join(a.mgr.dir, "control.sock") }

// sessionEnv is what an instance is given so that sessions inside it can
// reach Superdeck. It names things; it carries no secret.
func (a *App) sessionEnv(accountID, dir string) []string {
	return []string{
		"SUPERDECK=" + a.mgr.self,
		"SUPERDECK_SOCK=" + a.controlSock(),
		"SUPERDECK_ACCOUNT=" + accountID,
		"SUPERDECK_FOLDER=" + dir,
	}
}

// installSkill puts Superdeck's skill into an account's config directory, in
// a folder of its own. It is the one thing Superdeck writes there. Failing to
// is not a reason to refuse a launch.
func (a *App) installSkill(configDir string) {
	want, err := skillFS.ReadFile("skill/SKILL.md")
	if err != nil {
		return
	}
	dir := filepath.Join(configDir, "skills", "superdeck")
	path := filepath.Join(dir, "SKILL.md")
	if have, err := os.ReadFile(path); err == nil && string(have) == string(want) {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("installing the Superdeck skill: %v", err)
		return
	}
	writeFileAtomic(path, want)
}

// serveControl starts answering sessions. If it cannot, Superdeck carries on
// without it: launching and supervising do not depend on this.
func (a *App) serveControl() {
	if err := ensureRunDir(a.mgr.dir); err != nil {
		log.Printf("sessions cannot reach Superdeck: %v", err)
		return
	}
	path := a.controlSock()
	os.Remove(path) // only one Superdeck runs on this data, so a socket here is stale
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Printf("sessions cannot reach Superdeck: %v", err)
		return
	}
	os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	for route, h := range map[string]func(ctl) (int, string){
		"GET /status":    a.ctlStatus,
		"GET /accounts":  a.ctlAccounts,
		"GET /folders":   a.ctlFolders,
		"GET /handoff":   a.ctlHandoff,
		"POST /launch":   a.ctlLaunch,
		"POST /folder":   a.ctlNewFolder,
		"POST /stop":     a.ctlStop,
		"POST /relaunch": a.ctlRelaunch,
		"POST /move":     a.ctlMove,
		"POST /answer":   a.ctlAnswer,
		"POST /account":  a.ctlAddAccount,
		"POST /signin":   a.ctlSignIn,
		"POST /code":     a.ctlCode,
	} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxNotes+8<<10)
			if r.Method == http.MethodPost {
				// With no sign-in, the log is the record of what was asked.
				log.Printf("command %q for account %q in %q", strings.TrimPrefix(r.URL.Path, "/"), r.FormValue("account"), r.FormValue("folder"))
			}
			status, text := h(ctl{a: a, get: r.FormValue})
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			fmt.Fprintln(w, strings.TrimRight(text, "\n"))
		})
	}
	srv := &http.Server{Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("the socket sessions use stopped: %v", err)
		}
	}()
}

// ctl is one request from the command. Every answer is a status and plain
// text written for Claude, or a person at a terminal, to read.
type ctl struct {
	a   *App
	get func(string) string
}

func no(format string, args ...any) (int, string) {
	return 400, fmt.Sprintf(format, args...) + " Nothing has changed."
}

// callerInstance finds the running instance a session says it is in.
func (a *App) callerInstance(account, folder string) *Run {
	if account == "" || folder == "" {
		return nil
	}
	for _, v := range a.mgr.List() {
		if v.Kind == "server" && !v.Done && v.Account == account && v.Project == folder {
			return a.mgr.Get(v.ID)
		}
	}
	return nil
}

// which finds the instance a command is about: the one named, or with no
// name the one the caller is running in. A name is an id (or the start of
// one) or a folder's name, among instances that are running or due back.
func (c ctl) which() (*Run, string) {
	ref := strings.TrimSpace(c.get("instance"))
	if ref == "" {
		if run := c.a.callerInstance(c.get("account"), c.get("folder")); run != nil {
			return run, ""
		}
		return nil, "Say which instance: its name or id, as `superdeck status` lists them."
	}
	var found []*Run
	for _, v := range c.a.mgr.List() {
		if v.Kind != "server" || (v.Done && v.RestartAt.IsZero()) {
			continue
		}
		if strings.HasPrefix(v.ID, ref) || strings.EqualFold(filepath.Base(v.Project), ref) || v.Project == ref {
			found = append(found, c.a.mgr.Get(v.ID))
		}
	}
	switch len(found) {
	case 1:
		return found[0], ""
	case 0:
		return nil, fmt.Sprintf("No instance called %q is running. `superdeck status` lists them.", ref)
	}
	return nil, fmt.Sprintf("More than one instance matches %q. Use the id from `superdeck status`.", ref)
}

// own reports whether run is the instance the caller is running in. Stopping
// that one ends the session that asked, so it is given time to hear back.
func (c ctl) own(run *Run) bool {
	return run.Account == c.get("account") && run.Project == c.get("folder")
}

// accountID looks a name up among the accounts that exist: by the name people
// see first, then by the directory name.
func (a *App) accountID(want string) string {
	want = strings.TrimSpace(want)
	list := a.accountList()
	for _, ac := range list {
		if strings.EqualFold(ac.Name, want) {
			return ac.ID
		}
	}
	for _, ac := range list {
		if ac.ID == want {
			return ac.ID
		}
	}
	return ""
}

func (c ctl) ctlAccountID(field string) (string, string) {
	want := strings.TrimSpace(c.get(field))
	if id := c.a.accountID(want); id != "" {
		return id, ""
	}
	return "", fmt.Sprintf("There is no account called %q. `superdeck accounts` lists them.", want)
}

func (a *App) ctlStatus(c ctl) (int, string) {
	var b strings.Builder
	for _, v := range a.mgr.List() {
		iv := a.instance(v)
		switch {
		case v.Kind == "server" && (!v.Done || !v.RestartAt.IsZero()):
			mark := ""
			if c.own(v.Run) {
				mark = "  <- this session"
			}
			fmt.Fprintf(&b, "%s: %s, under %s, in %s (id %s)%s\n", iv.Title, iv.State, iv.Account, iv.Path, v.ID, mark)
			switch iv.Do {
			case "trust":
				fmt.Fprintf(&b, "    Claude is asking whether to trust this folder. Answer with: superdeck answer %s yes   (or no)\n", v.ID)
			case "consent":
				fmt.Fprintf(&b, "    Claude is asking whether to turn on Remote Control for this account. Answer with: superdeck answer %s yes   (or no)\n", v.ID)
			case "question":
				fmt.Fprintf(&b, "    Claude is asking: %s\n    Answer with: superdeck answer %s yes   (or no)\n", strings.TrimSpace(iv.Question), v.ID)
			case "open":
				if noCommits(v.Project) {
					fmt.Fprintf(&b, "    Sessions cannot start here yet: this repository has no commits. Make a first commit in it.\n")
				}
				fmt.Fprintf(&b, "    Open it in the Claude app: %s\n", iv.Link)
			}
		case v.Kind == "login" && !v.Done:
			fmt.Fprintf(&b, "Sign-in waiting for %s.", iv.Account)
			if v.URL != "" {
				fmt.Fprintf(&b, " Open %s and then run: superdeck code %s CODE", v.URL, iv.Account)
			}
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return 200, "Nothing is running."
	}
	return 200, b.String()
}

func (a *App) ctlAccounts(c ctl) (int, string) {
	var b strings.Builder
	for _, ac := range a.accountList() {
		state := "signed out"
		st := a.accountStatusNow(ac.ID)
		if in, _ := st["loggedIn"].(bool); in {
			state = "signed in"
			if email, _ := st["email"].(string); email != "" {
				state += " as " + email
			}
		}
		mark := ""
		if ac.ID == c.get("account") {
			mark = "  <- this session"
		}
		fmt.Fprintf(&b, "%s  (%s)%s\n", ac.Name, state, mark)
	}
	if b.Len() == 0 {
		return 200, "There are no accounts yet. Add one with: superdeck account add NAME"
	}
	return 200, b.String()
}

func (a *App) ctlFolders(c ctl) (int, string) {
	q := strings.ToLower(strings.TrimSpace(c.get("q")))
	var b strings.Builder
	n := 0
	for _, f := range a.folderList() {
		if q != "" && !strings.Contains(strings.ToLower(f.Path), q) {
			continue
		}
		if n++; n > 200 {
			b.WriteString("... and more. Narrow it down: superdeck folders WORD\n")
			break
		}
		b.WriteString(tilde(f.Path) + "\n")
	}
	if n == 0 {
		return 200, "No folders match. New ones can be made with: superdeck folder NAME"
	}
	return 200, b.String()
}

// folderNamed finds a folder Superdeck offers from what a person would say:
// its path, with or without ~, or just its name if only one has it.
func (a *App) folderNamed(want string) (string, string) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", "Say which folder. `superdeck folders` lists them."
	}
	var byName []string
	for _, f := range a.folderList() {
		if f.Path == want || tilde(f.Path) == want {
			return f.Path, ""
		}
		if strings.EqualFold(filepath.Base(f.Path), want) {
			byName = append(byName, f.Path)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], ""
	case 0:
		if filepath.IsAbs(want) {
			return want, "" // launch checks it properly and says why not
		}
		return "", fmt.Sprintf("No folder called %q is offered. `superdeck folders` lists them.", want)
	}
	for i, p := range byName {
		byName[i] = tilde(p)
	}
	return "", fmt.Sprintf("More than one folder is called %q: %s. Give the path.", want, strings.Join(byName, ", "))
}

// launch starts an instance the way the page's Launch button does.
func (a *App) launch(account, folder string) (*Run, error) {
	if _, err := a.accountDir(account); err != nil {
		return nil, err
	}
	if in, _ := a.accountStatusNow(account)["loggedIn"].(bool); !in {
		return nil, fmt.Errorf("sign in to %s first", a.accountName(account))
	}
	dir, err := resolveProject(a.roots, folder)
	if err != nil {
		return nil, err
	}
	if !a.allow() {
		return nil, errors.New("too many launches in a short time; wait a minute")
	}
	unlock := a.folderLock(dir)
	defer unlock()
	run, err := a.startServer(account, dir, "", 0, 0)
	if err != nil {
		return nil, err
	}
	a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	return run, nil
}

// settle waits a little for a new instance to connect or ask something, then
// says where it stands.
func (a *App) settle(run *Run) string {
	for deadline := time.Now().Add(settleFor); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if v := run.View(); v.Done || v.Link != "" || v.Hint != "" {
			break
		}
	}
	v := run.View()
	iv := a.instance(v)
	switch {
	case iv.Do == "trust":
		return fmt.Sprintf("Claude is asking whether to trust the folder %s. Ask the user, then answer with: superdeck answer %s yes   (or no)", iv.Path, v.ID)
	case iv.Do == "consent":
		return fmt.Sprintf("Claude is asking whether to turn on Remote Control for this account. Ask the user, then answer with: superdeck answer %s yes   (or no)", v.ID)
	case iv.Do == "question":
		return fmt.Sprintf("Claude is asking: %s\nAsk the user, then answer with: superdeck answer %s yes   (or no)", strings.TrimSpace(iv.Question), v.ID)
	case v.Link != "":
		return "It is connected. In the Claude app it is under Code, or at " + v.Link
	case v.Done:
		return "It stopped straight away: " + strings.TrimSpace(iv.Lede+" "+iv.Detail)
	}
	return "It is starting. `superdeck status` shows how it is doing."
}

var settleFor = 12 * time.Second

func (a *App) ctlLaunch(c ctl) (int, string) {
	to, why := c.ctlAccountID("to")
	if why != "" {
		return no("%s", why)
	}
	dir, why := a.folderNamed(c.get("in"))
	if why != "" {
		return no("%s", why)
	}
	run, err := a.launch(to, dir)
	if err != nil {
		return no("Not launched: %v.", err)
	}
	return 200, fmt.Sprintf("Launched %s under %s (id %s).\n%s", filepath.Base(run.Project), a.accountName(to), run.ID, a.settle(run))
}

func (a *App) ctlNewFolder(c ctl) (int, string) {
	root := strings.TrimSpace(c.get("root"))
	if root == "" && len(a.roots) == 1 {
		root = a.roots[0]
	}
	ok := false
	for _, r := range a.roots {
		if root == tilde(r) {
			root = r
		}
		ok = ok || root == r
	}
	if !ok {
		var opts []string
		for _, r := range a.roots {
			opts = append(opts, tilde(r))
		}
		return no("Say where to make it: superdeck folder NAME WHERE, with WHERE one of: %s.", strings.Join(opts, ", "))
	}
	if !a.allow() {
		return no("Too many requests in a short time; wait a minute.")
	}
	dir, err := createProject(root, strings.TrimSpace(c.get("name")))
	if err != nil {
		return no("Not made: %v.", err)
	}
	a.folderMu.Lock()
	a.folderLs = nil
	a.folderMu.Unlock()
	return 200, fmt.Sprintf("Made %s, an empty Git repository. Launch Claude in it with: superdeck launch ACCOUNT %s", tilde(dir), tilde(dir))
}

// later runs something that will end the session that asked for it, after
// that session has had time to hear the answer.
func (c ctl) later(run *Run, what string, do func()) string {
	if !c.own(run) {
		do()
		return ""
	}
	time.AfterFunc(moveDelay, func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("%s: %v", what, p)
			}
		}()
		do()
	})
	return fmt.Sprintf(" That is the instance this session is running in, so it happens in %d seconds and this session will end.", int(moveDelay.Seconds()))
}

func (a *App) ctlStop(c ctl) (int, string) {
	run, why := c.which()
	if run == nil {
		return no("%s", why)
	}
	name := filepath.Base(run.Project)
	note := c.later(run, "stopping "+name, func() {
		unlock := a.folderLock(run.Project)
		cur := a.current(run)
		a.remember(wanted{Account: cur.Account, Project: cur.Project}, false)
		cur.Stop()
		unlock()
	})
	return 200, fmt.Sprintf("Stopping %s. Any Claude sessions it is hosting end, and it will not come back until it is launched again.%s", name, note)
}

func (a *App) ctlRelaunch(c ctl) (int, string) {
	run, why := c.which()
	if run == nil {
		return no("%s", why)
	}
	if err := a.canStart(run.Account, run.Project, run); err != nil {
		return no("Not relaunched: %v.", err)
	}
	if !a.allow() {
		return no("Too many launches in a short time; wait a minute.")
	}
	name := filepath.Base(run.Project)
	note := c.later(run, "relaunching "+name, func() {
		if _, err := a.relaunch(run); err != nil {
			log.Printf("relaunching %s: %v", name, err)
		}
	})
	return 200, fmt.Sprintf("Relaunching %s. Any Claude sessions it is hosting end.%s", name, note)
}

func (a *App) ctlMove(c ctl) (int, string) {
	run, why := c.which()
	if run == nil {
		return no("%s", why)
	}
	to, why := c.ctlAccountID("to")
	if why != "" {
		return no("%s", why)
	}
	if to == run.Account {
		return no("That is already running under %s.", a.accountName(to))
	}
	if err := a.canStart(to, run.Project, run); err != nil {
		return no("Not moved: %v.", err)
	}
	if !a.allow() {
		return no("Not moved: too many launches in a short time. Wait a minute and try again.")
	}
	notes := strings.TrimSpace(c.get("notes"))
	if len(notes) > maxNotes {
		return no("Not moved: the notes are too long (%d KB at most).", maxNotes>>10)
	}
	if notes != "" {
		if err := a.saveHandoff(run.Project, a.accountName(run.Account), c.get("cwd"), notes); err != nil {
			return no("Not moved: the notes could not be saved (%v).", err)
		}
	}
	name := a.accountName(to)
	note := c.later(run, "moving to "+name, func() {
		if _, err := a.move(run, to); err != nil {
			log.Printf("moving %s to %s: %v", run.Project, name, err)
		}
	})
	kept := "No notes were given, so the next session starts without any."
	if notes != "" {
		kept = "Your notes are saved for the next session."
	}
	return 200, fmt.Sprintf("Moving %s to %s.%s %s\nThe user should start a new session for this folder in the Claude app and ask it to pick up where this left off. If that account has not worked in this folder before, Claude will first ask whether to trust it; `superdeck status` shows the question.", filepath.Base(run.Project), name, note, kept)
}

func (a *App) ctlAnswer(c ctl) (int, string) {
	run, why := c.which()
	if run == nil {
		return no("%s", why)
	}
	v := c.get("answer")
	if v != "yes" && v != "no" {
		return no("Answer yes or no.")
	}
	run = a.current(run)
	switch a.instance(run.View()).Do {
	case "trust", "consent", "question":
	default:
		return no("%s is not asking anything right now.", filepath.Base(run.Project))
	}
	if err := answer(run, v == "yes"); err != nil {
		return no("That could not be typed: %v.", err)
	}
	time.Sleep(answerWait)
	return 200, "Answered " + v + ".\n" + a.settle(run)
}

func (a *App) ctlAddAccount(c ctl) (int, string) {
	if !a.allow() {
		return no("Too many requests in a short time; wait a minute.")
	}
	id, err := a.addAccount(strings.TrimSpace(c.get("name")))
	if err != nil {
		return no("Not added: %v.", err)
	}
	status, text := a.signIn(id)
	if status != 200 {
		a.removeAccount(id) // do not leave behind an account that never got as far as signing in
	}
	return status, text
}

func (a *App) ctlSignIn(c ctl) (int, string) {
	id, why := c.ctlAccountID("name")
	if why != "" {
		return no("%s", why)
	}
	if !a.allow() {
		return no("Too many requests in a short time; wait a minute.")
	}
	return a.signIn(id)
}

// pendingLogin is the sign-in in progress for an account, if there is one.
func (a *App) pendingLogin(id string) *Run {
	for _, v := range a.mgr.List() {
		if v.Kind == "login" && v.Account == id && !v.Done {
			return a.mgr.Get(v.ID)
		}
	}
	return nil
}

// signIn starts Anthropic's own sign-in for an account, or goes back to one
// already waiting, and hands over the address of Anthropic's page.
func (a *App) signIn(id string) (int, string) {
	run := a.pendingLogin(id)
	if run == nil {
		var err error
		if run, err = a.startLogin(id); err != nil {
			return no("The sign-in could not be started: %v.", err)
		}
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && run.alive() && run.View().URL == ""; {
		time.Sleep(100 * time.Millisecond)
	}
	link := run.View().URL
	if link == "" {
		return 500, "The sign-in started but gave no address to open. `superdeck status` may show it in a moment."
	}
	name := a.accountName(id)
	return 200, fmt.Sprintf("To sign %s in, the user opens this address in a browser where they are signed in to the Claude account they want it to use (a private window, if their browser is signed in to a different one):\n\n%s\n\nThe page shows a code. When they give it to you, run: superdeck code %s CODE\nThe sign-in waits 15 minutes.", name, link, name)
}

func (a *App) ctlCode(c ctl) (int, string) {
	id, why := c.ctlAccountID("name")
	if why != "" {
		return no("%s", why)
	}
	run := a.pendingLogin(id)
	if run == nil {
		return no("No sign-in is waiting for %s. Start one with: superdeck signin %s.", a.accountName(id), a.accountName(id))
	}
	if err := submitCode(run, c.get("code")); err != nil {
		return no("%v.", err)
	}
	for deadline := time.Now().Add(10 * time.Second); run.alive() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	v := run.View()
	switch {
	case v.Done && v.ExitCode == 0:
		a.refreshStatus(id)
		who := ""
		if email, _ := a.accountStatusNow(id)["email"].(string); email != "" {
			who = " as " + email
		}
		return 200, fmt.Sprintf("%s is signed in%s.", a.accountName(id), who)
	case v.Done:
		return 400, fmt.Sprintf("The sign-in did not work: the code was wrong or had expired. Start again with: superdeck signin %s", a.accountName(id))
	}
	return 200, "The code was entered; the sign-in has not finished yet. `superdeck accounts` will show when it has."
}

func (a *App) ctlHandoff(c ctl) (int, string) {
	b, err := os.ReadFile(a.handoffPath(c.get("folder")))
	if err != nil {
		return 200, "No notes have been left for this folder."
	}
	return 200, string(b)
}

// Handoff notes are kept per folder, outside the folder itself so they never
// end up in a commit, and replaced by the next move.
func (a *App) handoffPath(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return filepath.Join(a.state, "handoffs", hex.EncodeToString(sum[:12])+".md")
}

func (a *App) saveHandoff(folder, from, cwd, notes string) error {
	path := a.handoffPath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Notes left %s by a session running under the account %q.\n", time.Now().Format("Monday 2 January 2006 at 15:04"), from)
	if filepath.IsAbs(cwd) {
		fmt.Fprintf(&b, "It was working in: %s\n", filepath.Clean(cwd))
	}
	b.WriteString("\n" + strings.ToValidUTF8(notes, "") + "\n")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- the command a session runs ----

// controlCommands are the words that make `superdeck WORD ...` a command to
// the running Superdeck and not a folder to serve.
var controlCommands = map[string]string{
	"status":   "status                      what is running, and anything waiting on a person",
	"accounts": "accounts                    the accounts and whether each is signed in",
	"folders":  "folders [WORD]              folders Claude can be launched in",
	"launch":   "launch ACCOUNT FOLDER       start Claude under an account in a folder",
	"folder":   "folder NAME [WHERE]         make a new folder (an empty Git repository)",
	"move":     "move ACCOUNT [INSTANCE]     move an instance to another account; notes on standard input",
	"handoff":  "handoff                     the notes the last session left for this folder",
	"relaunch": "relaunch [INSTANCE]         stop an instance and start it again",
	"stop":     "stop [INSTANCE]             stop an instance for good",
	"answer":   "answer INSTANCE yes|no      answer a question Claude is asking as it starts",
	"account":  "account add NAME            add an account and start signing it in",
	"signin":   "signin ACCOUNT              sign an account in (again)",
	"code":     "code ACCOUNT CODE           finish a sign-in with the code from Anthropic's page",
}

// controlMain runs one of those commands. Claude runs them from inside an
// instance, where the environment says which instance that is; a person can
// run the same ones in a terminal on the Mac.
func controlMain(defaultState, cmd string, args []string, stdin io.Reader, out io.Writer) int {
	sock := os.Getenv("SUPERDECK_SOCK")
	if sock == "" {
		sock = filepath.Join(runDir(defaultState), "control.sock")
	}
	who := url.Values{"account": {os.Getenv("SUPERDECK_ACCOUNT")}, "folder": {os.Getenv("SUPERDECK_FOLDER")}}
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	usage := func() int {
		fmt.Fprintln(out, "usage: superdeck "+strings.Join(strings.Fields(controlCommands[cmd]), " "))
		return 2
	}
	method, path := "POST", "/"+cmd
	switch cmd {
	case "status", "accounts", "handoff":
		method = "GET"
	case "folders":
		method = "GET"
		who.Set("q", arg(0))
	case "launch":
		if len(args) != 2 {
			return usage()
		}
		who.Set("to", arg(0))
		who.Set("in", arg(1))
	case "folder":
		if len(args) < 1 || len(args) > 2 {
			return usage()
		}
		who.Set("name", arg(0))
		who.Set("root", arg(1))
	case "move":
		if len(args) < 1 || len(args) > 2 {
			return usage()
		}
		who.Set("to", arg(0))
		who.Set("instance", arg(1))
		if cwd, err := os.Getwd(); err == nil {
			who.Set("cwd", cwd)
		}
		// Notes come on standard input when it is not a keyboard.
		if f, ok := stdin.(*os.File); !ok || !isTerminal(f) {
			b, _ := io.ReadAll(io.LimitReader(stdin, maxNotes+1))
			who.Set("notes", string(b))
		}
	case "stop", "relaunch":
		if len(args) > 1 {
			return usage()
		}
		who.Set("instance", arg(0))
	case "answer":
		if len(args) != 2 {
			return usage()
		}
		who.Set("instance", arg(0))
		who.Set("answer", arg(1))
	case "account":
		if len(args) != 2 || arg(0) != "add" {
			return usage()
		}
		who.Set("name", arg(1))
	case "signin":
		if len(args) != 1 {
			return usage()
		}
		who.Set("name", arg(0))
	case "code":
		if len(args) != 2 {
			return usage()
		}
		who.Set("name", arg(0))
		who.Set("code", arg(1))
	default:
		return usage()
	}
	client := &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", sock)
		}}}
	var resp *http.Response
	var err error
	if method == "GET" {
		resp, err = client.Get("http://superdeck" + path + "?" + who.Encode())
	} else {
		resp, err = client.PostForm("http://superdeck"+path, who)
	}
	if err != nil {
		fmt.Fprintln(out, "Superdeck is not answering. It may be restarting (try again in a few seconds), or not running on this Mac.")
		return 1
	}
	defer resp.Body.Close()
	io.Copy(out, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
