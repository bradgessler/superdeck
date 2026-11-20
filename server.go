package main

import (
	"context"
	"crypto/rand"
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
	"runtime/pprof"
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
	port          string          // the port being listened on
	anyAddr       bool            // listening on every interface
	selfNames     map[string]bool // this Mac's own names
	mgr           *Manager

	statusMu sync.Mutex
	status   map[string]map[string]any
	statusAt map[string]time.Time

	verMu   sync.Mutex
	ver     string
	verAt   time.Time
	closing atomic.Bool
	started time.Time

	storeOnce sync.Once
	store     *Store

	checkMu  sync.Mutex
	checking map[string]bool // accounts with a status check in flight
	checkSem chan struct{}   // how many `claude auth status` may run at once

	acctMu sync.Mutex // adding and removing accounts
	opMu   sync.Mutex
	ops    map[string]*sync.Mutex // one per folder; see folderLock

	flash []byte // signs the one-line messages carried across a redirect

	rateMu sync.Mutex
	rateAt time.Time
	tokens float64

	folderMu sync.Mutex
	folderAt time.Time
	folderLs []folder
}

func newApp(roots []string, state, claude string) *App {
	a := &App{roots: roots, state: state, claude: claude, started: time.Now(),
		status: map[string]map[string]any{}, statusAt: map[string]time.Time{}, hosts: map[string]bool{},
		checking: map[string]bool{}, checkSem: make(chan struct{}, 2), ops: map[string]*sync.Mutex{}}
	a.tokens, a.rateAt = 12, time.Now()
	a.flash = make([]byte, 32)
	rand.Read(a.flash)
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	a.mgr = NewManager(self, state)
	// Every process is recorded, so a restarted Superdeck knows what is being held.
	a.mgr.onStart = func(r *Run) error { return a.data().SaveRun(r) }
	a.mgr.onStop = func(r *Run) { a.data().MarkStopped(r.ID) }
	a.mgr.onEnd = func(r *Run) { a.data().EndRun(r.ID, r.View().ExitCode) }
	a.mgr.onHolder = func(id string, hpid, pid int) { a.data().SetHolder(id, hpid, pid) }
	return a
}

