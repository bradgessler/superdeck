package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const host = "127.0.0.1:7433"

// ---- folders ----

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
}

func TestWorktreesAreNotListedAsProjects(t *testing.T) {
	root := t.TempDir()
	createProject(root, "app")
	// A worktree has a .git file pointing at the main checkout.
	os.MkdirAll(filepath.Join(root, "app-feature"), 0o755)
	os.WriteFile(filepath.Join(root, "app-feature", ".git"), []byte("gitdir: ../app/.git/worktrees/app-feature\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "plain"), 0o755)
	if got := listProjects(root); len(got) != 1 || got[0] != "app" {
		t.Errorf("listProjects = %v, want [app]", got)
	}
}

// ---- process environment ----

func TestChildEnvIsExplicit(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "leak")
	t.Setenv("ANTHROPIC_API_KEY", "leak")
	t.Setenv("CLAUDE_CONFIG_DIR", "/wrong")
	got := 0
	for _, kv := range childEnv("/accounts/a") {
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

// ---- a stand-in for the Claude CLI ----

// fakeClaude answers the version and status questions, runs a sign-in that
// waits for a code, and "serves" until killed. Behaviour switches on marker
// files in the account's config directory.
func fakeClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
C="$CLAUDE_CONFIG_DIR"
case "$1 $2" in
  "--version "*) echo "9.9.9 (Claude Code)" ;;
  "auth status")
    if [ -f "$C/in" ]; then echo '{"loggedIn":true,"email":"t@example.com"}'; else echo '{"loggedIn":false}'; exit 1; fi ;;
  "auth login")
    echo "If the browser didn't open, visit: https://claude.example/oauth?state=abc"
    printf "Paste code here if prompted > "; read code
    if [ "$code" = "good-code" ]; then touch "$C/in"; echo "Login successful."; else echo "Login failed: invalid code"; exit 1; fi ;;
  "remote-control "*)
    if [ ! -f "$C/in" ]; then echo "Error: You must be logged in to use Remote Control."; exit 1; fi
    if [ -f "$C/asks-trust" ]; then
      echo "Quick safety check: is this a project you created or one you trust?"
      printf "Trust %s? [y/N] " "$PWD"; read x
      if [ "$x" != "y" ]; then echo "Workspace not trusted"; exit 1; fi
    fi
    if [ -f "$C/asks-other" ]; then printf "Share anonymous usage data? (y/n) "; read x; echo "answered=$x"; fi
    echo "serving $*"; sleep 600 ;;
  "update "*) echo "Claude Code is up to date" ;;
  *) echo "unexpected: $*"; exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// testApp has one signed-in account "work", one signed-out "idle", and a
