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

func TestResolveProjectStaysInsideAllowedFolders(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "root")
	other := filepath.Join(base, "other")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(filepath.Join(root, "org", "app"), 0o755)
	os.MkdirAll(filepath.Join(other, "tool"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "escape"))
	os.WriteFile(filepath.Join(root, "file"), nil, 0o644)
	roots := []string{root, other}

	for _, ok := range []string{root, filepath.Join(root, "org", "app"), filepath.Join(other, "tool"), root + "/org/../org/app"} {
		if _, err := resolveProject(roots, ok); err != nil {
			t.Errorf("resolveProject(%q) refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "org/app", outside, base, "/", "/etc",
		filepath.Join(root, "..", "outside"), filepath.Join(root, "escape"), filepath.Join(root, "file"),
		filepath.Join(root, "missing"), root + "-sibling"} {
		if p, err := resolveProject(roots, bad); err == nil {
			t.Errorf("resolveProject(%q) = %q, want error", bad, p)
		}
	}
}

func TestExpandRootsFromCommandLine(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range []string{"acme-api", "acme-web", "other"} {
		os.MkdirAll(filepath.Join(base, n), 0o755)
	}
	os.WriteFile(filepath.Join(base, "acme-notes.txt"), nil, 0o644)

	got, err := expandRoots([]string{filepath.Join(base, "acme-*"), filepath.Join(base, "other"), filepath.Join(base, "other")})
	want := []string{filepath.Join(base, "acme-api"), filepath.Join(base, "acme-web"), filepath.Join(base, "other")}
	if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expandRoots = %v, %v; want %v", got, err, want)
	}
	for _, bad := range [][]string{nil, {"/"}, {filepath.Join(base, "missing")}, {filepath.Join(base, "acme-notes.txt")}, {filepath.Join(base, "nothing-*")}} {
		if got, err := expandRoots(bad); err == nil {
			t.Errorf("expandRoots(%v) = %v, want error", bad, got)
		}
	}
}

func TestCreateProject(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
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
	root, _ := filepath.EvalSymlinks(t.TempDir())
	createProject(root, "app")
	// A worktree has a .git file pointing at the main checkout.
	os.MkdirAll(filepath.Join(root, "app-feature"), 0o755)
	os.WriteFile(filepath.Join(root, "app-feature", ".git"), []byte("gitdir: ../app/.git/worktrees/app-feature\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "plain"), 0o755)
	// The repository is listed, and so is the allowed folder that holds it.
	// The worktree and the empty folder are not.
	got := listProjects([]string{root})
	if len(got) != 2 || got[0] != (folder{root, 1}) || got[1] != (folder{filepath.Join(root, "app"), 0}) {
		t.Errorf("listProjects = %v", got)
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
    echo "serving $*"
    echo "Continue coding in the Claude mobile app or https://claude.ai/code?environment=env_0123456789abcdefABCDEFxyz"
    echo "Or ask Claude to work in this folder"; sleep 600 ;;
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
	root, _ := filepath.EvalSymlinks(t.TempDir())
	a := newApp([]string{root}, t.TempDir(), fakeClaude(t))
	for _, n := range []string{"work", "idle"} {
		os.MkdirAll(filepath.Join(a.state, "accounts", n), 0o700)
	}
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "in"), nil, 0o600)
	if _, err := createProject(root, "app"); err != nil {
		t.Fatal(err)
	}
	if err := a.allowHosts(host, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.closing.Store(true); a.mgr.StopAll() })
	return a
}

// in returns the full path of a folder inside the test app's allowed folder.
func in(a *App, rel string) string { return filepath.Join(a.roots[0], rel) }

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
	first, err := a.startServer("work", in(a, "app"), "", 0, 0)
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
	r, err := a.startServer("idle", in(a, "app"), "", 0, 0)
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
	if _, err := a.startServer("work", in(a, "app"), "", 0, 0); err != nil {
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
	os.MkdirAll(in(a, "plain"), 0o755)
	for _, c := range []struct{ account, project, spawn string }{
		{"nope", in(a, "app"), ""}, {"../work", in(a, "app"), ""}, {"work", "app", ""}, {"work", in(a, "../app"), ""},
		{"work", "/etc", "same-dir"}, {"work", in(a, "app"), "session"},
		{"work", in(a, "app"), "worktree --dangerously-skip-permissions"}, {"work", in(a, "plain"), "worktree"},
	} {
		if _, err := a.startServer(c.account, c.project, c.spawn, 0, 0); err == nil {
			t.Errorf("startServer(%q, %q, %q) succeeded", c.account, c.project, c.spawn)
		}
	}
	if _, err := a.startServer("work", in(a, "app"), "", 99, 0); err == nil {
		t.Error("capacity 99 accepted")
	}
}

