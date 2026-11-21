package main

// A holder is a small process that keeps one Claude process alive on a
// pseudo-terminal, independent of Switcharoo. Switcharoo starts it by running
// its own binary as `switcharoo hold ...`, detached, and talks to it over a
// Unix socket. When Switcharoo restarts, the holder and Claude carry on, and
// the new Switcharoo reconnects.
//
// The holder is deliberately dumb. It does not interpret what Claude prints;
// it keeps the last part of the raw output in memory and hands it over on
// request. Everything that could go wrong with parsing happens in Switcharoo,
// where a failure cannot take Claude down.
//
// Rules for changing this file:
//
//   - Nothing a client does may stall the holder. No write to a client
//     happens while h.mu is held, and every client has a deadline or a
//     bounded queue.
//   - Holders outlive Switcharoo, so a new Switcharoo must understand holders
//     started by an older one. Add verbs and reply fields; never change the
//     meaning of existing ones. holdProtocol says which a holder speaks.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	holdBuffer   = 512 << 10 // how much raw output a holder keeps
	holdProtocol = 1
)

// holdReply is the first line of a holder's answer to "snap".
type holdReply struct {
	V     int   `json:"v"` // protocol version
	Alive bool  `json:"alive"`
	Code  int   `json:"code"`
	Pid   int   `json:"pid"`   // the Claude process
	Hpid  int   `json:"hpid"`  // the holder itself
	Seq   int64 `json:"seq"`   // total bytes printed so far
	Reset bool  `json:"reset"` // the bytes that follow are everything kept, not a continuation
	N     int   `json:"n"`     // how many raw bytes follow
}

// holdExit is written beside the socket when the process ends, so Switcharoo
// can learn how it ended even after the holder has gone.
type holdExit struct {
	Code int `json:"code"`
}

// stream is one attached terminal. Output for it waits in a bounded queue; a
// viewer that falls behind is dropped rather than waited for.
type stream struct {
	c net.Conn
	q chan []byte
}

type holder struct {
	mu      sync.Mutex
	buf     []byte
	seq     int64
	alive   bool
	reaped  bool // the process has exited and been collected; its id may now belong to something else
	code    int
	cmd     *exec.Cmd
	tty     *os.File
	streams map[*stream]struct{}
}

