package main

import (
	"math/rand"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Two instances are put through a long run of the things that go wrong, in
// random order and on top of one another: Claude dies, a holder dies, a
// holder freezes, a socket is deleted, Switcharoo itself is replaced. After
// each, the same must be true: every instance a person launched is running
// exactly once, and nothing else is.
func TestInstancesSurviveARunOfRandomFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	minLife, restartBase = 0, 10*time.Millisecond
	a := testApp(t)
	if _, err := createProject(a.roots[0], "second"); err != nil {
		t.Fatal(err)
	}
	folders := []string{in(a, "app"), in(a, "second")}
	for _, f := range folders {
		run, err := a.startServer("work", f, "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		a.remember(wanted{run.Account, run.Project, run.spawn, run.capacity}, true)
	}
	cur := a
	t.Cleanup(func() { cur.closing.Store(true); cur.mgr.StopAll() })

	live := func() []*Run {
		var out []*Run
		for _, v := range servers(cur) {
			if !v.Done {
				out = append(out, cur.mgr.Get(v.ID))
			}
		}
		return out
	}
	// How many instances exist on this machine for this test, counted from
	// the process table: each is one holder and one stand-in Claude, and both
	// carry the stand-in's path, which is unique to the test.
	claudes := func() int {
		out, _ := exec.Command("pgrep", "-f", a.claude+" remote-control").Output()
		n := len(strings.Fields(string(out)))
		if n%2 == 1 {
			return -1 // a holder without its Claude, or the other way round
		}
		return n / 2
	}
	settled := func(why string) {
		t.Helper()
		ok := func() bool {
			rs := live()
			if len(rs) != len(folders) {
				return false
			}
			seen := map[string]bool{}
			for _, r := range rs {
				// One whose holder cannot be reached is alive and left
				// alone; there is no output to read from it.
				if seen[r.Project] || !processAlive(r.Pid()) || (!r.View().Lost && !strings.Contains(r.Tail(), "serving")) {
					return false
				}
				seen[r.Project] = true
			}
			return claudes() == len(folders)
		}
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			if ok() {
				return
			}
		}
		for _, v := range servers(cur) {
			t.Logf("  %s %s done=%v lost=%v pid=%d", v.ID, v.Project, v.Done, v.Lost, v.Run.Pid())
		}
		t.Fatalf("after %s: %d running in Switcharoo, %d in the process table, want %d of each", why, len(live()), claudes(), len(folders))
	}
	replace := func() {
		cur.closing.Store(true)
		next := newApp(a.roots, a.state, a.claude)
		if err := next.adopt(); err != nil {
			t.Fatal(err)
		}
		next.restore()
		cur = next
	}
	settled("starting")

	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 40; i++ {
		rs := live()
		r := rs[rng.Intn(len(rs))]
		r.mu.Lock()
		hpid, pid := r.hpid, r.pid
		r.mu.Unlock()
		if hpid <= 1 || pid <= 1 {
			// Never signal an id that is not a real process: 0 would mean
			// this test's own process group.
			t.Fatalf("a running instance has no process ids on record: holder %d, Claude %d", hpid, pid)
		}
		var what string
		switch rng.Intn(7) {
		case 0:
			what = "Claude killed"
			syscall.Kill(pid, syscall.SIGKILL)
		case 1:
			what = "holder killed"
			syscall.Kill(hpid, syscall.SIGKILL)
		case 2:
			what = "Switcharoo replaced"
			replace()
		case 3:
			what = "socket deleted, then Switcharoo replaced"
			os.Remove(r.sock())
			replace()
		case 4:
			what = "holder frozen for a while"
			syscall.Kill(hpid, syscall.SIGSTOP)
			time.Sleep(300 * time.Millisecond)
			syscall.Kill(hpid, syscall.SIGCONT)
		case 5:
			what = "Claude killed as Switcharoo is replaced"
			syscall.Kill(pid, syscall.SIGKILL)
			replace()
		case 6:
			what = "holder killed as Switcharoo is replaced"
			syscall.Kill(hpid, syscall.SIGKILL)
			replace()
		}
		settled(what)
	}
}

func TestPageAnswersAtWhateverAddressTheMacHas(t *testing.T) {
	a := newApp(nil, t.TempDir(), "claude")
	if err := a.allowHosts("0.0.0.0:7433", "Mac.Example:7433"); err != nil {
		t.Fatal(err)
	}
	name, _ := os.Hostname()
	for _, h := range []string{"192.168.9.9:7433", "10.0.1.50:7433", "[fe80::1]:7433", "mac.example:7433", "MAC.EXAMPLE:7433", "localhost:7433", strings.ToLower(name) + ":7433"} {
		if !a.hostOK(h) {
			t.Errorf("%s was refused; a change of address would lock the owner out", h)
		}
	}
	// A name someone else owns is how DNS rebinding gets in; so is another port.
	for _, h := range []string{"evil.example:7433", "192.168.9.9.evil.example:7433", "192.168.9.9:80", "192.168.9.9", ""} {
		if a.hostOK(h) {
			t.Errorf("%q was accepted", h)
		}
	}
	// Listening on one address means answering to that address only.
	b := newApp(nil, t.TempDir(), "claude")
	b.allowHosts("10.0.0.5:7433", "")
	if b.hostOK("10.0.0.6:7433") || !b.hostOK("10.0.0.5:7433") {
		t.Error("a server bound to one address should answer to that one only")
	}
}

func TestWatchdogRestartsAStuckSwitcharooAndLeavesAHealthyOneAlone(t *testing.T) {
	watchdogEvery, watchdogTimeout, watchdogFails = 10*time.Millisecond, 100*time.Millisecond, 3
	exits := make(chan struct{}, 8)
	watchdogExit = func() { exits <- struct{}{} }

	// Healthy: the real pages, answering.
	a := testApp(t)
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	if err := a.allowHosts(addr, ""); err != nil {
		t.Fatal(err)
	}
	go a.watchdog(addr)
	select {
	case <-exits:
		t.Fatal("a healthy Switcharoo was restarted")
	case <-time.After(400 * time.Millisecond):
	}
	a.closing.Store(true)

	// Stuck: something is listening but never answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	b := newApp(nil, t.TempDir(), "claude")
	go b.watchdog(ln.Addr().String())
	select {
	case <-exits:
	case <-time.After(5 * time.Second):
		t.Fatal("a Switcharoo that had stopped answering was left running")
	}
}
