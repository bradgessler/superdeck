package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

//go:embed ui/static ui/templates
var uiFS embed.FS

type App struct {
	roots         []string // real paths of the folders Claude may be launched in
	state, claude string
	hosts         map[string]bool
	mgr           *Manager

	statusMu sync.Mutex
	status   map[string]map[string]any
	statusAt map[string]time.Time

	verMu   sync.Mutex
	ver     string
	verAt   time.Time
	closing atomic.Bool
	wantMu  sync.Mutex
	started time.Time
}

func newApp(roots []string, state, claude string) *App {
	return &App{roots: roots, state: state, claude: claude, mgr: NewManager(), started: time.Now(),
		status: map[string]map[string]any{}, statusAt: map[string]time.Time{}, hosts: map[string]bool{}}
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7433", "address to listen on (use the Mac's LAN address to reach it over a VPN)")
	state := flag.String("state", filepath.Join(home, ".superdeck"), "directory for per-account Claude config")
	claude := flag.String("claude", "", "path to the claude binary (default: found on PATH)")
	extra := flag.String("hosts", "", "extra Host header values to accept, comma separated")
	agent := flag.Bool("launch-agent", false, "print a launchd agent that runs Superdeck at login with these settings, then exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: superdeck [flags] FOLDER...\n\n"+
			"Each FOLDER is a place Superdeck may launch Claude in, along with the\n"+
			"folders inside it. Glob patterns work too, for example ~/Projects/acme-*.\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	roots, err := expandRoots(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "superdeck: %v\n\n", err)
		flag.Usage()
		os.Exit(2)
	}

	bin := *claude
	if bin == "" {
		bin = "claude"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		log.Fatalf("claude binary not found: %v", err)
	}
	if p, err = filepath.Abs(p); err != nil {
		log.Fatal(err)
	}
	a := newApp(roots, *state, p)
	if *agent {
		self, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		args := []string{"-addr", *addr, "-state", a.state, "-claude", a.claude}
		if *extra != "" {
			args = append(args, "-hosts", *extra)
		}
		args = append(args, a.roots...)
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

	rotateLog(filepath.Join(a.state, "superdeck.log"))
	srv := &http.Server{Addr: *addr, Handler: a.routes(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
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
	log.Printf("superdeck on http://%s  claude=%s", *addr, a.claude)
	for _, root := range a.roots {
		log.Printf("allowed folder: %s", root)
	}
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
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
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
	out := []string{}
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
		return "", errors.New("account names use lowercase letters, digits and dashes")
	}
	dir := filepath.Join(a.state, "accounts", name)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", errors.New("there is no account with that name")
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
	st := map[string]any{}
	var raw map[string]any
	if json.Unmarshal(out, &raw) == nil {
		for _, k := range []string{"loggedIn", "email", "orgName"} {
			if v, ok := raw[k]; ok {
				st[k] = v
			}
		}
	}
	a.statusMu.Lock()
	a.status[name] = st
	a.statusAt[name] = time.Now()
	a.statusMu.Unlock()
}

// accountStatus returns the cached sign-in state, checking once if there is
// none and re-checking in the background when it is a couple of minutes old.
func (a *App) accountStatus(name string) map[string]any {
	a.statusMu.Lock()
	st, ok := a.status[name]
	stale := ok && time.Since(a.statusAt[name]) > 2*time.Minute
	if stale {
		a.statusAt[name] = time.Now()
	}
	a.statusMu.Unlock()
	if !ok {
		a.refreshStatus(name)
		a.statusMu.Lock()
		st = a.status[name]
		a.statusMu.Unlock()
	} else if stale {
		go a.refreshStatus(name)
	}
	return st
}

// tilde shortens a path under the home directory for display.
func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		return "~" + p[len(home):]
	}
	return p
}

// rotateLog keeps the log launchd appends to from growing without bound: once
// it passes 5 MB the old one is set aside and a new one starts.
func rotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		os.Rename(path, path+".1")
	}
}
