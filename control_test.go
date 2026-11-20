package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ask runs the command a session would run, as that session.
func ask(t *testing.T, a *App, r *Run, stdin string, cmd string, args ...string) (string, int) {
	t.Helper()
	t.Setenv("SUPERDECK_SOCK", a.controlSock())
	t.Setenv("SUPERDECK_ACCOUNT", r.Account)
	t.Setenv("SUPERDECK_FOLDER", r.Project)
	var out bytes.Buffer
	code := controlMain(cmd, args, strings.NewReader(stdin), &out)
	return out.String(), code
}

func TestASessionCanMoveItsInstanceAndLeaveNotes(t *testing.T) {
	moveDelay = 20 * time.Millisecond
	a := testApp(t)
	os.MkdirAll(filepath.Join(a.state, "accounts", "spare"), 0o700)
	os.WriteFile(filepath.Join(a.state, "accounts", "spare", "in"), nil, 0o600)
	a.serveControl()

	run, err := a.startServer("work", in(a, "app"), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	// The instance is told how to reach Superdeck, and given the skill.
	want := "reach=work|" + in(a, "app") + "|" + a.controlSock()
	waitFor(t, func() bool { return strings.Contains(run.Tail(), want) })
	skill, err := os.ReadFile(filepath.Join(a.state, "accounts", "work", "skills", "superdeck", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), "name: superdeck") {
		t.Fatalf("the skill was not installed: %v", err)
	}

	out, code := ask(t, a, run, "", "accounts")
	if code != 0 || !strings.Contains(out, "work  (signed in as t@example.com)  <- this session") || !strings.Contains(out, "idle  (signed out)") {
		t.Errorf("accounts: %d %q", code, out)
	}
	if out, _ := ask(t, a, run, "", "handoff"); !strings.Contains(out, "No notes") {
		t.Errorf("handoff before any notes: %q", out)
	}

	// A move that cannot work changes nothing and says why.
	for _, bad := range []string{"idle", "work", "nobody", "../spare", ""} {
		out, code := ask(t, a, run, "notes", "move", bad)
		time.Sleep(60 * time.Millisecond)
		if code == 0 || !run.alive() {
			t.Fatalf("move to %q: code %d alive=%v %q", bad, code, run.alive(), out)
		}
	}
	if out, _ := ask(t, a, run, "", "handoff"); !strings.Contains(out, "No notes") {
		t.Errorf("a refused move left notes behind: %q", out)
	}

	out, code = ask(t, a, run, "Fix the login bug.\nNext: run the tests on branch wip.\n", "move", "Spare")
	if code != 0 || !strings.Contains(out, "Moving to spare") {
		t.Fatalf("move: %d %q", code, out)
	}
	if !run.alive() {
		t.Error("the instance was stopped before the session could hear the answer")
	}
	waitFor(t, func() bool { return !run.alive() })
	var moved *Run
	waitFor(t, func() bool {
		moved = a.callerInstance("spare", in(a, "app"))
		return moved != nil && strings.Contains(moved.Tail(), "serving")
	})
	if w := a.loadWanted(); len(w) != 1 || w[0].Account != "spare" {
		t.Errorf("remembered: %+v", w)
	}
	// The next session, under the other account, gets the notes.
	out, _ = ask(t, a, moved, "", "handoff")
	if !strings.Contains(out, "Next: run the tests on branch wip.") || !strings.Contains(out, `under the account "work"`) || !strings.Contains(out, "It was working in: ") {
		t.Errorf("handoff: %q", out)
	}
}

func TestSessionCommandsOnlyWorkInsideAnInstance(t *testing.T) {
	a := testApp(t)
	a.serveControl()
	t.Setenv("SUPERDECK_SOCK", "")
	var out bytes.Buffer
	if controlMain("accounts", nil, strings.NewReader(""), &out) == 0 || !strings.Contains(out.String(), "relaunch") {
		t.Errorf("outside an instance: %q", out.String())
	}
	// A session whose instance Superdeck is not running cannot move anything.
	ghost := &Run{Account: "work", Project: in(a, "app")}
	if out, code := ask(t, a, ghost, "", "move", "idle"); code == 0 || !strings.Contains(out, "nothing to move") {
		t.Errorf("move with no instance: %d %q", code, out)
	}
	// The socket is for the owner only, and survives the sweep at startup.
	if st, err := os.Stat(a.controlSock()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("control socket: %v %v", st, err)
	}
	if err := a.adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.controlSock()); err != nil {
		t.Error("startup swept away the socket sessions use")
	}
}