// holdMain is the entry point for `switcharoo hold`. It is not for people.
func holdMain(args []string) (status int) {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	sock := fs.String("socket", "", "")
	dir := fs.String("dir", "", "")
	logPath := fs.String("log", "", "")
	cols := fs.Int("cols", 200, "")
	rows := fs.Int("rows", 50, "")
	if err := fs.Parse(args); err != nil || *sock == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "switcharoo hold is used by Switcharoo itself")
		return 2
	}
	// A holder's own failures must leave a trace somewhere.
	say := func(format string, a ...any) {}
	if *logPath != "" {
		if f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			defer f.Close()
			id := strings.TrimSuffix(filepath.Base(*sock), ".sock")
			say = func(format string, a ...any) {
				fmt.Fprintf(f, "%s holder %s: %s\n", time.Now().Format("2006/01/02 15:04:05"), id, fmt.Sprintf(format, a...))
			}
			// A Go panic prints to standard error; send that to the log too.
			syscall.Dup2(int(f.Fd()), 2)
		}
	}

	// A holder must outlive whatever started it, so it shrugs off hang-ups.
	// They are caught and dropped rather than ignored: an ignored signal is
	// inherited by the process the holder starts, and Claude must still die
	// from a hang-up if its holder ever goes away.
	hup := make(chan os.Signal, 4)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		for range hup {
		}
	}()

	h := &holder{alive: true, streams: map[*stream]struct{}{}}
	os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		say("cannot listen: %v", err)
		return 1
	}
	os.Chmod(*sock, 0o600)

	argv := fs.Args()
	h.cmd = exec.Command(argv[0], argv[1:]...)
	h.cmd.Dir = *dir
	h.cmd.Env = os.Environ() // Switcharoo starts the holder with exactly the environment the process should have
	h.tty, err = pty.StartWithSize(h.cmd, &pty.Winsize{Cols: uint16(*cols), Rows: uint16(*rows)})
	if err != nil {
		// Say why in the output, so the failure is visible where output is shown.
		h.write([]byte("could not start: " + err.Error() + "\r\n"))
		say("could not start %s: %v", argv[0], err)
		h.finish(*sock, 127)
		ln.Close()
		os.Remove(*sock)
		return 1
	}

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		h.stop()
	}()
	go h.serve(ln, say)

	// Wait for the process itself, not only for its terminal to close: a
	// background process it leaves behind can keep the terminal open, and the
	// instance must not look alive because of that.
	exited := make(chan int, 1)
	go func() {
		code := 0
		err := h.cmd.Wait()
		h.mu.Lock()
		h.reaped = true
		h.mu.Unlock()
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		exited <- code
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		b := make([]byte, 16<<10)
		for {
			n, err := h.tty.Read(b)
			if n > 0 {
				h.write(b[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	code := <-exited
	select { // let the last of its output arrive
	case <-drained:
	case <-time.After(time.Second):
	}
	h.tty.Close()
	h.finish(*sock, code)
	ln.Close()
	os.Remove(*sock)
	return 0
}

func (h *holder) write(b []byte) {
	chunk := append([]byte(nil), b...)
	h.mu.Lock()
	h.buf = append(h.buf, chunk...)
	if len(h.buf) > holdBuffer {
		h.buf = append([]byte(nil), h.buf[len(h.buf)-holdBuffer:]...)
	}
	h.seq += int64(len(chunk))
	for s := range h.streams {
		select {
		case s.q <- chunk:
		default: // this viewer is not keeping up; let it go
			delete(h.streams, s)
			close(s.q)
		}
	}
	h.mu.Unlock()
}

// finish records how the process ended: its output and exit code are written
// beside the socket before the holder goes away. The output is written first,
// so an exit record always has its output with it. Only once both are on disk
// does the holder say the process has ended: Switcharoo clears the files away
// when it hears that, and must not do so before they exist.
func (h *holder) finish(sock string, code int) {
	h.mu.Lock()
	out := append([]byte(nil), h.buf...)
	h.mu.Unlock()
	base := strings.TrimSuffix(sock, ".sock")
	writeFileAtomic(base+".out", out)
	b, _ := json.Marshal(holdExit{Code: code})
	writeFileAtomic(base+".exit", b)
	h.mu.Lock()
	h.alive, h.code = false, code
	for s := range h.streams {
		delete(h.streams, s)
		close(s.q)
	}
	h.mu.Unlock()
}

func writeFileAtomic(path string, b []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, path)
	}
}

// stop asks the process group to exit and kills it if it lingers. The holder
// does the waiting itself, so a stop completes even if Switcharoo goes away.
func (h *holder) stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	pid := h.cmd.Process.Pid
	// The signal is sent with the lock held and only while the process has
	// not been collected, so it can never land on something else that was
	// given the same id afterwards.
	send := func(sig syscall.Signal) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.alive || h.reaped {
			return false
		}
		syscall.Kill(-pid, sig)
		return true
	}
	if !send(syscall.SIGTERM) {
		return
	}
	go func() {
		time.Sleep(5 * time.Second)
		send(syscall.SIGKILL)
	}()
}

func (h *holder) serve(ln net.Listener, say func(string, ...any)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Running out of file descriptors, say, must not end the service:
			// Switcharoo would take the silence for a dead holder.
			say("accept: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go h.handle(c)
	}
}

// handle answers one request per connection:
//
//	snap <since>   the output printed after byte <since>, with the process's state
//	in <base64>    bytes to type into the process
//	stop           end the process
//	attach         a live two-way stream, for `switcharoo attach`
func (h *holder) handle(c net.Conn) {
	defer func() { recover() }()
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(io.LimitReader(c, 64<<10))
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch verb {
	case "snap":
		since, _ := strconv.ParseInt(arg, 10, 64)
		h.mu.Lock()
		rep := holdReply{V: holdProtocol, Alive: h.alive, Code: h.code, Seq: h.seq, Hpid: os.Getpid()}
		if h.cmd != nil && h.cmd.Process != nil {
			rep.Pid = h.cmd.Process.Pid
		}
		start := h.seq - int64(len(h.buf))
		data := h.buf
		if since >= start && since <= h.seq {
			data = h.buf[since-start:]
		} else {
			rep.Reset = true
		}
		data = append([]byte(nil), data...)
		h.mu.Unlock()
		rep.N = len(data)
		head, _ := json.Marshal(rep)
		c.Write(append(head, '\n'))
		c.Write(data)
	case "in":
		b, err := base64.StdEncoding.DecodeString(arg)
		h.mu.Lock()
		alive := h.alive
		h.mu.Unlock()
		if err != nil || !alive {
			fmt.Fprintln(c, "no")
			return
		}
		// Typing can block if the process is not reading. Do not let that
		// hold this connection past its deadline.
		done := make(chan error, 1)
		go func() { _, err := h.tty.Write(b); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintln(c, "no")
				return
			}
			fmt.Fprintln(c, "ok")
		case <-time.After(5 * time.Second):
			fmt.Fprintln(c, "no")
		}
	case "stop":
		h.stop()
		fmt.Fprintln(c, "ok")
	case "attach":
		c.SetDeadline(time.Time{})
		s := &stream{c: c, q: make(chan []byte, 256)}
		h.mu.Lock()
		backlog := append([]byte(nil), h.buf...)
		alive := h.alive
		if alive {
			h.streams[s] = struct{}{}
		}
		h.mu.Unlock()
		if !alive {
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			c.Write(backlog)
			return
		}
		// The viewer's writer: everything it is sent goes through here, with
		// a deadline, and never while the holder's lock is held.
		go func() {
			send := func(b []byte) bool {
				c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, err := c.Write(b)
				return err == nil
			}
			ok := send(backlog)
			for b := range s.q {
				if ok = ok && send(b); !ok {
					break // a viewer that cannot be written to is let go at once
				}
			}
			c.Close() // this also ends the read below, which unregisters the viewer
		}()
		io.Copy(h.tty, c) // what the person types, until they detach
		h.mu.Lock()
		if _, on := h.streams[s]; on {
			delete(h.streams, s)
			close(s.q)
		}
		h.mu.Unlock()
	}
}

