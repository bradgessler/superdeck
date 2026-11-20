package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var pages = template.Must(template.ParseFS(uiFS, "ui/templates/*.html"))

// assetVersion is a fingerprint of the stylesheet and script. Pages address
// them with it, so browsers can keep them until they change.
var assetVersion = func() string {
	h := sha256.New()
	for _, f := range []string{"ui/static/app.css", "ui/static/htmx.min.js"} {
		b, _ := uiFS.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}()

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	// Exactly two files are served, by name. Nothing else in the binary is
	// reachable, and there is no directory listing.
	for name, ctype := range map[string]string{"app.css": "text/css; charset=utf-8", "htmx.min.js": "text/javascript; charset=utf-8"} {
		body, _ := uiFS.ReadFile("ui/static/" + name)
		mux.HandleFunc("GET /static/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Write(body)
		})
	}
	mux.HandleFunc("/", notFound) // anything else gets the page with a way back

	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /{$}", a.pageHome)
	mux.HandleFunc("GET /instances", a.partInstances)
	mux.HandleFunc("GET /new", a.pageNew)
	mux.HandleFunc("GET /new/folders", a.partFolders)
	mux.HandleFunc("POST /instances", a.doLaunch)
	mux.HandleFunc("GET /folders/new", a.pageNewFolder)
	mux.HandleFunc("POST /folders", a.doNewFolder)

	mux.HandleFunc("GET /accounts", a.pageAccounts)
	mux.HandleFunc("GET /accounts/new", a.pageNewAccount)
	mux.HandleFunc("POST /accounts", a.doNewAccount)
	mux.HandleFunc("GET /accounts/{id}", a.pageAccount)
	mux.HandleFunc("POST /accounts/{id}/rename", a.doRename)
	mux.HandleFunc("POST /accounts/{id}/signin", a.doSignIn)
	mux.HandleFunc("POST /accounts/{id}/remove", a.doRemoveAccount)

	mux.HandleFunc("GET /instances/{id}", a.pageInstance)
	mux.HandleFunc("GET /instances/{id}/output", a.pageOutput)
	mux.HandleFunc("GET /instances/{id}/screen", a.partScreen)
	mux.HandleFunc("GET /instances/{id}/watch", a.watch)
	mux.HandleFunc("POST /instances/{id}/answer", a.doAnswer)
	mux.HandleFunc("POST /instances/{id}/key", a.doKey)
	mux.HandleFunc("POST /instances/{id}/code", a.doCode)
	mux.HandleFunc("POST /instances/{id}/stop", a.doStop)
	mux.HandleFunc("POST /instances/{id}/relaunch", a.doRelaunch)
	mux.HandleFunc("GET /instances/{id}/move", a.pageMove)
	mux.HandleFunc("POST /instances/{id}/move", a.doMove)
	mux.HandleFunc("POST /update", a.doUpdate)
	mux.HandleFunc("GET /refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("HX-Refresh", "true") // used by pages waiting on something that has no state of its own
		w.WriteHeader(http.StatusNoContent)
	})
	return a.guard(mux)
}

// ---- helpers ----

func render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// page builds the data every full page needs. A message carried over from a
// redirect is shown only if this server signed it, so a link someone crafted
// cannot put words on the page.
func (a *App) page(r *http.Request, title, nav string) map[string]any {
	msg := r.URL.Query().Get("error")
	if len(msg) > 300 || !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(a.sign(msg))) {
		msg = ""
	}
	return map[string]any{"Title": title, "Nav": nav, "Error": msg, "V": assetVersion}
}

func (a *App) sign(msg string) string {
	m := hmac.New(sha256.New, a.flash)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil)[:12])
}

