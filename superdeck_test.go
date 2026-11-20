package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveProjectStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(filepath.Join(root, "org", "app"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "escape"))
	os.WriteFile(filepath.Join(root, "file"), nil, 0o644)

	if _, err := resolveProject(root, "org/app"); err != nil {
		t.Fatalf("valid project refused: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "../outside", "org/../../outside", "escape", outside, "file", "missing"} {
		if p, err := resolveProject(root, bad); err == nil {
			t.Errorf("resolveProject(%q) = %q, want error", bad, p)
		}
	}
}

func TestCreateProject(t *testing.T) {
	root := t.TempDir()
	if _, err := createProject(root, "new-app"); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(filepath.Join(root, "new-app")) {
		t.Error("new project is not a Git repository")
	}
	for _, bad := range []string{"new-app", "../x", "a/b", ".hidden", "", "-rf"} {
		if _, err := createProject(root, bad); err == nil {
			t.Errorf("createProject(%q) succeeded, want error", bad)
		}
	}
	if got := listProjects(root); len(got) != 1 || got[0] != "new-app" {
		t.Errorf("listProjects = %v", got)
	}
}

func TestChildEnvIsExplicit(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "leak")
	t.Setenv("ANTHROPIC_API_KEY", "leak")
	t.Setenv("CLAUDE_CONFIG_DIR", "/wrong")
	env := childEnv("/accounts/a")
	got := 0
	for _, kv := range env {
		if strings.Contains(kv, "leak") || kv == "CLAUDE_CONFIG_DIR=/wrong" {
			t.Errorf("leaked %q", kv)
		}
		if kv == "CLAUDE_CONFIG_DIR=/accounts/a" {
			got++
		}
	}
	if got != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR set %d times, want 1", got)
	}
}

