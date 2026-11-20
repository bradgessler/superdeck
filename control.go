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
// What can be asked for is deliberately little: list the accounts, move the
// instance the caller is running in, leave notes, read notes. Account names
// are matched against the accounts that exist; nothing a session sends
// reaches a command line.

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
	mux.HandleFunc("GET /accounts", a.ctlAccounts)
	mux.HandleFunc("POST /move", a.ctlMove)
	mux.HandleFunc("GET /handoff", a.ctlHandoff)
	srv := &http.Server{Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("the socket sessions use stopped: %v", err)
		}
	}()
}

func say(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, format+"\n", args...)
}

// callerInstance finds the running instance a session says it is in.
func (a *App) callerInstance(account, folder string) *Run {
	for _, v := range a.mgr.List() {
		if v.Kind == "server" && !v.Done && v.Account == account && v.Project == folder {
			return a.mgr.Get(v.ID)
		}
	}
	return nil
}

func (a *App) ctlAccounts(w http.ResponseWriter, r *http.Request) {
	here := r.URL.Query().Get("account")
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
		if ac.ID == here {
			mark = "  <- this session"
		}
		fmt.Fprintf(&b, "%s  (%s)%s\n", ac.Name, state, mark)
	}
	if b.Len() == 0 {
		b.WriteString("There are no accounts.\n")
	}
	say(w, 200, "%s", strings.TrimRight(b.String(), "\n"))
}

func (a *App) ctlMove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxNotes+8<<10)
	account, folder, want := r.FormValue("account"), r.FormValue("folder"), strings.TrimSpace(r.FormValue("to"))
	run := a.callerInstance(account, folder)
	if run == nil {
		say(w, 404, "Superdeck is not running an instance for this account in this folder, so there is nothing to move.")
		return
	}
	// The name is looked up among the accounts that exist. By the name
	// people see first, then by the directory name.
	to := ""
	for _, ac := range a.accountList() {
		if strings.EqualFold(ac.Name, want) {
			to = ac.ID
		}
	}
	if to == "" {
		for _, ac := range a.accountList() {
			if ac.ID == want {
				to = ac.ID
			}
		}
	}
	switch {
	case to == "":
		say(w, 400, "There is no account called %q. Run `\"$SUPERDECK\" accounts` to see them.", want)
		return
	case to == run.Account:
		say(w, 400, "This is already running under %s.", a.accountName(to))
		return
	}
	if err := a.canStart(to, run.Project, run); err != nil {
		say(w, 409, "Not moved: %v. Nothing has changed.", err)
		return
	}
	if !a.allow() {
		say(w, 429, "Not moved: too many launches in a short time. Wait a minute and try again. Nothing has changed.")
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if len(notes) > maxNotes {
		say(w, 400, "Not moved: the notes are too long (%d KB at most). Nothing has changed.", maxNotes>>10)
		return
	}
	if notes != "" {
		if err := a.saveHandoff(run.Project, a.accountName(run.Account), r.FormValue("cwd"), notes); err != nil {
			say(w, 500, "Not moved: the notes could not be saved (%v). Nothing has changed.", err)
			return
		}
	}
	name := a.accountName(to)
	log.Printf("a session asked to move %s from %s to %s", run.Project, a.accountName(run.Account), name)
	time.AfterFunc(moveDelay, func() {
		defer func() { recover() }()
		if _, err := a.move(run, to); err != nil {
			log.Printf("moving %s to %s: %v", run.Project, name, err)
		}
	})
	kept := "No notes were given, so the next session starts without any."
	if notes != "" {
		kept = "Your notes are saved for the next session."
	}
	say(w, 200, "Moving to %s in %d seconds. This session will end. %s\nThe user should start a new session for this folder in the Claude app and ask it to pick up where this left off. If that account has not worked in this folder before, Claude will first ask whether to trust it, and the user answers that on the Superdeck page.", name, int(moveDelay.Seconds()), kept)
}

func (a *App) ctlHandoff(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(a.handoffPath(r.URL.Query().Get("folder")))
	if err != nil {
		say(w, 200, "No notes have been left for this folder.")
		return
	}
	say(w, 200, "%s", strings.TrimRight(string(b), "\n"))
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

// controlMain is `superdeck accounts`, `superdeck move NAME` and `superdeck
// handoff`, run by Claude from inside an instance.
func controlMain(cmd string, args []string, stdin io.Reader, out io.Writer) int {
	sock, account, folder := os.Getenv("SUPERDECK_SOCK"), os.Getenv("SUPERDECK_ACCOUNT"), os.Getenv("SUPERDECK_FOLDER")
	if sock == "" || account == "" || folder == "" {
		fmt.Fprintln(out, "This only works inside a Claude session that Superdeck started. If this is one, it was started before this was possible: relaunch the instance from the Superdeck page.")
		return 1
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", sock)
		}}}
	who := url.Values{"account": {account}, "folder": {folder}}
	var resp *http.Response
	var err error
	switch cmd {
	case "accounts":
		resp, err = client.Get("http://superdeck/accounts?" + who.Encode())
	case "handoff":
		resp, err = client.Get("http://superdeck/handoff?" + who.Encode())
	case "move":
		if len(args) != 1 {
			fmt.Fprintln(out, "usage: superdeck move ACCOUNT   (handoff notes on standard input)")
			return 2
		}
		who.Set("to", args[0])
		if cwd, err := os.Getwd(); err == nil {
			who.Set("cwd", cwd)
		}
		// Notes come on standard input when it is not a keyboard.
		if f, ok := stdin.(*os.File); !ok || !isTerminal(f) {
			b, _ := io.ReadAll(io.LimitReader(stdin, maxNotes+1))
			who.Set("notes", string(b))
		}
		resp, err = client.PostForm("http://superdeck/move", who)
	}
	if err != nil {
		fmt.Fprintln(out, "Superdeck is not answering right now. It may be restarting; try again in a few seconds.")
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
