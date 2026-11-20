package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed ui
var uiFS embed.FS

type App struct {
	root, state, claude string
	hosts               map[string]bool
	mgr                 *Manager

	statusMu sync.Mutex
	status   map[string]map[string]any
	statusAt map[string]time.Time

	verMu   sync.Mutex
	ver     string
	verAt   time.Time
	closing atomic.Bool
	wantMu  sync.Mutex
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7433", "address to listen on (use the Mac's LAN address to reach it over a VPN)")
	root := flag.String("root", filepath.Join(home, "Projects"), "directory that holds your projects")
	state := flag.String("state", filepath.Join(home, ".superdeck"), "directory for per-account Claude config")
	claude := flag.String("claude", "", "path to the claude binary (default: found on PATH)")
	extra := flag.String("hosts", "", "extra Host header values to accept, comma separated")
	agent := flag.Bool("launch-agent", false, "print a launchd agent that runs Superdeck at login with these settings, then exit")
	flag.Parse()

	a := &App{root: *root, state: *state, mgr: NewManager(), status: map[string]map[string]any{}, statusAt: map[string]time.Time{}, hosts: map[string]bool{}}
	bin := *claude
	if bin == "" {
		bin = "claude"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		log.Fatalf("claude binary not found: %v", err)
	}
	if a.claude, err = filepath.Abs(p); err != nil {
		log.Fatal(err)
	}
	if *agent {
		self, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		args := []string{"-addr", *addr, "-root", a.root, "-state", a.state, "-claude", a.claude}
		if *extra != "" {
			args = append(args, "-hosts", *extra)
		}
		path := filepath.Dir(a.claude) + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
		fmt.Print(launchAgent(self, args, filepath.Join(a.state, "superdeck.log"), path))
		return
	}
	if err := os.MkdirAll(filepath.Join(a.state, "accounts"), 0o700); err != nil {
		log.Fatal(err)
	}
	if err := a.allowHosts(*addr, *extra); err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Addr: *addr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("stopping claude processes")
		a.closing.Store(true)
		a.mgr.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("superdeck on http://%s  projects=%s  claude=%s", *addr, a.root, a.claude)
	log.Print("no sign-in yet: anyone who can reach this address can use it")
	go a.restore()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// allowHosts fixes the set of Host header values the server answers to, which
// blocks DNS-rebinding attacks from web pages open on the same network.
func (a *App) allowHosts(addr, extra string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	switch {
	case host == "" || (ip != nil && ip.IsUnspecified()):
		if extra == "" {
			return errors.New("listening on every interface needs -hosts with the names you will browse to")
		}
	case ip != nil && ip.IsLoopback():
		a.hosts[net.JoinHostPort("localhost", port)] = true
		a.hosts[addr] = true
	default:
		a.hosts[addr] = true
	}
	for _, h := range strings.Split(extra, ",") {
		if h = strings.TrimSpace(h); h != "" {
			a.hosts[h] = true
		}
	}
	return nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	static := http.FileServer(http.FS(sub))
	mux.Handle("GET /", static)
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/accounts", a.handleAddAccount)
	mux.HandleFunc("POST /api/accounts/{name}/login", a.handleAccountLogin)
	mux.HandleFunc("POST /api/accounts/{name}/refresh", a.handleAccountRefresh)
	mux.HandleFunc("POST /api/projects", a.handleAddProject)
	mux.HandleFunc("POST /api/servers", a.handleStartServer)
	mux.HandleFunc("POST /api/update", a.handleUpdate)
	mux.HandleFunc("POST /api/runs/{id}/stop", a.handleStop)
	mux.HandleFunc("POST /api/runs/{id}/relaunch", a.handleRelaunch)
	mux.HandleFunc("POST /api/runs/{id}/input", a.handleInput)
	mux.HandleFunc("POST /api/runs/{id}/resize", a.handleResize)
	mux.HandleFunc("GET /api/runs/{id}/stream", a.handleStream)
	return a.guard(mux)
}

// guard applies a Host allow-list and a same-origin check on every
// state-changing request, so a web page elsewhere cannot drive this server
// through your browser. There is no sign-in yet.
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hosts[r.Host] {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *App) accounts() []string {
	es, _ := os.ReadDir(filepath.Join(a.state, "accounts"))
	var out []string
	for _, e := range es {
		if e.IsDir() && accountNameRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) accountDir(name string) (string, error) {
	if !accountNameRe.MatchString(name) {
		return "", errors.New("account name must be lowercase letters, digits or dashes")
	}
	dir := filepath.Join(a.state, "accounts", name)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", errors.New("no such account")
	}
	return dir, nil
}

// refreshStatus asks the official CLI who is signed in for one account. Only
// identity fields are kept; the launcher never reads credentials itself.
func (a *App) refreshStatus(name string) {
	dir, err := a.accountDir(name)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.claude, "auth", "status")
	cmd.Env = childEnv(dir)
	cmd.Dir = dir
	out, _ := cmd.Output()
	st := map[string]any{"checked": time.Now().Format(time.RFC3339)}
	var raw map[string]any
	if json.Unmarshal(out, &raw) == nil {
		for _, k := range []string{"loggedIn", "authMethod", "email", "orgName", "subscriptionType", "loginExpiresAt"} {
			if v, ok := raw[k]; ok {
				st[k] = v
			}
		}
	} else {
		st["error"] = "could not read `claude auth status`"
	}
	a.statusMu.Lock()
	a.status[name] = st
	a.statusAt[name] = time.Now()
	a.statusMu.Unlock()
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		Name   string         `json:"name"`
		Status map[string]any `json:"status"`
	}
	var accts []acct
	for _, n := range a.accounts() {
		a.statusMu.Lock()
		st, ok := a.status[n]
		stale := ok && time.Since(a.statusAt[n]) > 2*time.Minute
		if stale {
			a.statusAt[n] = time.Now() // one background re-check at a time
		}
		a.statusMu.Unlock()
		if stale {
			go a.refreshStatus(n)
		}
		if !ok {
			a.refreshStatus(n)
			a.statusMu.Lock()
			st = a.status[n]
			a.statusMu.Unlock()
		}
		accts = append(accts, acct{n, st})
	}
	writeJSON(w, map[string]any{
		"root": tilde(a.root), "accounts": accts, "projects": listProjects(a.root), "runs": a.mgr.List(),
		"claude": a.claudeVersion(),
	})
}