// to sends the browser to another page, for both plain forms and HTMX.
func to(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fail returns to a page and shows what went wrong there.
func (a *App) fail(w http.ResponseWriter, r *http.Request, back string, err error) {
	msg := strings.ToValidUTF8(err.Error(), "")
	if len(msg) > 280 { // page shows nothing longer than 300
		msg = strings.ToValidUTF8(msg[:280], "") + "…"
	}
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	to(w, r, back+sep+"error="+url.QueryEscape(msg)+"&sig="+a.sign(msg))
}

// sure makes a person confirm something that cannot be undone. With HTMX the
// browser has already asked; without it, this shows a page that asks. It
// reports whether to go ahead.
func (a *App) sure(w http.ResponseWriter, r *http.Request, question, yes, back string) bool {
	if r.Header.Get("HX-Request") != "" || r.FormValue("sure") == "yes" {
		return true
	}
	d := a.page(r, "Are you sure?", "")
	d["Question"], d["Yes"], d["Back"], d["Action"] = question, yes, back, r.URL.Path
	fields := map[string]string{}
	for k, v := range r.PostForm {
		if len(v) > 0 && k != "sure" {
			fields[k] = v[0]
		}
	}
	d["Fields"] = fields
	render(w, "confirm.html", d)
	return false
}

// allow limits how fast processes can be started through the page, whoever
// is asking: a burst, then a few a minute.
func (a *App) allow() bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := time.Now()
	a.tokens += now.Sub(a.rateAt).Seconds() / 6 // one every six seconds
	a.rateAt = now
	if a.tokens > 12 {
		a.tokens = 12
	}
	if a.tokens < 1 {
		return false
	}
	a.tokens--
	return true
}

func ago(t time.Time) string {
	s := time.Since(t)
	switch {
	case s < time.Minute:
		return "just now"
	case s < time.Hour:
		return fmt.Sprintf("%dm ago", int(s.Minutes()))
	case s < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(s.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(s.Hours()/24))
}

func cookie(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		if v, err := url.QueryUnescape(c.Value); err == nil {
			return v
		}
	}
	return ""
}

func remember(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: url.QueryEscape(value), Path: "/",
		MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode, HttpOnly: true})
}

// ---- view models ----

type accountView struct {
	ID, Name, Who string
	Email, Org    string
	SignedIn      bool
	Same          string // another account signed in as the same Claude user
	Checking      bool   // sign-in state not known yet
	Pending       string // id of a sign-in in progress
	Selected      bool
}