func testApp(t *testing.T) *App {
	a := &App{root: t.TempDir(), state: t.TempDir(), mgr: NewManager(),
		status: map[string]map[string]any{}, statusAt: map[string]time.Time{}, hosts: map[string]bool{}}
	os.MkdirAll(filepath.Join(a.state, "accounts"), 0o700)
	if err := a.allowHosts("127.0.0.1:7433", ""); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGuard(t *testing.T) {
	h := testApp(t).routes()
	do := func(method, host, origin string) int {
		r := httptest.NewRequest(method, "/api/state", nil)
		if method == "POST" {
			r = httptest.NewRequest(method, "/api/accounts", strings.NewReader(`{"name":"x"}`))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name, method, host, origin string
		want                       int
	}{
		{"read", "GET", "127.0.0.1:7433", "", 200},
		{"same-origin post", "POST", "127.0.0.1:7433", "http://127.0.0.1:7433", 200},
		{"rebinding host", "GET", "evil.example:7433", "", 403},
		{"cross-origin post", "POST", "127.0.0.1:7433", "http://evil.example", 403},
		{"post without origin", "POST", "127.0.0.1:7433", "", 403},
	}
	for _, c := range cases {
		if got := do(c.method, c.host, c.origin); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAllowHostsRefusesWildcardWithoutNames(t *testing.T) {
	a := &App{hosts: map[string]bool{}}
	if err := a.allowHosts("0.0.0.0:7433", ""); err == nil {
		t.Error("wildcard bind accepted without -hosts")
	}
}

func TestRunLifecycle(t *testing.T) {
	m := NewManager()
	dir := t.TempDir()
	r := &Run{Kind: "server", Account: "a", dir: dir}
	if err := m.Start(r, []string{"/bin/sh", "-c", "echo Workspace not trusted; read x; echo got=$x"}, childEnv(dir), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(&Run{Kind: "server", Account: "a", dir: dir}, []string{"/bin/sh", "-c", "true"}, nil, nil); err == nil {
		t.Error("duplicate run for same account and directory was allowed")
	}
	waitFor(t, func() bool { return r.View().Hint == "needs-trust" })
	if err := r.Input("yes\r"); err != nil {
		t.Fatal(err)
	}
	if h := r.View().Hint; h != "" {
		t.Errorf("hint %q still showing after the prompt was answered", h)
	}
	waitFor(t, func() bool { return r.View().Done })
	snap, ch := r.Subscribe()
	if ch != nil || !strings.Contains(string(snap), "got=yes") {
		t.Errorf("output %q", snap)
	}
	if r.View().ExitCode != 0 {
		t.Errorf("exit code %d", r.View().ExitCode)
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	m := NewManager()
	r := &Run{Kind: "server", Account: "a", dir: t.TempDir()}
	if err := m.Start(r, []string{"/bin/sh", "-c", "sleep 60 & wait"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	r.Stop()
	waitFor(t, func() bool { return r.View().Done })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// fakeClaude stands in for the real CLI: it answers the version and status
// questions and "serves" until killed.
func fakeClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
case "$1 $2" in
  "--version "*) echo "9.9.9 (Claude Code)" ;;
  "auth status") echo '{"loggedIn":true,"email":"t@example.com"}' ;;
  "remote-control "*) echo "serving $*"; sleep 600 ;;
  *) echo "unexpected: $*"; exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverApp(t *testing.T) *App {
	a := testApp(t)
	a.claude = fakeClaude(t)
	os.MkdirAll(filepath.Join(a.state, "accounts", "work"), 0o700)
	if _, err := createProject(a.root, "app"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.closing.Store(true); a.mgr.StopAll() })
	return a
}

func servers(a *App) []RunView {
	var out []RunView
	for _, v := range a.mgr.List() {
		if v.Kind == "server" {
			out = append(out, v)
		}
	}
	return out
}

func TestServerRestartsOnlyWhenItDiesOnItsOwn(t *testing.T) {
	minLife, restartBase = 0, 10*time.Millisecond
	a := serverApp(t)
	first, err := a.startServer("work", "app", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "9.9.9" {
		t.Errorf("version %q", first.Version)
	}
	waitFor(t, func() bool { snap, _ := first.Subscribe(); return strings.Contains(string(snap), "serving") })
	snap, _ := first.Subscribe()
	if !strings.Contains(string(snap), "remote-control --name app (work) --spawn worktree --capacity 4") {
		t.Errorf("unexpected command line: %s", snap)
	}

	syscall.Kill(-first.cmd.Process.Pid, syscall.SIGKILL) // dies on its own
	waitFor(t, func() bool { return len(servers(a)) == 2 })
	second := a.mgr.Get(servers(a)[0].ID)
	if second.Restarts != 1 || !second.alive() {
		t.Fatalf("restart: restarts=%d alive=%v", second.Restarts, second.alive())
	}

	second.Stop() // a person stopped it
	waitFor(t, func() bool { return !second.alive() })
	time.Sleep(150 * time.Millisecond)
	if n := len(servers(a)); n != 2 {
		t.Errorf("a stopped instance was restarted: %d runs", n)
	}
}

func TestStartServerRejectsBadInput(t *testing.T) {
	a := serverApp(t)
	os.MkdirAll(filepath.Join(a.root, "plain"), 0o755)
	for _, c := range []struct{ account, project, spawn string }{
		{"nope", "app", ""}, {"../work", "app", ""}, {"work", "../app", ""}, {"work", "/etc", "same-dir"},
		{"work", "app", "session"}, {"work", "app", "worktree --dangerously-skip-permissions"}, {"work", "plain", "worktree"},
	} {
		if _, err := a.startServer(c.account, c.project, c.spawn, 0, 0); err == nil {
			t.Errorf("startServer(%q, %q, %q) succeeded", c.account, c.project, c.spawn)
		}
	}
	if _, err := a.startServer("work", "app", "", 99, 0); err == nil {
		t.Error("capacity 99 accepted")
	}
}

func TestInputIsLimitedToKeysAndCodes(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	start := func(kind string) *Run {
		r := &Run{Kind: kind, Account: kind, dir: t.TempDir()}
		if err := a.mgr.Start(r, []string{"/bin/cat"}, nil, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Stop)
		return r
	}
	server, login := start("server"), start("login")
	post := func(r *Run, body string) int {
		req := httptest.NewRequest("POST", "/api/runs/"+r.ID+"/input", strings.NewReader(body))
		req.Host = "127.0.0.1:7433"
		req.Header.Set("Origin", "http://127.0.0.1:7433")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	cases := []struct {
		run  *Run
		body string
		want int
	}{
		{server, `{"key":"enter"}`, 200},
		{server, `{"key":"y"}`, 200},
		{server, `{"key":"rm -rf ~"}`, 400},
		{server, `{"data":"anything\r"}`, 400},
		{server, `{"code":"abc123"}`, 400}, // codes only go to a sign-in
		{login, `{"code":"abc123#state-XYZ"}`, 200},
		{login, `{"code":"abc; rm -rf ~"}`, 400},
		{login, `{"code":"line1\nline2"}`, 400},
	}
	for _, c := range cases {
		if got := post(c.run, c.body); got != c.want {
			t.Errorf("%s input %s: status %d, want %d", c.run.Kind, c.body, got, c.want)
		}
	}
}

func TestInstancesComeBackAfterSuperdeckRestarts(t *testing.T) {
	a := serverApp(t)
	a.remember(wanted{"work", "app", "worktree", 4}, true)
	a.remember(wanted{"work", "../../etc", "same-dir", 1}, true) // a tampered entry
	a.remember(wanted{"gone", "app", "worktree", 4}, true)
	a.remember(wanted{"gone", "app", "", 0}, false) // stopped by a person

	next := &App{root: a.root, state: a.state, claude: a.claude, mgr: NewManager(),
		status: map[string]map[string]any{}, statusAt: map[string]time.Time{}, hosts: map[string]bool{}}
	t.Cleanup(func() { next.closing.Store(true); next.mgr.StopAll() })
	next.restore()
	got := servers(next)
	if len(got) != 1 || got[0].Project != "app" || got[0].Account != "work" {
		t.Fatalf("restored %+v, want only work/app", got)
	}
}

func TestLaunchAgentEscapesArguments(t *testing.T) {
	out := launchAgent("/bin/superdeck", []string{"-root", "/Users/a&b/<Projects>"}, "/tmp/log", "/usr/bin")
	if !strings.Contains(out, "<string>/Users/a&amp;b/&lt;Projects&gt;</string>") || strings.Contains(out, "a&b") {
		t.Errorf("arguments not escaped:\n%s", out)
	}
}