func (a *App) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	if !accountNameRe.MatchString(in.Name) {
		http.Error(w, "account name must be lowercase letters, digits or dashes", http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(filepath.Join(a.state, "accounts", in.Name), 0o700); err != nil {
		http.Error(w, "could not create account: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.refreshStatus(in.Name)
	writeJSON(w, map[string]any{"ok": true})
}

func (a *App) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if _, err := a.accountDir(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	a.refreshStatus(r.PathValue("name"))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAccountLogin runs Anthropic's own `claude auth login` under a
// terminal the browser can see. The sign-in itself happens on Anthropic's page.
func (a *App) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dir, err := a.accountDir(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	run := &Run{Kind: "login", Account: name, Name: "Sign in: " + name, dir: dir}
	err = a.mgr.Start(run, []string{a.claude, "auth", "login", "--claudeai"}, childEnv(dir),
		func(*Run) { a.refreshStatus(name) })
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, run.View())
}

func (a *App) handleAddProject(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	rel, err := createProject(a.root, in.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"project": rel})
}

func (a *App) run(w http.ResponseWriter, r *http.Request) *Run {
	run := a.mgr.Get(r.PathValue("id"))
	if run == nil {
		http.Error(w, "no such process", http.StatusNotFound)
	}
	return run
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if run := a.run(w, r); run != nil {
		if run.Kind == "server" {
			a.remember(wanted{Account: run.Account, Project: run.Project}, false)
		}
		run.Stop()
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (a *App) handleResize(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	var in struct{ Cols, Rows int }
	if !readJSON(w, r, &in) {
		return
	}
	if in.Cols < 20 || in.Cols > 300 || in.Rows < 5 || in.Rows > 100 {
		http.Error(w, "size out of range", http.StatusBadRequest)
		return
	}
	if err := run.Resize(in.Cols, in.Rows); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleStream sends the process's terminal output as server-sent events,
// base64 encoded: first everything buffered, then live output.
func (a *App) handleStream(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	snap, ch := run.Subscribe()
	send := func(b []byte) {
		fmt.Fprintf(w, "data: %s\n\n", base64.StdEncoding.EncodeToString(b))
		fl.Flush()
	}
	if len(snap) > 0 {
		send(snap)
	}
	if ch != nil {
		defer run.Unsubscribe(ch)
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
	loop:
		for {
			select {
			case b, open := <-ch:
				if !open {
					break loop
				}
				send(b)
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
	fmt.Fprint(w, "event: end\ndata: end\n\n")
	fl.Flush()
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// tilde shortens a path under the home directory for display.
func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