func (a *App) accountViews(selected string) []accountView {
	// Just after Superdeck starts nothing is known about who is signed in.
	// A check takes a fraction of a second, so give them a moment rather
	// than show every account as unknown.
	for deadline := time.Now().Add(1200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(40 * time.Millisecond) {
		waiting := false
		for _, ac := range a.accountList() {
			c, _ := a.accountStatus(ac.ID)["checking"].(bool)
			waiting = waiting || c
		}
		if !waiting {
			break
		}
	}
	runs := a.mgr.List()
	out := []accountView{}
	for _, ac := range a.accountList() {
		st := a.accountStatus(ac.ID)
		v := accountView{ID: ac.ID, Name: ac.Name, Who: "signed out", Selected: ac.ID == selected}
		if c, _ := st["checking"].(bool); c {
			v.Who, v.Checking = "checking", true
		}
		if in, _ := st["loggedIn"].(bool); in {
			v.SignedIn = true
			v.Email, _ = st["email"].(string)
			v.Org, _ = st["orgName"].(string)
			v.Who = v.Email
			if v.Who == "" {
				v.Who = "signed in"
			}
		}
		for _, r := range runs {
			if r.Kind == "login" && r.Account == ac.ID && !r.Done {
				v.Pending, v.Who = r.ID, "signing in"
			}
		}
		out = append(out, v)
	}
	return out
}

type instanceView struct {
	ID, Kind, Title, Account, AccountID, Sub, Meta, Path string

	State, Cls string // one status word and its colour
	Live, Dim  bool
	IsServer   bool
	IsLogin    bool

	// What the screen says and offers. Lede is one sentence about what is
	// happening; Detail is a line of Claude's own output when that explains
	// it; Do names the single emphasised action, if there is one.
	Lede, Detail, Question string
	Do                     string // trust, consent, question, open, signin, next, again, done
	URL, Link, Next        string
	Outdated               bool
	Installed, Version     string
	Failed, Restarting     bool
	Sig                    string // changes whenever the screen should be redrawn
}

func (a *App) instance(v RunView) instanceView {
	name := a.accountName(v.Account)
	iv := instanceView{ID: v.ID, Kind: v.Kind, Account: name, AccountID: v.Account, Live: !v.Done, Dim: v.Done,
		IsServer: v.Kind == "server", IsLogin: v.Kind == "login", URL: v.URL, Link: v.Link, Version: v.Version, Next: v.Next}
	switch {
	case iv.IsServer:
		iv.Title = filepath.Base(v.Project)
		iv.Path = tilde(v.Project)
	case iv.IsLogin:
		iv.Title = "Sign in " + name
	default:
		iv.Title = v.Name
	}
	lastLine := lastLines(v.Tail(), 1)
	switch {
	// ---- waiting on a person ----
	case v.Hint == "needs-trust" && !v.Done:
		iv.State, iv.Cls, iv.Do = "needs you", "wait", "trust"
		iv.Lede = "Claude is asking whether to trust this folder before it works here. Only trust folders whose contents you know."
	case v.Hint == "needs-consent" && !v.Done:
		iv.State, iv.Cls, iv.Do = "needs you", "wait", "consent"
		iv.Lede = "Claude is asking to turn on Remote Control for " + name + ". It asks once per account, and it is what lets the Claude app reach instances on this Mac."
	case v.Hint == "needs-answer":
		iv.State, iv.Cls, iv.Do, iv.Question = "needs you", "wait", "question", v.Question
		iv.Lede = "Claude is asking a question."

	// ---- running ----
	case v.Lost:
		iv.State, iv.Cls = "not responding", "wait"
		iv.Lede = "Superdeck cannot reach this instance at the moment. It has been left alone and may well still be running; this clears by itself if it answers again."
	case !v.Done && iv.IsLogin:
		iv.State, iv.Cls = "waiting for you", "wait"
		if v.URL == "" && time.Since(v.Started) > 15*time.Second {
			iv.Lede = "slow"
		}
	case !v.Done && iv.IsServer && v.Link != "":
		iv.State, iv.Cls, iv.Do = "running", "on", "open"
		iv.Lede = "Connected. Start a session here from the Claude app, under Code."
		if noCommits(v.Project) {
			iv.State, iv.Cls = "needs a commit", "wait"
			iv.Lede = "Connected, but sessions cannot start here yet. This repository has no commits, and Claude needs one to work from. Make a first commit in this folder; nothing needs restarting."
		}
	case !v.Done && iv.IsServer:
		iv.State, iv.Cls = "starting", "on"
		iv.Lede = "Connecting to Claude…"
		if time.Since(v.Started) > time.Minute {
			iv.Lede = "Still connecting to Claude. This usually takes a few seconds; the terminal shows what it is doing."
		}
	case !v.Done:
		iv.State, iv.Cls = "working", "on"
		iv.Lede = "Checking for a newer Claude Code…"

	// ---- ended, and something comes next ----
	case v.Next != "":
		iv.State, iv.Do = "replaced", "next"
		iv.Lede = "This one ended and was started again."
	case !v.RestartAt.IsZero() && v.Hint == "needs-login":
		iv.State, iv.Cls, iv.Restarting = "restarting", "wait", true
		iv.Lede = "Claude said " + name + " is signed out. Just after Superdeck starts that can be wrong, so it is trying again."
	case !v.RestartAt.IsZero():
		iv.State, iv.Cls, iv.Restarting = "restarting", "wait", true
		iv.Lede = "It stopped unexpectedly. Superdeck is starting it again."

	// ---- ended ----
	case v.Hint == "needs-trust":
		iv.State, iv.Do = "not trusted", "again"
		iv.Lede = "Claude did not start because this folder has not been trusted."
	case v.Hint == "needs-login":
		iv.State, iv.Cls, iv.Do = "signed out", "wait", "signin"
		iv.Lede = name + " is signed out, so Claude could not start. Sign it in and this instance will start again."
		if in, _ := a.accountStatus(v.Account)["loggedIn"].(bool); in {
			iv.Do = "again"
			iv.Lede = name + " was signed out when this started. It is signed in now."
		}
	case iv.IsLogin && v.Stopped:
		iv.State = "cancelled"
	case iv.IsLogin && v.ExitCode == 0:
		iv.State, iv.Cls = "signed in", "on"
	case iv.IsLogin:
		iv.State, iv.Cls, iv.Failed = "failed", "bad", true
	case iv.IsServer && v.Stopped:
		iv.State, iv.Do = "stopped", "again"
		iv.Lede = "This instance was stopped."
	case iv.IsServer && v.ExitCode == 0:
		iv.State, iv.Do = "stopped", "again"
		iv.Lede = "Claude exited."
	case iv.IsServer:
		iv.State, iv.Cls, iv.Failed, iv.Do = "failed", "bad", true, "again"
		iv.Lede = "Claude stopped with an error and was not started again."
		iv.Detail = lastLine
	case v.ExitCode == 0: // an update check
		iv.State, iv.Do = "finished", "done"
		iv.Lede = lastLine
		if iv.Lede == "" {
			iv.Lede = "Done."
		}
	default:
		iv.State, iv.Cls, iv.Failed, iv.Do = "failed", "bad", true, "done"
		iv.Lede = "The update check did not finish."
		iv.Detail = lastLine
	}
	sub := []string{}
	if iv.IsServer {
		sub = append(sub, name)
	}
	sub = append(sub, "started "+ago(v.Started))
	if v.Restarts > 0 {
		sub = append(sub, fmt.Sprintf("restarted %d×", v.Restarts))
	}
	if inst := a.claudeVersion(); iv.IsServer && !v.Done && v.Version != "" && inst != "" && inst != v.Version {
		iv.Outdated, iv.Installed = true, inst
		sub = append(sub, "update ready")
	}
	iv.Sub = strings.Join(sub, " · ")
	meta := sub
	if iv.IsServer && v.Version != "" {
		meta = append(meta, "Claude Code "+v.Version)
	}
	iv.Meta = strings.Join(meta, " · ")
	// Everything that changes what the screen shows or offers. The relative
	// time is left out on purpose: the screen is not redrawn just for that.
	sum := sha256.Sum256([]byte(strings.Join([]string{iv.State, iv.Do, iv.Lede, iv.Detail, iv.Question,
		iv.URL, iv.Link, iv.Next, fmt.Sprint(iv.Outdated, iv.Live, v.Restarts, v.Lost)}, "\x00")))
	iv.Sig = hex.EncodeToString(sum[:8])
	return iv
}

type folderView struct {
	Rel, Name, Parent, Note string
	Selected                bool
}

// folders returns the folder choices for a filter: a short list when nothing
// is typed, more when it is. A folder whose own name matches comes before one
// that only matches by its path, so typing a workspace's name finds the
// workspace first. The selected folder always stays in the list.
func (a *App) folders(q, selected string) (rows []folderView, more, total int) {
	all := a.folderList()
	total = len(all)
	q = strings.ToLower(strings.TrimSpace(q))
	limit := 8
	if q != "" {
		limit = 40
	}
	view := func(f folder) folderView {
		// Every row says where the folder lives, so the grey line is always
		// there and always starts at the same place.
		v := folderView{Rel: f.Path, Name: filepath.Base(f.Path), Parent: tilde(filepath.Dir(f.Path)), Selected: f.Path == selected}
		if f.Repos == 1 {
			v.Note = v.Parent + " · 1 repository inside"
		} else if f.Repos > 1 {
			v.Note = fmt.Sprintf("%s · %d repositories inside", v.Parent, f.Repos)
		}
		return v
	}
	rank := func(f folder) int {
		name := strings.ToLower(filepath.Base(f.Path))
		switch {
		case q == "":
			return 0
		case name == q:
			return 0
		case strings.HasPrefix(name, q):
			return 1
		case strings.Contains(name, q):
			return 2
		case strings.Contains(strings.ToLower(f.Path), q):
			return 3
		}
		return -1
	}
	var hits []folder
	for _, f := range all {
		if f.Path == selected {
			rows = append(rows, view(f))
		} else if rank(f) >= 0 {
			hits = append(hits, f)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return rank(hits[i]) < rank(hits[j]) })
	shown := 0
	for _, f := range hits {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, view(f))
		shown++
	}
	return rows, len(hits) - shown, total
}