func TestInstancesComeBackAfterSuperdeckRestarts(t *testing.T) {
	a := testApp(t)
	a.remember(wanted{"work", in(a, "app"), "worktree", 4}, true)
	a.remember(wanted{"work", "/etc", "same-dir", 1}, true) // a tampered entry
	a.remember(wanted{"gone", in(a, "app"), "worktree", 4}, true)
	a.remember(wanted{"gone", in(a, "app"), "", 0}, false) // stopped by a person

	next := newApp(a.roots, a.state, a.claude)
	t.Cleanup(func() { next.closing.Store(true); next.mgr.StopAll() })
	next.restore()
	got := servers(next)
	if len(got) != 1 || got[0].Project != in(a, "app") || got[0].Account != "work" {
		t.Fatalf("restored %+v, want only work/app", got)
	}
}

func TestOldRelativeEntriesStillComeBack(t *testing.T) {
	a := testApp(t)
	a.remember(wanted{"work", "app", "worktree", 4}, true) // saved before folders were full paths
	a.restore()
	if got := servers(a); len(got) != 1 || got[0].Project != in(a, "app") {
		t.Fatalf("restored %+v", got)
	}
	if w := a.loadWanted(); len(w) != 1 || w[0].Project != in(a, "app") {
		t.Errorf("saved list not upgraded: %+v", w)
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
	if err := newApp(nil, "", "").allowHosts("0.0.0.0:7433", ""); err == nil {
		t.Error("wildcard bind accepted without -hosts")
	}
}

func TestEveryPageRendersEvenWhenEmpty(t *testing.T) {
	emptyRoot, _ := filepath.EvalSymlinks(t.TempDir())
	empty := newApp([]string{emptyRoot}, t.TempDir(), fakeClaude(t))
	os.MkdirAll(filepath.Join(empty.state, "accounts"), 0o700)
	empty.allowHosts(host, "")
	for _, a := range []*App{empty, testApp(t)} {
		for _, p := range []string{"/", "/instances", "/new", "/new/folders?q=a", "/folders/new", "/accounts", "/accounts/new"} {
			if a != empty && p == "/accounts" {
				for _, sub := range []string{"/accounts/work", "/accounts/idle"} {
					if w := do(a, "GET", sub, nil, ""); w.Code != 200 {
						t.Errorf("GET %s: %d", sub, w.Code)
					}
				}
			}
			w := do(a, "GET", p, nil, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "<") || strings.Contains(w.Body.String(), "<no value>") {
				t.Errorf("GET %s: %d %q", p, w.Code, w.Body.String())
			}
		}
	}
	if w := do(empty, "GET", "/accounts/nope", nil, ""); w.Code != 404 {
		t.Errorf("missing account: %d", w.Code)
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
	w := do(a, "POST", "/instances", url.Values{"account": {"idle"}, "folder": {in(a, "app")}}, same)
	if loc := w.Header().Get("Location"); w.Code != 303 || !strings.HasPrefix(loc, "/accounts?error=") {
		t.Fatalf("signed-out launch: %d %q", w.Code, loc)
	}
	if n := len(servers(a)); n != 0 {
		t.Fatalf("%d instances started for a signed-out account", n)
	}

	w = do(a, "POST", "/instances", url.Values{"account": {"work"}, "folder": {in(a, "app")}}, same)
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

	if w := do(a, "GET", loc+"/output", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Trust ") {
		t.Errorf("terminal screen: %d", w.Code)
	}
	if strings.Contains(page, "<details") || strings.Contains(page, "<pre") {
		t.Error("the instance screen should not embed the terminal")
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
	waitFor(t, func() bool { return run.View().Link != "" })
	page = do(a, "GET", loc+"/live", nil, "").Body.String()
	if !strings.Contains(page, `href="https://claude.ai/code?environment=env_0123456789abcdefABCDEFxyz"`) {
		t.Errorf("Open in Claude link missing or broken: %s", page)
	}
	if strings.Contains(page, "Trust this folder?") || !strings.Contains(page, "running") {
		t.Errorf("page did not move on after trust: %s", page)
	}

	// A second launch of the same thing is refused, not duplicated.
	w = do(a, "POST", "/instances", url.Values{"account": {"work"}, "folder": {in(a, "app")}}, same)
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
	run, err := a.startServer("work", in(a, "app"), "", 0, 0)
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

func TestGarbageOutputCannotCrashSuperdeck(t *testing.T) {
	m := NewManager()
	r := &Run{Kind: "server", Account: "a", dir: t.TempDir()}
	if err := m.Start(r, []string{"/bin/sh", "-c", "head -c 200000 /dev/urandom; printf '\\033[999999;999999H\\033[9999999A\\033[?1049h\\033[2J\\033]0;'; head -c 5000 /dev/urandom"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.View().Done })
	for _, size := range [][2]int{{1, 1}, {300, 100}, {46, 24}} {
		r.mu.Lock()
		r.done = false // let Resize through for the purpose of the test
		r.mu.Unlock()
		r.Resize(size[0], size[1])
		_ = r.Screen()
	}
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

func TestSignedOutSoonAfterStartupIsRetried(t *testing.T) {
	minLife, bootRetry = 0, 30*time.Millisecond
	t.Cleanup(func() { bootRetry = 30 * time.Second })
	a := testApp(t)
	// The account looks signed out at first, as when the Keychain is not yet
	// unlocked, and becomes readable a moment later.
	marker := filepath.Join(a.state, "accounts", "work", "in")
	os.Remove(marker)
	if _, err := a.startServer("work", in(a, "app"), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	os.WriteFile(marker, nil, 0o600)
	waitFor(t, func() bool {
		for _, v := range servers(a) {
			if !v.Done && strings.Contains(v.Tail(), "serving") {
				return true
			}
		}
		return false
	})
}

func TestRenameFromTheAccountScreen(t *testing.T) {
	a := testApp(t)
	w := do(a, "POST", "/accounts/work/rename", url.Values{"name": {"overton"}}, same)
	if w.Code != 303 || w.Header().Get("Location") != "/accounts/work" {
		t.Fatalf("rename: %d %q", w.Code, w.Header().Get("Location"))
	}
	if page := do(a, "GET", "/accounts/work", nil, "").Body.String(); !strings.Contains(page, `value="overton"`) {
		t.Error("new name not shown in the edit box")
	}
	w = do(a, "POST", "/accounts/work/rename", url.Values{"name": {"Bad Name"}}, same)
	if !strings.HasPrefix(w.Header().Get("Location"), "/accounts/work?error=") {
		t.Errorf("bad name: %q", w.Header().Get("Location"))
	}
}

func TestSignInScreens(t *testing.T) {
	a := testApp(t)
	w := do(a, "POST", "/accounts/idle/signin", url.Values{}, same)
	loc := w.Header().Get("Location")
	run := a.mgr.Get(strings.TrimPrefix(loc, "/instances/"))
	waitFor(t, func() bool { return run.View().URL != "" })
	page := do(a, "GET", loc, nil, "").Body.String()
	for _, want := range []string{"Sign in to idle", `href="https://claude.example/oauth?state=abc"`, `name="code"`, "Cancel"} {
		if !strings.Contains(page, want) {
			t.Errorf("sign-in screen is missing %q", want)
		}
	}
	// While you are typing, the watcher says nothing and redraws nothing.
	if w := do(a, "GET", loc+"/watch?url=1", nil, ""); w.Code != 204 || w.Header().Get("HX-Refresh") != "" || w.Header().Get("HX-Redirect") != "" {
		t.Errorf("watcher interrupted a waiting sign-in: %v", w.Header())
	}
	// A wrong code lands on a clear failure with a way to retry.
	do(a, "POST", loc+"/code", url.Values{"code": {"wrong-code"}}, same)
	waitFor(t, func() bool { return !run.alive() })
	page = do(a, "GET", loc, nil, "").Body.String()
	if !strings.Contains(page, "That didn") || !strings.Contains(page, "Try again") {
		t.Errorf("failure screen: %s", page)
	}
	// The right code goes straight to the account, now signed in.
	w = do(a, "POST", "/accounts/idle/signin", url.Values{}, same)
	loc = w.Header().Get("Location")
	run = a.mgr.Get(strings.TrimPrefix(loc, "/instances/"))
	waitFor(t, func() bool { return run.View().URL != "" })
	w = do(a, "POST", loc+"/code", url.Values{"code": {"good-code"}}, same)
	if got := w.Header().Get("Location"); got != "/accounts/idle" {
		t.Fatalf("after a good code: %q", got)
	}
	if page := do(a, "GET", "/accounts/idle", nil, "").Body.String(); !strings.Contains(page, "signed in") || !strings.Contains(page, "t@example.com") {
		t.Errorf("account not shown as signed in: %s", page)
	}
}

func TestWorkspaceFoldersCanBeLaunchedIn(t *testing.T) {
	a := testApp(t)
	// A folder that is not a repository itself but holds several.
	for _, n := range []string{"mono/api", "mono/web", "zeta-mono-tools"} {
		os.MkdirAll(in(a, n+"/.git"), 0o755)
	}

	rows, _, _ := a.folders("mono", "")
	if len(rows) < 3 || rows[0].Rel != in(a, "mono") || !strings.HasSuffix(rows[0].Note, "holds 2 repos") {
		t.Fatalf("typing the workspace name should find the workspace first: %+v", rows)
	}
	run, err := a.startServer("work", in(a, "mono"), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(run.Tail(), "serving") })
	if !strings.Contains(run.Tail(), "--spawn same-dir") {
		t.Errorf("a folder that is not a repository cannot use worktrees: %s", run.Tail())
	}
}

func TestNewFolderOnlyInsideAllowedFolders(t *testing.T) {
	a := testApp(t)
	w := do(a, "POST", "/folders", url.Values{"name": {"fresh"}, "in": {a.roots[0]}}, same)
	if got := w.Header().Get("Location"); got != "/new?folder="+url.QueryEscape(in(a, "fresh")) {
		t.Fatalf("create: %q", got)
	}
	for _, bad := range []string{"", "/tmp", in(a, "app"), a.roots[0] + "/.."} {
		w := do(a, "POST", "/folders", url.Values{"name": {"sneaky"}, "in": {bad}}, same)
		if !strings.Contains(w.Header().Get("Location"), "error=") {
			t.Errorf("folder created in %q", bad)
		}
	}
}