// project "app".
func testApp(t *testing.T) *App {
	t.Helper()
	a := newApp(t.TempDir(), t.TempDir(), fakeClaude(t))
	for _, n := range []string{"work", "idle"} {
		os.MkdirAll(filepath.Join(a.state, "accounts", n), 0o700)
	}
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "in"), nil, 0o600)
	if _, err := createProject(a.root, "app"); err != nil {
		t.Fatal(err)
	}
	if err := a.allowHosts(host, ""); err != nil {
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

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// ---- processes ----

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
	if !strings.Contains(r.Tail(), "got=yes") || r.View().ExitCode != 0 {
		t.Errorf("output %q exit %d", r.Tail(), r.View().ExitCode)
	}
	if err := r.Input("late"); err == nil {
		t.Error("input accepted after exit")
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

func TestScreenEscapesProcessOutput(t *testing.T) {
	m := NewManager()
	r := &Run{Kind: "server", Account: "a", dir: t.TempDir()}
	if err := m.Start(r, []string{"/bin/sh", "-c", `printf '\033[31m<script>alert(1)</script>\033[0m & "q"\n'`}, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.View().Done })
	s := string(r.Screen())
	if strings.Contains(s, "<script>") || !strings.Contains(s, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("screen not escaped: %q", s)
	}
	if !strings.Contains(s, `<span style="color:`) {
		t.Errorf("colour lost: %q", s)
	}
}

// ---- instances ----

func TestServerRestartsOnlyWhenItDiesOnItsOwn(t *testing.T) {
	minLife, restartBase = 0, 10*time.Millisecond
	a := testApp(t)
	first, err := a.startServer("work", "app", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "9.9.9" {
		t.Errorf("version %q", first.Version)
	}
	waitFor(t, func() bool { return strings.Contains(first.Tail(), "serving") })
	if !strings.Contains(first.Tail(), "remote-control --name app (work) --spawn worktree --capacity 4") {
		t.Errorf("unexpected command line: %s", first.Tail())
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

func TestSignedOutInstanceIsNotRestarted(t *testing.T) {
	minLife, restartBase = 0, 10*time.Millisecond
	a := testApp(t)
	r, err := a.startServer("idle", "app", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !r.alive() })
	time.Sleep(150 * time.Millisecond)
	if n := len(servers(a)); n != 1 {
		t.Errorf("restart loop on a signed-out account: %d runs", n)
	}
	if h := r.View().Hint; h != "needs-login" {
		t.Errorf("hint %q, want needs-login", h)
	}
}

func TestRestartGivesUpEventually(t *testing.T) {
	minLife, restartBase = 0, time.Millisecond
	a := testApp(t)
	a.claude = "/usr/bin/true" // every launch exits at once
	a.ver = "1"
	a.verAt = time.Now()
	if _, err := a.startServer("work", "app", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(servers(a)) == maxRestarts+1 })
	time.Sleep(400 * time.Millisecond)
	if n := len(servers(a)); n != maxRestarts+1 {
		t.Errorf("%d launches, want it to stop at %d", n, maxRestarts+1)
	}
}

func TestStartServerRejectsBadInput(t *testing.T) {
	a := testApp(t)
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

func TestInstancesComeBackAfterSuperdeckRestarts(t *testing.T) {
	a := testApp(t)
	a.remember(wanted{"work", "app", "worktree", 4}, true)
	a.remember(wanted{"work", "../../etc", "same-dir", 1}, true) // a tampered entry
	a.remember(wanted{"gone", "app", "worktree", 4}, true)
	a.remember(wanted{"gone", "app", "", 0}, false) // stopped by a person

	next := newApp(a.root, a.state, a.claude)
	t.Cleanup(func() { next.closing.Store(true); next.mgr.StopAll() })
	next.restore()
	got := servers(next)
	if len(got) != 1 || got[0].Project != "app" || got[0].Account != "work" {
		t.Fatalf("restored %+v, want only work/app", got)
	}
}

// ---- accounts ----

func TestRenameKeepsTheLoginDirectory(t *testing.T) {
	a := testApp(t)
	if err := a.setName("work", "rocketship"); err != nil {
		t.Fatal(err)
	}
	if got := a.accountName("work"); got != "rocketship" {
		t.Errorf("name %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.state, "accounts", "work", "in")); err != nil {
		t.Error("the account's directory moved; its Keychain login would be lost")
	}
	if err := a.setName("idle", "rocketship"); err == nil {
		t.Error("two accounts were given the same name")
	}
	for _, bad := range []string{"", "Has Space", "../x", "-flag"} {
		if err := a.setName("work", bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	// The old name is free again, and gets a directory of its own.
	id, err := a.addAccount("work")
	if err != nil || id == "work" {
		t.Errorf("addAccount(work) = %q, %v; want a fresh directory", id, err)
	}
}

func TestSignInWithCode(t *testing.T) {
	a := testApp(t)
	run, err := a.startLogin("idle")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return run.View().URL != "" })
	if u := run.View().URL; u != "https://claude.example/oauth?state=abc" {
		t.Errorf("sign-in URL %q", u)
	}
	if _, err := a.startLogin("idle"); err == nil {
		t.Error("a second sign-in for the same account was allowed")
	}
	for _, bad := range []string{"", "abc", "has space", "x; rm -rf ~", "line1\nline2", "$(id)"} {
		if err := submitCode(run, bad); err == nil {
			t.Errorf("code %q accepted", bad)
		}
	}
	if err := submitCode(run, "  good-code\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !run.alive() })
	waitFor(t, func() bool { in, _ := a.accountStatus("idle")["loggedIn"].(bool); return in })
}

func TestWrongCodeLeavesAccountSignedOut(t *testing.T) {
	a := testApp(t)
	run, _ := a.startLogin("idle")
	waitFor(t, func() bool { return run.View().URL != "" })
	submitCode(run, "wrong-code")
	waitFor(t, func() bool { return !run.alive() })
	if v := a.instance(run.View()); !v.Failed {
		t.Errorf("failed sign-in shown as %q", v.State)
	}
	a.refreshStatus("idle")
	if in, _ := a.accountStatus("idle")["loggedIn"].(bool); in {
		t.Error("account signed in after a wrong code")
	}
}

func TestKeysAreLimited(t *testing.T) {
	m := NewManager()
	r := &Run{Kind: "server", Account: "a", dir: t.TempDir()}
	if err := m.Start(r, []string{"/bin/cat"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	for _, ok := range []string{"enter", "y", "n", "1", "up", "esc"} {
		if err := pressKey(r, ok); err != nil {
			t.Errorf("key %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "rm -rf ~", "\r", "a", "enter\r"} {
		if err := pressKey(r, bad); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
	if err := submitCode(r, "abc123"); err == nil {
		t.Error("a code was accepted by something that is not a sign-in")
	}
}

// ---- web ----

func do(a *App, method, path string, form url.Values, origin string) *httptest.ResponseRecorder {
	var body *strings.Reader = strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.Host = host
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

const same = "http://" + host

func TestGuard(t *testing.T) {
	a := testApp(t)
	name := url.Values{"name": {"x"}}
	if w := do(a, "POST", "/accounts", name, "http://evil.example"); w.Code != 403 {
		t.Errorf("cross-origin post: %d", w.Code)
	}
	if w := do(a, "POST", "/accounts", name, ""); w.Code != 403 {
		t.Errorf("post without origin: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example:7433"
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("unknown host: %d", w.Code)
	}
}

func TestAllowHostsRefusesWildcardWithoutNames(t *testing.T) {
	if err := newApp("", "", "").allowHosts("0.0.0.0:7433", ""); err == nil {
		t.Error("wildcard bind accepted without -hosts")
	}
}

func TestEveryPageRendersEvenWhenEmpty(t *testing.T) {
	empty := newApp(t.TempDir(), t.TempDir(), fakeClaude(t))
	os.MkdirAll(filepath.Join(empty.state, "accounts"), 0o700)
	empty.allowHosts(host, "")
	for _, a := range []*App{empty, testApp(t)} {
		for _, p := range []string{"/", "/instances", "/new", "/new/folders?q=a", "/folders/new", "/accounts", "/accounts/new"} {
			w := do(a, "GET", p, nil, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "<") || strings.Contains(w.Body.String(), "<no value>") {
				t.Errorf("GET %s: %d %q", p, w.Code, w.Body.String())
			}
		}
	}
	if w := do(empty, "GET", "/instances/nope", nil, ""); w.Code != 404 {
		t.Errorf("missing instance: %d", w.Code)
	}
}

func TestLaunchFlowInTheBrowser(t *testing.T) {
	a := testApp(t)
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "asks-trust"), nil, 0o600)
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "asks-other"), nil, 0o600)

	// Launching under a signed-out account sends you to Accounts with a reason.
	w := do(a, "POST", "/instances", url.Values{"account": {"idle"}, "folder": {"app"}}, same)
	if loc := w.Header().Get("Location"); w.Code != 303 || !strings.HasPrefix(loc, "/accounts?error=") {
		t.Fatalf("signed-out launch: %d %q", w.Code, loc)
	}
	if n := len(servers(a)); n != 0 {
		t.Fatalf("%d instances started for a signed-out account", n)
	}

	w = do(a, "POST", "/instances", url.Values{"account": {"work"}, "folder": {"app"}}, same)
	loc := w.Header().Get("Location")
	if w.Code != 303 || !strings.HasPrefix(loc, "/instances/") {
		t.Fatalf("launch: %d %q %s", w.Code, loc, w.Body)
	}
	run := a.mgr.Get(strings.TrimPrefix(loc, "/instances/"))
	waitFor(t, func() bool { return run.View().Hint == "needs-trust" })
	page := do(a, "GET", loc, nil, "").Body.String()
	if !strings.Contains(page, "Trust this folder?") {
		t.Errorf("trust question missing from page")
	}

	if w := do(a, "POST", loc+"/key", url.Values{"key": {"rm -rf"}}, same); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Errorf("bad key not refused: %d %q", w.Code, w.Header().Get("Location"))
	}
	do(a, "POST", loc+"/answer", url.Values{"answer": {"yes"}}, same)
	// A question this program has never seen is shown in Claude's own words.
	waitFor(t, func() bool { return run.View().Hint == "needs-answer" })
	if q := do(a, "GET", loc+"/live", nil, "").Body.String(); !strings.Contains(q, "Share anonymous usage data?") {
		t.Errorf("unknown question not shown: %s", q)
	}
	do(a, "POST", loc+"/answer", url.Values{"answer": {"no"}}, same)
	waitFor(t, func() bool { return strings.Contains(run.Tail(), "serving") })
	if !strings.Contains(run.Tail(), "answered=n") {
		t.Errorf("the no answer did not reach Claude: %q", run.Tail())
	}
	page = do(a, "GET", loc+"/live", nil, "").Body.String()
	if strings.Contains(page, "Trust this folder?") || !strings.Contains(page, "running") {
		t.Errorf("page did not move on after trust: %s", page)
	}

	// A second launch of the same thing is refused, not duplicated.
	w = do(a, "POST", "/instances", url.Values{"account": {"work"}, "folder": {"app"}}, same)
	if !strings.Contains(w.Header().Get("Location"), "error=") || len(servers(a)) != 1 {
		t.Errorf("duplicate launch: %q, %d instances", w.Header().Get("Location"), len(servers(a)))
	}

	do(a, "POST", loc+"/stop", url.Values{}, same)
	if run.alive() {
		t.Error("instance still alive after stop")
	}
	if len(a.loadWanted()) != 0 {
		t.Error("a stopped instance is still remembered for restore")
	}
}

func TestLaunchAgentEscapesArguments(t *testing.T) {
	out := launchAgent("/bin/superdeck", []string{"-root", "/Users/a&b/<Projects>"}, "/tmp/log", "/usr/bin")
	if !strings.Contains(out, "<string>/Users/a&amp;b/&lt;Projects&gt;</string>") || strings.Contains(out, "a&b") {
		t.Errorf("arguments not escaped:\n%s", out)
	}
}

func TestDecliningTrustStopsCleanlyWithoutARestartLoop(t *testing.T) {
	minLife, restartBase = 0, 10*time.Millisecond
	a := testApp(t)
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "asks-trust"), nil, 0o600)
	run, err := a.startServer("work", "app", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return run.View().Hint == "needs-trust" })
	if err := answer(run, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !run.alive() })
	time.Sleep(150 * time.Millisecond)
	if n := len(servers(a)); n != 1 {
		t.Errorf("declining trust led to %d launches", n)
	}
}

func TestAbandonedSignInExpires(t *testing.T) {
	loginTimeout = 50 * time.Millisecond
	t.Cleanup(func() { loginTimeout = 15 * time.Minute })
	a := testApp(t)
	run, err := a.startLogin("idle")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !run.alive() })
	if v := a.instance(run.View()); v.State != "cancelled" {
		t.Errorf("expired sign-in shown as %q", v.State)
	}
	if _, err := a.startLogin("idle"); err != nil {
		t.Errorf("could not start a fresh sign-in afterwards: %v", err)
	}
}