// ---- Instances ----

// instanceLists splits instances into what is running, or about to be again,
// and what recently ended. A run that was replaced by a newer one is left
// out: the newer one is the instance.
func (a *App) instanceLists() (live, past []instanceView) {
	for _, v := range a.mgr.List() {
		if v.Kind != "server" || v.Next != "" {
			continue
		}
		if !v.Done || !v.RestartAt.IsZero() {
			live = append(live, a.instance(v))
		} else if len(past) < 5 {
			past = append(past, a.instance(v))
		}
	}
	return
}

func (a *App) pageHome(w http.ResponseWriter, r *http.Request) {
	d := a.page(r, "Instances", "instances")
	d["Live"], d["Past"] = a.instanceLists()
	d["Claude"] = a.claudeVersion()
	for _, v := range a.mgr.List() {
		if v.Kind == "update" && !v.Done {
			d["Updating"] = v.ID
		}
	}
	render(w, "home.html", d)
}

func (a *App) partInstances(w http.ResponseWriter, r *http.Request) {
	d := map[string]any{}
	d["Live"], d["Past"] = a.instanceLists()
	render(w, "instance-list", d)
}

func (a *App) pageNew(w http.ResponseWriter, r *http.Request) {
	d := a.page(r, "New instance", "instances")
	selected := r.URL.Query().Get("folder")
	if selected == "" {
		selected = cookie(r, "sd_folder")
	}
	chosen := r.URL.Query().Get("account") // kept when the filter is submitted as a plain form
	if chosen == "" {
		chosen = cookie(r, "sd_account")
	}
	views := a.accountViews(chosen)
	// Only a signed-in account can be chosen. Pick the remembered one if it
	// still is, otherwise the first that is.
	picked, usable := false, false
	for i := range views {
		usable = usable || views[i].SignedIn
		views[i].Selected = views[i].Selected && views[i].SignedIn
		picked = picked || views[i].Selected
	}
	for i := range views {
		if !picked && views[i].SignedIn {
			views[i].Selected, picked = true, true
		}
	}
	q := r.URL.Query().Get("q") // so the filter also works as a plain form
	d["Accounts"], d["Usable"], d["Q"] = views, usable, q
	for _, v := range views {
		if v.Checking {
			d["Checking"] = true // the page looks again in a moment
		}
	}
	d["Folders"], d["More"], d["Total"] = a.folders(q, selected)
	d["Root"] = a.rootNames()
	render(w, "new.html", d)
}