// ---- the client side, used by Switcharoo ----

// holdGone reports whether an error from talking to a holder means the holder
// is definitely not there (nothing is listening on its socket), as opposed to
// merely slow or busy. Only this may be taken as evidence that it has died.
func holdGone(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) || errors.Is(err, os.ErrNotExist)
}

func holdDial(sock string) (net.Conn, error) {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, nil
}

// holdSnap asks a holder for output after byte since.
func holdSnap(sock string, since int64) (holdReply, []byte, error) {
	var rep holdReply
	c, err := holdDial(sock)
	if err != nil {
		return rep, nil, err
	}
	defer c.Close()
	fmt.Fprintf(c, "snap %d\n", since)
	r := bufio.NewReaderSize(c, 64<<10)
	head, err := r.ReadBytes('\n')
	if err != nil {
		return rep, nil, err
	}
	if err := json.Unmarshal(head, &rep); err != nil {
		return rep, nil, err
	}
	if rep.N < 0 || rep.N > holdBuffer {
		return rep, nil, errors.New("holder sent a bad length")
	}
	data := make([]byte, rep.N)
	if _, err := io.ReadFull(r, data); err != nil {
		return rep, nil, err
	}
	return rep, data, nil
}

func holdSay(sock, line string) error {
	c, err := holdDial(sock)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Fprintln(c, line)
	ans, _ := bufio.NewReader(c).ReadString('\n')
	if strings.TrimSpace(ans) != "ok" {
		return errors.New("that process is not accepting input right now")
	}
	return nil
}

// attachMain is `switcharoo attach [-state DIR] [ID]`: it connects this
// terminal to a running process, for looking at it or typing into it from the
// Mac itself.
func attachMain(defaultState string, args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	state := fs.String("state", defaultState, "Switcharoo's state directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := runDir(*state)
	if fs.NArg() == 0 {
		socks, _ := filepath.Glob(filepath.Join(dir, "*.sock"))
		var live []string
		for _, s := range socks {
			if _, _, err := holdSnap(s, 1<<62); err == nil {
				live = append(live, strings.TrimSuffix(filepath.Base(s), ".sock"))
			}
		}
		if len(live) == 0 {
			fmt.Println("Nothing is running.")
			return 0
		}
		fmt.Println("Running processes. Attach with: switcharoo attach ID")
		for _, id := range live {
			fmt.Println("  " + id)
		}
		return 0
	}
	id := filepath.Base(fs.Arg(0))
	c, err := net.DialTimeout("unix", filepath.Join(dir, id+".sock"), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "switcharoo: nothing is running with id %s\n", id)
		return 1
	}
	defer c.Close()
	fmt.Fprintln(c, "attach")
	fmt.Fprint(os.Stderr, "Attached. Press Ctrl-] to detach; what you type goes to Claude.\r\n")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		old, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer term.Restore(int(os.Stdin.Fd()), old)
		}
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(os.Stdout, c); done <- struct{}{} }()
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(b)
			for i := 0; i < n; i++ {
				if b[i] == 0x1d { // Ctrl-]
					done <- struct{}{}
					return
				}
			}
			if n > 0 {
				c.Write(b[:n])
			}
			if err != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	<-done
	fmt.Fprint(os.Stderr, "\r\nDetached.\r\n")
	return 0
}
