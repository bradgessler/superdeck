package main

import (
	"bytes"
	"os"
	"os/exec"
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
	code := controlMain(a.state, cmd, args, strings.NewReader(stdin), &out)
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
	for _, bad := range []string{"idle", "work", "nobody", "../spare"} {
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
	if code != 0 || !strings.Contains(out, "Moving app to spare") {
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

func TestCommandsFromOutsideAnInstance(t *testing.T) {
	a := testApp(t)
	a.serveControl()
	// A terminal on the Mac has no instance of its own.
	nowhere := &Run{}
	if out, code := ask(t, a, nowhere, "", "move", "idle"); code == 0 || !strings.Contains(out, "Say which instance") {
		t.Errorf("move with no instance: %d %q", code, out)
	}
	if out, code := ask(t, a, nowhere, "", "status"); code != 0 || !strings.Contains(out, "Nothing is running") {
		t.Errorf("status: %d %q", code, out)
	}
	// With no Superdeck to talk to, it says so.
	t.Setenv("SUPERDECK_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	var out bytes.Buffer
	if controlMain(a.state, "status", nil, strings.NewReader(""), &out) == 0 || !strings.Contains(out.String(), "not answering") {
		t.Errorf("no Superdeck: %q", out.String())
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

// Everything the page's buttons do, less removing an account, can be asked
// for in words: from Claude in a session, or from a terminal.
func TestSuperdeckCanBeDrivenByCommands(t *testing.T) {
	moveDelay, settleFor = 20*time.Millisecond, 3*time.Second
	a := testApp(t)
	a.serveControl()
	me := &Run{} // a terminal on the Mac

	// A new folder, only where folders are allowed and only with a plain name.
	for _, bad := range [][]string{{"../escape"}, {".hidden"}, {"ok", "/tmp"}, {"app"}} {
		if out, code := ask(t, a, me, "", "folder", bad...); code == 0 {
			t.Errorf("folder %v was made: %q", bad, out)
		}
	}
	if out, code := ask(t, a, me, "", "folder", "fresh"); code != 0 || !isGitRepo(in(a, "fresh")) {
		t.Fatalf("folder: %d %q", code, out)
	}
	if out, _ := ask(t, a, me, "", "folders", "fre"); !strings.Contains(out, "fresh") || strings.Contains(out, "/app") {
		t.Errorf("folders: %q", out)
	}

	// Launching goes through the same checks as the page.
	os.WriteFile(filepath.Join(a.state, "accounts", "work", "asks-trust"), nil, 0o600)
	for _, bad := range [][]string{{"idle", "fresh"}, {"nobody", "fresh"}, {"work", "/etc"}, {"work", "nowhere"}, {"work", a.state}} {
		if out, code := ask(t, a, me, "", "launch", bad...); code == 0 || len(servers(a)) != 0 {
			t.Fatalf("launch %v: %d %q", bad, code, out)
		}
	}
	out, code := ask(t, a, me, "", "launch", "work", "fresh")
	if code != 0 || !strings.Contains(out, "asking whether to trust") {
		t.Fatalf("launch: %d %q", code, out)
	}
	run := a.callerInstance("work", in(a, "fresh"))
	if w := a.loadWanted(); run == nil || len(w) != 1 {
		t.Fatalf("launched instance not running or not remembered: %+v", w)
	}
	if out, _ := ask(t, a, me, "", "status"); !strings.Contains(out, "fresh: needs you, under work") || !strings.Contains(out, "superdeck answer "+run.ID+" yes") {
		t.Errorf("status while waiting: %q", out)
	}
	// The question is answered by name; an instance that is not asking is left alone.
	if out, code := ask(t, a, me, "", "answer", "fresh", "maybe"); code == 0 {
		t.Errorf("a bad answer was taken: %q", out)
	}
	if out, code := ask(t, a, me, "", "answer", "fresh", "yes"); code != 0 || !strings.Contains(out, "It is connected") {
		t.Fatalf("answer: %d %q", code, out)
	}
	if out, code := ask(t, a, me, "", "answer", "fresh", "yes"); code == 0 || !strings.Contains(out, "not asking") {
		t.Errorf("answering nothing: %d %q", code, out)
	}
	if out, _ := ask(t, a, run, "", "status"); !strings.Contains(out, "fresh: running") || !strings.Contains(out, "<- this session") || !strings.Contains(out, "https://claude.ai/code") {
		t.Errorf("status: %q", out)
	}

	// Relaunch and stop, by name from outside.
	if out, code := ask(t, a, me, "", "relaunch", "fresh"); code != 0 {
		t.Fatalf("relaunch: %d %q", code, out)
	}
	waitFor(t, func() bool { r := a.callerInstance("work", in(a, "fresh")); return r != nil && r.ID != run.ID })
	if out, code := ask(t, a, me, "", "stop", "nothing-like-this"); code == 0 {
		t.Errorf("stopped something that is not there: %q", out)
	}
	// Stopping the instance you are in waits long enough to hear the answer.
	run = a.callerInstance("work", in(a, "fresh"))
	if out, code := ask(t, a, run, "", "stop"); code != 0 || !strings.Contains(out, "this session will end") || !run.alive() {
		t.Fatalf("stop own: %d alive=%v %q", code, run.alive(), out)
	}
	waitFor(t, func() bool { return !run.alive() })
	if w := a.loadWanted(); len(w) != 0 {
		t.Errorf("a stopped instance is still remembered: %+v", w)
	}
}

func TestAnAccountCanBeAddedAndSignedInByCommands(t *testing.T) {
	a := testApp(t)
	a.serveControl()
	me := &Run{}
	if out, code := ask(t, a, me, "", "account", "add", "Bad Name"); code == 0 {
		t.Errorf("a bad account name was taken: %q", out)
	}
	out, code := ask(t, a, me, "", "account", "add", "extra")
	if code != 0 || !strings.Contains(out, "https://claude.ai/oauth/authorize") || !strings.Contains(out, "superdeck code extra CODE") {
		t.Fatalf("account add: %d %q", code, out)
	}
	if out, _ := ask(t, a, me, "", "status"); !strings.Contains(out, "Sign-in waiting for extra") {
		t.Errorf("status during sign-in: %q", out)
	}
	// Only something shaped like a code is ever typed into the sign-in.
	if out, code := ask(t, a, me, "", "code", "extra", "rm -rf /; echo"); code == 0 || a.pendingLogin("extra") == nil {
		t.Fatalf("a code that is not a code: %d %q", code, out)
	}
	if out, code := ask(t, a, me, "", "code", "extra", "wrong-code"); code == 0 || !strings.Contains(out, "did not work") {
		t.Fatalf("wrong code: %d %q", code, out)
	}
	if out, code := ask(t, a, me, "", "code", "extra", "good-code"); code == 0 || !strings.Contains(out, "No sign-in is waiting") {
		t.Errorf("code with no sign-in waiting: %d %q", code, out)
	}
	if out, code := ask(t, a, me, "", "signin", "extra"); code != 0 || !strings.Contains(out, "https://claude.ai/") {
		t.Fatalf("signin: %d %q", code, out)
	}
	if out, code := ask(t, a, me, "", "code", "extra", "good-code"); code != 0 || !strings.Contains(out, "extra is signed in as t@example.com") {
		t.Fatalf("good code: %d %q", code, out)
	}
	if out, _ := ask(t, a, me, "", "accounts"); !strings.Contains(out, "extra  (signed in") {
		t.Errorf("accounts: %q", out)
	}
}

// Claude starts each session in a Git worktree, which a repository with no
// commit cannot give it: on a real Mac every message sent to such an instance
// went unanswered, with the reason only in the terminal.
func TestARepositoryWithNoCommitsIsSaidToNeedOne(t *testing.T) {
	a := testApp(t)
	a.serveControl()

	// A folder Superdeck makes starts with a commit.
	made, err := createProject(a.roots[0], "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if noCommits(made) {
		t.Error("a new project folder has no commit")
	}
	if noCommits(a.roots[0]) {
		t.Error("a folder that is not a repository was said to have no commits")
	}

	// One made elsewhere may not.
	bare := filepath.Join(a.roots[0], "bare")
	os.Mkdir(bare, 0o755)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = bare
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	if !noCommits(bare) {
		t.Fatal("a repository with no commits was not noticed")
	}
	run, err := a.startServer("work", bare, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return a.instance(run.View()).Do == "open" })
	if iv := a.instance(run.View()); iv.State != "needs a commit" || !strings.Contains(iv.Lede, "no commits") {
		t.Errorf("page: %q %q", iv.State, iv.Lede)
	}
	if out, _ := ask(t, a, run, "", "status"); !strings.Contains(out, "no commits") {
		t.Errorf("status: %q", out)
	}
	before := a.instance(run.View()).Sig

	git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-q", "-m", "one")
	iv := a.instance(run.View())
	if iv.State != "running" || iv.Sig == before {
		t.Errorf("after a commit: %q, and the page would not have redrawn: %v", iv.State, iv.Sig == before)
	}
	git("pack-refs", "--all")
	if noCommits(bare) {
		t.Error("a packed branch was read as no commits")
	}
}

// Two Superdecks opening the same new or old database at once both tried to
// apply the first migration, and one refused to start.
func TestOpeningTheDatabaseTwiceAtOnceUpgradesItOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "superdeck.db")
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() {
			s, err := openStore(path)
			if err == nil {
				s.db.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// The Claude app lists only the instances of the login it is signed in as. A
// session that launches under another login has to be told, or the user is
// left looking for an instance their app will not show.
func TestLaunchingUnderAnotherLoginSaysToSwitch(t *testing.T) {
	a := testApp(t)
	for n, email := range map[string]string{"same": "", "other": "o@example.com"} {
		dir := filepath.Join(a.state, "accounts", n)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "in"), nil, 0o600)
		if email != "" {
			os.WriteFile(filepath.Join(dir, "email"), []byte(email), 0o600)
		}
	}
	a.serveControl()
	run, err := a.startServer("work", in(a, "app"), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return run.View().Link != "" })

	out, code := ask(t, a, run, "", "launch", "other", "app")
	if code != 0 || !strings.Contains(out, "only while signed in as o@example.com; this session is signed in as t@example.com") {
		t.Errorf("another login: %d %q", code, out)
	}
	out, code = ask(t, a, run, "", "launch", "same", "app")
	if code != 0 || !strings.Contains(out, "connected") || strings.Contains(out, "switch") {
		t.Errorf("the same login: %d %q", code, out)
	}
}

// Launching through Superdeck is asking for Remote Control, so the question
// Claude asks once per account about turning it on is answered yes without
// troubling anyone. Whether to trust a folder is still the owner's to answer.
func TestRemoteControlIsTurnedOnWithoutAsking(t *testing.T) {
	a := testApp(t)
	cfg := filepath.Join(a.state, "accounts", "work")
	os.WriteFile(filepath.Join(cfg, "asks-trust"), nil, 0o600)
	os.WriteFile(filepath.Join(cfg, "asks-consent"), nil, 0o600)
	run, err := a.startServer("work", in(a, "app"), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return run.View().Hint == "needs-trust" })
	time.Sleep(10 * consentEvery)
	if v := run.View(); v.Hint != "needs-trust" || v.Done {
		t.Fatalf("the trust question was answered for the owner: %q done=%v", v.Hint, v.Done)
	}
	if err := answer(run, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return run.View().Link != "" })
	if !strings.Contains(run.Tail(), "consented=y") {
		t.Errorf("Remote Control was not turned on: %s", run.Tail())
	}
}