func (a *App) partFolders(w http.ResponseWriter, r *http.Request) {
	d := map[string]any{"Q": r.URL.Query().Get("q"), "Root": a.rootNames()}
	d["Folders"], d["More"], d["Total"] = a.folders(r.URL.Query().Get("q"), r.URL.Query().Get("folder"))
	render(w, "folder-list", d)
}

func (a *App) doLaunch(w http.ResponseWriter, r *http.Request) {
	acct, folder := r.FormValue("account"), r.FormValue("folder")
	back := "/new?folder=" + url.QueryEscape(folder)
	if acct == "" {
		a.fail(w, r, back, fmt.Errorf("pick an account"))
		return
	}
	if folder == "" {
		a.fail(w, r, back, fmt.Errorf("pick a folder"))
		return
	}
	if _, err := a.accountDir(acct); err != nil {
		a.fail(w, r, back, err)
		return
	}
	if in, _ := a.accountStatusNow(acct)["loggedIn"].(bool); !in {
		a.fail(w, r, "/accounts/"+url.PathEscape(acct), fmt.Errorf("sign in to %s first", a.accountName(acct)))
		return
	}
	if !a.allow() {
		a.fail(w, r, back, fmt.Errorf("too many launches in a short time; wait a moment"))
		return
	}
	// Lock the folder by its real path, as everything else does.
	dir, err := resolveProject(a.roots, folder)
	if err != nil {
		a.fail(w, r, back, err)
		return
	}
	unlock := a.folderLock(dir)
	run, err := a.startServer(acct, dir, "", 0, 0)
	if err == nil {
		a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	}
	unlock()
	if err != nil {
		a.fail(w, r, back, err)
		return
	}
	remember(w, "sd_account", acct)
	remember(w, "sd_folder", folder)
	to(w, r, "/instances/"+run.ID)
}