func main() {
	home, _ := os.UserHomeDir()
	// Two other ways this program is run. `hold` is Superdeck starting a
	// holder for one process; `attach` connects your terminal to one.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hold":
			os.Exit(holdMain(os.Args[2:]))
		case "attach":
			os.Exit(attachMain(filepath.Join(home, ".superdeck"), os.Args[2:]))
		default: // commands to the running Superdeck; see control.go
			if _, ok := controlCommands[os.Args[1]]; ok {
				os.Exit(controlMain(filepath.Join(home, ".superdeck"), os.Args[1], os.Args[2:], os.Stdin, os.Stdout))
			}
		}
	}
	addr := flag.String("addr", "127.0.0.1:7433", "address to listen on (use the Mac's LAN address to reach it over a VPN)")
	state := flag.String("state", filepath.Join(home, ".superdeck"), "directory for per-account Claude config")
	claude := flag.String("claude", "", "path to the claude binary (default: found on PATH)")
	extra := flag.String("hosts", "", "extra Host header values to accept, comma separated")
	agent := flag.Bool("launch-agent", false, "print a launchd agent that runs Superdeck at login with these settings, then exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: superdeck [flags] FOLDER...\n       superdeck attach [ID]\n       superdeck COMMAND ...   to a Superdeck that is running:\n")
		var lines []string
		for _, l := range controlCommands {
			lines = append(lines, "         "+l)
		}
		sort.Strings(lines)
		fmt.Fprintf(flag.CommandLine.Output(), strings.Join(lines, "\n")+"\n\n"+
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
	// Everything Superdeck writes is for this user only.
	syscall.Umask(0o077)
	if err := os.MkdirAll(filepath.Join(a.state, "accounts"), 0o700); err != nil {
		log.Fatal(err)
	}
	os.Chmod(a.state, 0o700)
	// Two Superdecks sharing one state directory would each restart and
	// duplicate the other's instances. Only one may run.
	lock, err := os.OpenFile(filepath.Join(a.state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("another Superdeck is already running with %s", a.state)
	}
	defer lock.Close()
	// Open the data, upgrading its schema if this is a newer Superdeck.
	// Nothing is launched and nothing listens until that has succeeded.
	if err := a.openData(); err != nil {
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
		// Claude processes are kept alive by their holders and carry on.
		// Superdeck picks them up again when it next starts.
		log.Print("superdeck stopping; instances keep running")
		a.closing.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("superdeck on http://%s  claude=%s", *addr, a.claude)
	for _, root := range a.roots {
		log.Printf("allowed folder: %s", root)
	}
	log.Print("no sign-in yet: anyone who can reach this address can use it")
	// Claim the address before touching any process: if it is taken, this
	// Superdeck must leave without having launched anything.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	// Without knowing what is already running, anything launched could be a
	// duplicate. Stop here; launchd will try again.
	if err := a.adopt(); err != nil {
		log.Fatal(err)
	}
	a.serveControl()
	go a.restore()
	for _, f := range []string{"superdeck.log", "holders.log"} {
		os.Chmod(filepath.Join(a.state, f), 0o600)
	}
	go a.watchdog(*addr)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
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
	a.port = port
	ip := net.ParseIP(host)
	switch {
	case host == "" || (ip != nil && ip.IsUnspecified()):
		if extra == "" {
			return errors.New("listening on every interface needs -hosts with the names you will browse to")
		}
		a.anyAddr = true
		a.selfNames = map[string]bool{"localhost": true}
		if name, err := os.Hostname(); err == nil && name != "" {
			name = strings.ToLower(strings.TrimSuffix(name, "."))
			a.selfNames[name] = true
			a.selfNames[strings.TrimSuffix(name, ".local")+".local"] = true
		}
	case ip != nil && ip.IsLoopback():
		a.hosts[net.JoinHostPort("localhost", port)] = true
		a.hosts[addr] = true
	default:
		a.hosts[addr] = true
	}
	for _, h := range strings.Split(extra, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			a.hosts[h] = true
		}
	}
	return nil
}

// hostOK reports whether a request's Host header is one this server answers
// to. The names given at startup always are. When listening on every
// interface, so are any numeric address and this Mac's own name, on the right
// port: the Mac's address on the network can change (a new DHCP lease, a
// different network), and the page must not refuse its owner over that while
// they are away. Neither opens the door the list exists to close. DNS
// rebinding works by pointing a name the attacker owns at this Mac; a numeric
// address, or the Mac's own name, is not a name an attacker controls.
func (a *App) hostOK(h string) bool {
	h = strings.ToLower(h)
	if a.hosts[h] {
		return true
	}
	if !a.anyAddr {
		return false
	}
	host, port, err := net.SplitHostPort(h)
	if err != nil || port != a.port {
		return false
	}
	return net.ParseIP(host) != nil || a.selfNames[host]
}

// watchdog restarts Superdeck if it stops answering. launchd brings back a
// Superdeck that crashed, but cannot see one that is running and stuck, and
// a stuck one cannot be fixed from a phone. Restarting is safe by design:
// instances are kept alive by their holders and picked up again.
//
// It asks for a page the way a browser would, over the network socket, so it
// covers the listener, the request guard and the locks every page needs. It
// takes several failures in a row, over a couple of minutes, to act.
func (a *App) watchdog(addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	at := net.JoinHostPort(host, port)
	client := &http.Client{Timeout: watchdogTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	fails := 0
	for {
		time.Sleep(watchdogEvery)
		if a.closing.Load() {
			return
		}
		resp, err := client.Get("http://" + at + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fails = 0
				continue
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if a.closing.Load() {
			return
		}
		fails++
		log.Printf("health check failed (%d of %d): %v", fails, watchdogFails, err)
		if fails >= watchdogFails {
			log.Print("superdeck has stopped answering; restarting it. Instances keep running. What it was doing:")
			pprof.Lookup("goroutine").WriteTo(log.Writer(), 1)
			watchdogExit()
			return
		}
	}
}

var (
	watchdogEvery   = 30 * time.Second
	watchdogTimeout = 15 * time.Second
	watchdogFails   = 4
	watchdogExit    = func() { os.Exit(3) }
)

// health answers the watchdog. It touches what every page touches, so that
// if those are stuck, so is this. A database error is not a failure here:
// restarting does not mend a full disk, and would take the page away too.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	a.mgr.List()
	a.data().Alive()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// guard applies a Host allow-list and a same-origin check on every
// state-changing request, so a web page elsewhere cannot drive this server
// through your browser. There is no sign-in yet.
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r.Host) {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			// With no sign-in, this log is the only record of who did what.
			from, _, _ := net.SplitHostPort(r.RemoteAddr)
			// Quoted, so nothing in an address can pass for a line of its own.
			log.Printf("%s %q from %s", r.Method, r.URL.EscapedPath(), from)
		} else if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			// Another site has no business even reading these pages, and
			// should not be able to make this Mac do work by embedding them.
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Query().Get("v") == assetVersion {
			// Stylesheet and script are addressed by a fingerprint of their
			// contents, so they can be kept until they change.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}

// openData opens Superdeck's database, applying schema migrations, and
// brings in anything saved by versions that kept plain files.
func (a *App) openData() error {
	path := filepath.Join(a.state, "superdeck.db")
	st, err := openStore(path)
	if err != nil {
		return err
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		os.Chmod(f, 0o600) // it may predate the stricter permissions
	}
	a.store = st
	a.importLegacy()
	return nil
}

// data returns the store, opening it on first use. main opens it explicitly
// so a failed upgrade stops startup with a clear message.
func (a *App) data() *Store {
	a.storeOnce.Do(func() {
		if a.store == nil {
			if err := a.openData(); err != nil {
				panic(err)
			}
		}
	})
	return a.store
}

// accountDirs lists the account config directories on disk.
func (a *App) accountDirs() []string {
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

// accountStatus returns what is known about an account's sign-in without
// waiting. If nothing is known yet, or it is a couple of minutes old, a check
// starts in the background; pages show "checking" until it lands. Loading a
// page never runs a command inline.
func (a *App) accountStatus(name string) map[string]any {
	a.statusMu.Lock()
	st, ok := a.status[name]
	stale := !ok || time.Since(a.statusAt[name]) > 2*time.Minute
	a.statusMu.Unlock()
	if stale {
		a.checkSoon(name)
	}
	if !ok {
		return map[string]any{"checking": true}
	}
	return st
}

// accountStatusNow is for decisions that must not be made on a guess, such as
// launching: it waits for a check if nothing is known yet.
func (a *App) accountStatusNow(name string) map[string]any {
	a.statusMu.Lock()
	st, ok := a.status[name]
	a.statusMu.Unlock()
	if ok {
		return st
	}
	a.refreshStatus(name)
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	return a.status[name]
}

// checkSoon starts a background status check, at most one per account and a
// couple at a time overall.
func (a *App) checkSoon(name string) {
	a.checkMu.Lock()
	if a.checking[name] {
		a.checkMu.Unlock()
		return
	}
	a.checking[name] = true
	a.checkMu.Unlock()
	go func() {
		a.checkSem <- struct{}{}
		a.refreshStatus(name)
		<-a.checkSem
		a.checkMu.Lock()
		delete(a.checking, name)
		a.checkMu.Unlock()
	}()
}

// folderList is every folder an instance may be launched in. Walking the
// allowed folders is not free, so the result is kept for a few seconds.
func (a *App) folderList() []folder {
	a.folderMu.Lock()
	defer a.folderMu.Unlock()
	switch {
	case a.folderLs == nil:
		a.folderLs, a.folderAt = listProjects(a.roots), time.Now()
	case time.Since(a.folderAt) > 5*time.Second:
		a.folderAt = time.Now() // one walk at a time
		go func() {
			ls := listProjects(a.roots)
			a.folderMu.Lock()
			a.folderLs = ls
			a.folderMu.Unlock()
		}()
	}
	return a.folderLs
}

// offered reports whether dir is one of the folders Superdeck lists. Being
// somewhere inside an allowed folder is not enough: hidden directories and
// the insides of repositories are not places to run an instance.
func (a *App) offered(dir string) bool {
	for pass := 0; pass < 2; pass++ {
		for _, f := range a.folderList() {
			if f.Path == dir {
				return true
			}
		}
		a.folderMu.Lock()
		a.folderLs = nil // it may have been created a moment ago; look again now
		a.folderMu.Unlock()
	}
	return false
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
