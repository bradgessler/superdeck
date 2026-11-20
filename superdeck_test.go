package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		status: map[string]map[string]any{}, hosts: map[string]bool{}}
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