func (a *App) pageNewFolder(w http.ResponseWriter, r *http.Request) {
	d := a.page(r, "New folder", "instances")
	type choice struct{ Path, Name string }
	var roots []choice
	for _, root := range a.roots {
		roots = append(roots, choice{root, tilde(root)})
	}
	d["Roots"] = roots
	render(w, "folder-new.html", d)
}

func (a *App) doNewFolder(w http.ResponseWriter, r *http.Request) {
	in := r.FormValue("in")
	allowed := false
	for _, root := range a.roots {
		allowed = allowed || root == in
	}
	if !allowed {
		a.fail(w, r, "/folders/new", fmt.Errorf("pick where to create the folder"))
		return
	}
	if !a.allow() {
		a.fail(w, r, "/folders/new", fmt.Errorf("too many requests in a short time; wait a moment"))
		return
	}
	rel, err := createProject(in, strings.TrimSpace(r.FormValue("name")))
	if err != nil {
		a.fail(w, r, "/folders/new", err)
		return
	}
	a.folderMu.Lock()
	a.folderLs = nil
	a.folderMu.Unlock()
	to(w, r, "/new?folder="+url.QueryEscape(rel))
}

// ---- Accounts ----

// sameLogin notes accounts that are signed in as the same Claude user, which
// is easy to do by accident and defeats the point of having two.
func sameLogin(views []accountView) {
	for i := range views {
		for j := range views {
			if i != j && views[i].Email != "" && views[i].Email == views[j].Email {
				views[i].Same = views[j].Name
			}
		}
	}
}

func (a *App) pageAccounts(w http.ResponseWriter, r *http.Request) {
	d := a.page(r, "Accounts", "accounts")
	views := a.accountViews("")
	sameLogin(views)
	d["Accounts"] = views
	for _, v := range views {
		if v.Checking {
			d["Checking"] = true
		}
	}
	render(w, "accounts.html", d)
}

// pageAccount is one account: who it is signed in as and what you can do.
func (a *App) pageAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	views := a.accountViews("")
	sameLogin(views)
	for _, v := range views {
		if v.ID != id {
			continue
		}
		d := a.page(r, v.Name, "accounts")
		d["A"], d["Checking"] = v, v.Checking
		var running []instanceView
		for _, rv := range a.mgr.List() {
			if rv.Kind == "server" && rv.Account == id && rv.Next == "" && (!rv.Done || !rv.RestartAt.IsZero()) {
				running = append(running, a.instance(rv))
			}
		}
		d["Running"] = running
		render(w, "account.html", d)
		return
	}
	notFound(w, r)
}

func (a *App) pageNewAccount(w http.ResponseWriter, r *http.Request) {
	render(w, "account-new.html", a.page(r, "Add account", "accounts"))
}

