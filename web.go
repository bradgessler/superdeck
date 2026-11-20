package main

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var pages = template.Must(template.ParseFS(uiFS, "ui/templates/*.html"))

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /static/", http.FileServer(http.FS(static)))

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

	mux.HandleFunc("GET /instances/{id}", a.pageInstance)
	mux.HandleFunc("GET /instances/{id}/output", a.pageOutput)
	mux.HandleFunc("GET /instances/{id}/live", a.partLive)
	mux.HandleFunc("GET /instances/{id}/screen", a.partScreen)
	mux.HandleFunc("GET /instances/{id}/watch", a.watch)
	mux.HandleFunc("POST /instances/{id}/answer", a.doAnswer)
	mux.HandleFunc("POST /instances/{id}/key", a.doKey)
	mux.HandleFunc("POST /instances/{id}/code", a.doCode)
	mux.HandleFunc("POST /instances/{id}/stop", a.doStop)
	mux.HandleFunc("POST /instances/{id}/relaunch", a.doRelaunch)
	mux.HandleFunc("POST /update", a.doUpdate)
	return a.guard(mux)
}

// ---- helpers ----

func render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// page builds the data every full page needs.
func page(r *http.Request, title, nav string) map[string]any {
	msg := r.URL.Query().Get("error")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return map[string]any{"Title": title, "Nav": nav, "Error": msg}
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
func fail(w http.ResponseWriter, r *http.Request, back string, err error) {
	msg := err.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	to(w, r, back+sep+"error="+url.QueryEscape(msg))
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
	Pending       string // id of a sign-in in progress
	Selected      bool
}

func (a *App) accountViews(selected string) []accountView {
	runs := a.mgr.List()
	out := []accountView{}
	for _, ac := range a.accountList() {
		st := a.accountStatus(ac.ID)
		v := accountView{ID: ac.ID, Name: ac.Name, Who: "Signed out", Selected: ac.ID == selected}
		if in, _ := st["loggedIn"].(bool); in {
			v.SignedIn = true
			v.Email, _ = st["email"].(string)
			v.Org, _ = st["orgName"].(string)
			v.Who = v.Email
			if v.Who == "" {
				v.Who = "Signed in"
			}
		}
		for _, r := range runs {
			if r.Kind == "login" && r.Account == ac.ID && !r.Done {
				v.Pending, v.Who = r.ID, "Sign-in in progress"
			}
		}
		out = append(out, v)
	}
	return out
}

type instanceView struct {
	ID, Kind, Title, Account, AccountID, Sub, Meta, Path string
	Question                                             string
	State, Cls                                           string
	Live, Dim, IsServer, IsLogin                         bool
	Ask                                                  string // "trust", "consent" or ""
	Notice                                               string
	URL, Link                                            string
	Outdated                                             bool
	Installed, Version                                   string
	Failed                                               bool
}

func (a *App) instance(v RunView) instanceView {
	name := a.accountName(v.Account)
	iv := instanceView{ID: v.ID, Kind: v.Kind, Account: name, AccountID: v.Account, Live: !v.Done, Dim: v.Done,
		IsServer: v.Kind == "server", IsLogin: v.Kind == "login", URL: v.URL, Link: v.Link, Version: v.Version}
	switch {
	case iv.IsServer:
		iv.Title = filepath.Base(v.Project)
		iv.Path = tilde(v.Project)
	case iv.IsLogin:
		iv.Title = "Sign in " + name
	default:
		iv.Title = v.Name
	}
	switch {
	case v.Hint == "needs-trust" && !v.Done:
		iv.State, iv.Cls, iv.Ask = "needs you", "wait", "trust"
	case v.Hint == "needs-consent" && !v.Done:
		iv.State, iv.Cls, iv.Ask = "needs you", "wait", "consent"
	case v.Hint == "needs-answer":
		iv.State, iv.Cls, iv.Ask, iv.Question = "needs you", "wait", "question", v.Question
	case v.Hint == "needs-trust":
		iv.State, iv.Cls = "not trusted", "wait"
		iv.Notice = "Claude would not start here because this folder has not been trusted yet."
	case v.Hint == "needs-login":
		iv.State, iv.Cls = "signed out", "wait"
		iv.Notice = name + " is signed out. Sign it in under Accounts, then launch again."
	case !v.Done && iv.IsLogin:
		iv.State, iv.Cls = "waiting for you", "wait"
		if v.URL == "" && time.Since(v.Started) > 15*time.Second {
			iv.Notice = "Claude has not produced a sign-in link yet. Look at the output below, or cancel and try again."
		}
	case iv.IsLogin && v.Stopped:
		iv.State = "cancelled"
	case iv.IsLogin && v.ExitCode == 0:
		iv.State, iv.Cls = "signed in", "on"
	case iv.IsLogin:
		iv.State, iv.Cls, iv.Failed = "sign-in failed", "bad", true
		iv.Notice = "Claude did not accept that sign-in. Codes expire quickly and work once; start again to get a fresh one."
	case !v.Done && iv.IsServer:
		iv.State, iv.Cls = "running", "on"
	case !v.Done:
		iv.State, iv.Cls = "working", "on"
	case v.Stopped || v.ExitCode == 0:
		iv.State = "finished"
		if iv.IsServer {
			iv.State = "stopped"
		}
	default:
		iv.State, iv.Cls, iv.Failed = "failed", "bad", true
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
	all := listProjects(a.roots)
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
			v.Note = v.Parent + " · holds 1 repo"
		} else if f.Repos > 1 {
			v.Note = fmt.Sprintf("%s · holds %d repos", v.Parent, f.Repos)
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

func (a *App) instanceLists() (live, past []instanceView) {
	for _, v := range a.mgr.List() {
		if v.Kind != "server" {
			continue
		}
		if !v.Done {
			live = append(live, a.instance(v))
		} else if len(past) < 5 {
			past = append(past, a.instance(v))
		}
	}
	return
}

func (a *App) pageHome(w http.ResponseWriter, r *http.Request) {
	d := page(r, "Instances", "instances")
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
	d := page(r, "New instance", "instances")
	selected := r.URL.Query().Get("folder")
	if selected == "" {
		selected = cookie(r, "sd_folder")
	}
	acct := cookie(r, "sd_account")
	views := a.accountViews(acct)
	picked := false
	for _, v := range views {
		picked = picked || v.Selected
	}
	if !picked {
		for i := range views {
			if views[i].SignedIn || i == len(views)-1 {
				views[i].Selected = true
				break
			}
		}
	}
	d["Accounts"] = views
	d["Folders"], d["More"], d["Total"] = a.folders("", selected)
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
	if acct == "" {
		fail(w, r, "/new", fmt.Errorf("pick an account"))
		return
	}
	if folder == "" {
		fail(w, r, "/new", fmt.Errorf("pick a folder"))
		return
	}
	remember(w, "sd_account", acct)
	remember(w, "sd_folder", folder)
	if in, _ := a.accountStatus(acct)["loggedIn"].(bool); !in {
		fail(w, r, "/accounts", fmt.Errorf("%s is signed out; sign it in first", a.accountName(acct)))
		return
	}
	run, err := a.startServer(acct, folder, "", 0, 0)
	if err != nil {
		fail(w, r, "/new", err)
		return
	}
	a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	to(w, r, "/instances/"+run.ID)
}

func (a *App) pageNewFolder(w http.ResponseWriter, r *http.Request) {
	d := page(r, "New folder", "instances")
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
		fail(w, r, "/folders/new", fmt.Errorf("pick where to create the folder"))
		return
	}
	rel, err := createProject(in, strings.TrimSpace(r.FormValue("name")))
	if err != nil {
		fail(w, r, "/folders/new", err)
		return
	}
	to(w, r, "/new?folder="+url.QueryEscape(rel))
}

// ---- Accounts ----

func (a *App) pageAccounts(w http.ResponseWriter, r *http.Request) {
	d := page(r, "Accounts", "accounts")
	d["Accounts"] = a.accountViews("")
	render(w, "accounts.html", d)
}

// pageAccount is one account: who it is signed in as and what you can do.
func (a *App) pageAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, v := range a.accountViews("") {
		if v.ID != id {
			continue
		}
		d := page(r, v.Name, "accounts")
		d["A"] = v
		var running []instanceView
		for _, rv := range a.mgr.List() {
			if rv.Kind == "server" && rv.Account == id && !rv.Done {
				running = append(running, a.instance(rv))
			}
		}
		d["Running"] = running
		render(w, "account.html", d)
		return
	}
	http.NotFound(w, r)
}

func (a *App) pageNewAccount(w http.ResponseWriter, r *http.Request) {
	render(w, "account-new.html", page(r, "Add account", "accounts"))
}

func (a *App) doNewAccount(w http.ResponseWriter, r *http.Request) {
	id, err := a.addAccount(strings.TrimSpace(r.FormValue("name")))
	if err != nil {
		fail(w, r, "/accounts/new", err)
		return
	}
	run, err := a.startLogin(id)
	if err != nil {
		fail(w, r, "/accounts", err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

func (a *App) doRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.setName(id, strings.TrimSpace(r.FormValue("name"))); err != nil {
		fail(w, r, "/accounts/"+url.PathEscape(id), err)
		return
	}
	to(w, r, "/accounts/"+url.PathEscape(id))
}

func (a *App) doSignIn(w http.ResponseWriter, r *http.Request) {
	// If a sign-in for this account is already waiting, go back to it.
	for _, v := range a.mgr.List() {
		if v.Kind == "login" && v.Account == r.PathValue("id") && !v.Done {
			to(w, r, "/instances/"+v.ID)
			return
		}
	}
	run, err := a.startLogin(r.PathValue("id"))
	if err != nil {
		fail(w, r, "/accounts/"+url.PathEscape(r.PathValue("id")), err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

// ---- One instance ----

func (a *App) run(w http.ResponseWriter, r *http.Request) *Run {
	run := a.mgr.Get(r.PathValue("id"))
	if run == nil {
		http.NotFound(w, r)
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
	d := page(r, iv.Title+" output", "instances")
	if iv.IsLogin {
		d["Nav"] = "accounts"
	}
	d["I"], d["Screen"] = iv, run.Screen()
	render(w, "output.html", d)
}

func (a *App) pageInstance(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	iv := a.instance(run.View())
	d := page(r, iv.Title, "instances")
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

func (a *App) partLive(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	iv := a.instance(run.View())
	if !iv.Live {
		// The process ended: reload once so the whole page settles.
		w.Header().Set("HX-Refresh", "true")
	}
	render(w, "instance-live", map[string]any{"I": iv})
}

// watch lets a page that is otherwise still notice that something changed.
// It answers with nothing until the sign-in link appears or the process ends,
// then tells the browser where to go. The page itself is never redrawn
// underneath you while you are typing.
func (a *App) watch(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	v := run.View()
	switch {
	case v.Done && v.Kind == "login" && v.ExitCode == 0 && !v.Stopped:
		w.Header().Set("HX-Redirect", "/accounts/"+url.PathEscape(v.Account))
	case v.Done, v.URL != "" && r.URL.Query().Get("url") == "0":
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) partScreen(w http.ResponseWriter, r *http.Request) {
	if run := a.run(w, r); run != nil {
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
		fail(w, r, back, fmt.Errorf("answer yes or no"))
		return
	}
	if err := answer(run, v == "yes"); err != nil {
		fail(w, r, back, err)
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
		fail(w, r, back, err)
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
		fail(w, r, back, err)
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
	if run.Kind == "server" {
		a.remember(wanted{Account: run.Account, Project: run.Project}, false)
	}
	run.Stop()
	for deadline := time.Now().Add(3 * time.Second); run.alive() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if run.Kind == "login" {
		to(w, r, "/accounts/"+url.PathEscape(run.Account))
		return
	}
	to(w, r, "/")
}

func (a *App) doRelaunch(w http.ResponseWriter, r *http.Request) {
	old := a.run(w, r)
	if old == nil {
		return
	}
	run, err := a.relaunch(old)
	if err != nil {
		fail(w, r, "/instances/"+old.ID, err)
		return
	}
	to(w, r, "/instances/"+run.ID)
}

func (a *App) doUpdate(w http.ResponseWriter, r *http.Request) {
	run, err := a.startUpdate()
	if err != nil {
		fail(w, r, "/", err)
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