func (a *App) doNewAccount(w http.ResponseWriter, r *http.Request) {
	if !a.allow() {
		a.fail(w, r, "/accounts/new", fmt.Errorf("too many requests in a short time; wait a moment"))
		return
	}
	id, err := a.addAccount(strings.TrimSpace(r.FormValue("name")))
	if err != nil {
		a.fail(w, r, "/accounts/new", err)
		return
	}
	run, err := a.startLogin(id)
	if err != nil {
		a.removeAccount(id) // do not leave behind an account that never got as far as signing in
		a.fail(w, r, "/accounts", err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

func (a *App) doRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.setName(id, strings.TrimSpace(r.FormValue("name"))); err != nil {
		a.fail(w, r, "/accounts/"+url.PathEscape(id), err)
		return
	}
	to(w, r, "/accounts/"+url.PathEscape(id))
}

func (a *App) doRemoveAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.accountDir(id); err != nil {
		notFound(w, r)
		return
	}
	if !a.sure(w, r, "Remove "+a.accountName(id)+"? It will be signed out on this Mac and its settings deleted.", "Remove account", "/accounts/"+url.PathEscape(id)) {
		return
	}
	if err := a.removeAccount(id); err != nil {
		a.fail(w, r, "/accounts/"+url.PathEscape(id), err)
		return
	}
	to(w, r, "/accounts")
}

func (a *App) doSignIn(w http.ResponseWriter, r *http.Request) {
	// If a sign-in for this account is already waiting, go back to it.
	for _, v := range a.mgr.List() {
		if v.Kind == "login" && v.Account == r.PathValue("id") && !v.Done {
			to(w, r, "/instances/"+v.ID)
			return
		}
	}
	if !a.allow() {
		a.fail(w, r, "/accounts/"+url.PathEscape(r.PathValue("id")), fmt.Errorf("too many requests in a short time; wait a moment"))
		return
	}
	run, err := a.startLogin(r.PathValue("id"))
	if err != nil {
		a.fail(w, r, "/accounts/"+url.PathEscape(r.PathValue("id")), err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

// ---- One instance ----

// notFound is a proper page, with a way back, for an address that is no
// longer anything: an instance from before a restart, a removed account. A
// page that was polling such an address is told to reload, so it stops.
func notFound(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Refresh", "true")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	pages.ExecuteTemplate(w, "notfound.html", map[string]any{"Title": "Not found", "Nav": "", "V": assetVersion})
}

func (a *App) run(w http.ResponseWriter, r *http.Request) *Run {
	run := a.mgr.Get(r.PathValue("id"))
	if run == nil {
		notFound(w, r)
	}
	return run
}

// pageOutput is the terminal for one process: what Claude has printed, and
// the handful of keys that can be pressed in it.
func (a *App) pageOutput(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	iv := a.instance(run.View())
	d := a.page(r, iv.Title+" output", "instances")
	if iv.IsLogin {
		d["Nav"] = "accounts"
	}
	d["I"], d["Screen"], d["Wide"] = iv, run.Screen(), true
	render(w, "output.html", d)
}

func (a *App) pageInstance(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	if cur := a.current(run); cur != run {
		to(w, r, "/instances/"+cur.ID)
		return
	}
	iv := a.instance(run.View())
	d := a.page(r, iv.Title, "instances")
	if iv.IsLogin {
		d["Nav"] = "accounts"
	}
	d["I"], d["Screen"] = iv, run.Screen()
	if iv.IsLogin {
		render(w, "signin.html", d)
		return
	}
	render(w, "instance.html", d)
}

// watch lets a page that is otherwise still notice that something changed.
// The page sends the signature of what it is showing; while that still
// matches, the answer is empty and nothing is redrawn, so nothing moves under
// a finger or a cursor. When it no longer matches, the browser is told to
// load the page afresh, or to go where the outcome is.
func (a *App) watch(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	v := run.View()
	switch {
	case v.Done && v.Kind == "login" && v.ExitCode == 0 && !v.Stopped:
		w.Header().Set("HX-Redirect", "/accounts/"+url.PathEscape(v.Account))
	case a.instance(v).Sig != r.URL.Query().Get("sig"):
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) partScreen(w http.ResponseWriter, r *http.Request) {
	if run := a.run(w, r); run != nil {
		if !run.alive() {
			// The page was showing a live terminal; reload it once so it
			// stops polling and says the process has ended.
			w.Header().Set("HX-Refresh", "true")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(run.Screen()))
	}
}

func (a *App) doAnswer(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	back := "/instances/" + run.ID
	v := r.FormValue("answer")
	if v != "yes" && v != "no" {
		a.fail(w, r, back, fmt.Errorf("answer yes or no"))
		return
	}
	// The answer is for the question the page showed. If Claude has moved on
	// since, show what it is asking now and type nothing.
	if a.instance(run.View()).Do != r.FormValue("to") {
		to(w, r, back)
		return
	}
	if err := answer(run, v == "yes"); err != nil {
		a.fail(w, r, back, err)
		return
	}
	to(w, r, back)
}

func (a *App) doKey(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	back := "/instances/" + run.ID
	back += "/output"
	if err := pressKey(run, r.FormValue("key")); err != nil {
		a.fail(w, r, back, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	to(w, r, back)
}

func (a *App) doCode(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	back := "/instances/" + run.ID
	if err := submitCode(run, r.FormValue("code")); err != nil {
		a.fail(w, r, back, err)
		return
	}
	// Give Claude a moment to finish the sign-in before showing the result.
	for deadline := time.Now().Add(8 * time.Second); run.alive() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	if v := run.View(); v.Done && v.ExitCode == 0 {
		a.refreshStatus(run.Account)
		to(w, r, "/accounts/"+url.PathEscape(run.Account))
		return
	}
	to(w, r, back)
}

func (a *App) doStop(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	if run.Kind != "server" {
		run.Stop()
		for deadline := time.Now().Add(3 * time.Second); run.alive() && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
		if run.Kind == "login" {
			to(w, r, "/accounts/"+url.PathEscape(run.Account))
			return
		}
		to(w, r, "/")
		return
	}
	// Whatever tab this came from, it means the instance as it is now.
	unlock := a.folderLock(run.Project)
	run = a.current(run)
	title := filepath.Base(run.Project)
	if run.alive() && !a.sure(w, r, "Stop "+title+"? Any Claude sessions it is hosting will end.", "Stop instance", "/instances/"+run.ID) {
		unlock()
		return
	}
	a.remember(wanted{Account: run.Account, Project: run.Project}, false)
	run.Stop() // for one that is waiting to restart, this cancels the restart
	unlock()
	for deadline := time.Now().Add(3 * time.Second); run.alive() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	to(w, r, "/")
}

func (a *App) doRelaunch(w http.ResponseWriter, r *http.Request) {
	old := a.run(w, r)
	if old == nil {
		return
	}
	if cur := a.current(old); cur.alive() && !a.sure(w, r, "Relaunch "+filepath.Base(cur.Project)+"? Any Claude sessions it is hosting will end.", "Relaunch", "/instances/"+cur.ID) {
		return
	}
	if !a.allow() {
		a.fail(w, r, "/instances/"+old.ID, fmt.Errorf("too many launches in a short time; wait a moment"))
		return
	}
	run, err := a.relaunch(old)
	if err != nil {
		a.fail(w, r, "/instances/"+old.ID, err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

// pageMove lets you pick another account to run an instance under.
func (a *App) pageMove(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	if run.Kind != "server" {
		notFound(w, r)
		return
	}
	run = a.current(run)
	iv := a.instance(run.View())
	d := a.page(r, "Move "+iv.Title, "instances")
	var others []accountView
	for _, v := range a.accountViews("") {
		if v.ID != run.Account {
			others = append(others, v)
		}
	}
	d["I"], d["Others"] = iv, others
	render(w, "move.html", d)
}

func (a *App) doMove(w http.ResponseWriter, r *http.Request) {
	old := a.run(w, r)
	if old == nil {
		return
	}
	if cur := a.current(old); cur.alive() && !a.sure(w, r, "Move "+filepath.Base(cur.Project)+" to "+a.accountName(r.FormValue("account"))+"? Any Claude sessions it is hosting will end.", "Move it", "/instances/"+cur.ID+"/move") {
		return
	}
	if !a.allow() {
		a.fail(w, r, "/instances/"+old.ID+"/move", fmt.Errorf("too many launches in a short time; wait a moment"))
		return
	}
	run, err := a.move(old, r.FormValue("account"))
	if err != nil {
		a.fail(w, r, "/instances/"+old.ID+"/move", err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

func (a *App) doUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.allow() {
		a.fail(w, r, "/", fmt.Errorf("too many requests in a short time; wait a moment"))
		return
	}
	run, err := a.startUpdate()
	if err != nil {
		a.fail(w, r, "/", err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

// rootNames lists the allowed folders for display.
func (a *App) rootNames() string {
	var names []string
	for _, root := range a.roots {
		names = append(names, tilde(root))
	}
	return strings.Join(names, ", ")
}
