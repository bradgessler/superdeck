package main

// A holder is a small process that keeps one Claude process alive on a
// pseudo-terminal, independent of Superdeck. Superdeck starts it by running
// its own binary as `superdeck hold ...`, detached, and talks to it over a
// Unix socket. When Superdeck restarts, the holder and Claude carry on, and
// the new Superdeck reconnects.
//
// The holder is deliberately dumb. It does not interpret what Claude prints;
// it keeps the last part of the raw output in memory and hands it over on
// request. Everything that could go wrong with parsing happens in Superdeck,
// where a failure cannot take Claude down.

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

const holdBuffer = 512 << 10 // how much raw output a holder keeps

// holdReply is the first line of a holder's answer to "snap".
type holdReply struct {
	Alive bool  `json:"alive"`
	Code  int   `json:"code"`
	Pid   int   `json:"pid"`
	Seq   int64 `json:"seq"`   // total bytes printed so far
	Reset bool  `json:"reset"` // the bytes that follow are everything kept, not a continuation
	N     int   `json:"n"`     // how many raw bytes follow
}

// holdExit is written beside the socket when the process ends, so Superdeck
// can learn how it ended even after the holder has gone.
type holdExit struct {
	Code int `json:"code"`
}

type holder struct {
	mu      sync.Mutex
	buf     []byte
	seq     int64
	alive   bool
	code    int
	cmd     *exec.Cmd
	tty     *os.File
	streams map[net.Conn]struct{}
}

// holdMain is the entry point for `superdeck hold`. It is not for people.
func holdMain(args []string) int {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	sock := fs.String("socket", "", "")
	dir := fs.String("dir", "", "")
	cols := fs.Int("cols", 200, "")
	rows := fs.Int("rows", 50, "")
	if err := fs.Parse(args); err != nil || *sock == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "superdeck hold is used by Superdeck itself")
		return 2
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

	h := &holder{alive: true, streams: map[net.Conn]struct{}{}}
	os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Chmod(*sock, 0o600)
	defer os.Remove(*sock)

	argv := fs.Args()
	h.cmd = exec.Command(argv[0], argv[1:]...)
	h.cmd.Dir = *dir
	h.cmd.Env = os.Environ() // Superdeck starts the holder with exactly the environment the process should have
	h.tty, err = pty.StartWithSize(h.cmd, &pty.Winsize{Cols: uint16(*cols), Rows: uint16(*rows)})
	if err != nil {
		// Say why in the output, so the failure is visible where output is shown.
		h.write([]byte("could not start: " + err.Error() + "\r\n"))
		h.finish(*sock, 127)
		return 1
	}

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		h.stop()
	}()
	go h.serve(ln)

	b := make([]byte, 16<<10)
	for {
		n, err := h.tty.Read(b)
		if n > 0 {
			h.write(b[:n])
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := h.cmd.Wait(); err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	h.tty.Close()
	h.finish(*sock, code)
	ln.Close()
	return 0
}

func (h *holder) write(b []byte) {
	h.mu.Lock()
	h.buf = append(h.buf, b...)
	if len(h.buf) > holdBuffer {
		h.buf = append([]byte(nil), h.buf[len(h.buf)-holdBuffer:]...)
	}
	h.seq += int64(len(b))
	for c := range h.streams {
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(b); err != nil {
			c.Close()
			delete(h.streams, c)
		}
	}
	h.mu.Unlock()
}

// finish records how the process ended: its output and exit code are written
// beside the socket before the holder goes away.
func (h *holder) finish(sock string, code int) {
	h.mu.Lock()
	h.alive, h.code = false, code
	out := append([]byte(nil), h.buf...)
	for c := range h.streams {
		c.Close()
	}
	h.mu.Unlock()
	base := strings.TrimSuffix(sock, ".sock")
	writeFileAtomic(base+".out", out)
	b, _ := json.Marshal(holdExit{Code: code})
	writeFileAtomic(base+".exit", b)
}

func writeFileAtomic(path string, b []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, path)
	}
}

// stop asks the process group to exit and kills it if it lingers. The holder
// does the waiting itself, so a stop completes even if Superdeck goes away.
func (h *holder) stop() {
	h.mu.Lock()
	alive := h.alive
	h.mu.Unlock()
	if !alive || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	pid := h.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		h.mu.Lock()
		still := h.alive
		h.mu.Unlock()
		if still {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

func (h *holder) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handle(c)
	}
}

// handle answers one request per connection:
//
//	snap <since>   the output printed after byte <since>, with the process's state
//	in <base64>    bytes to type into the process
//	stop           end the process
//	attach         a live two-way stream, for `superdeck attach`
func (h *holder) handle(c net.Conn) {
	defer func() { recover() }()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(io.LimitReader(c, 64<<10))
	line, err := r.ReadString('\n')
	if err != nil {
		c.Close()
		return
	}
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch verb {
	case "snap":
		since, _ := strconv.ParseInt(arg, 10, 64)
		h.mu.Lock()
		rep := holdReply{Alive: h.alive, Code: h.code, Seq: h.seq}
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
			break
		}
		if _, err := h.tty.Write(b); err != nil {
			fmt.Fprintln(c, "no")
			break
		}
		fmt.Fprintln(c, "ok")
	case "stop":
		h.stop()
		fmt.Fprintln(c, "ok")
	case "attach":
		c.SetDeadline(time.Time{})
		h.mu.Lock()
		c.Write(h.buf)
		alive := h.alive
		if alive {
			h.streams[c] = struct{}{}
		}
		h.mu.Unlock()
		if !alive {
			break
		}
		io.Copy(h.tty, c) // what the person types
		h.mu.Lock()
		delete(h.streams, c)
		h.mu.Unlock()
	}
	c.Close()
}

// ---- the client side, used by Superdeck ----

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
		return errors.New("that process has already exited")
	}
	return nil
}

// attachMain is `superdeck attach ID`: it connects this terminal to a running
// process, for looking at it or typing into it from the Mac itself.
func attachMain(state string, args []string) int {
	dir := runDir(state)
	if len(args) == 0 {
		socks, _ := filepath.Glob(filepath.Join(dir, "*.sock"))
		if len(socks) == 0 {
			fmt.Println("Nothing is running.")
			return 0
		}
		fmt.Println("Running processes. Attach with: superdeck attach ID")
		for _, s := range socks {
			fmt.Println("  " + strings.TrimSuffix(filepath.Base(s), ".sock"))
		}
		return 0
	}
	id := filepath.Base(args[0])
	c, err := net.DialTimeout("unix", filepath.Join(dir, id+".sock"), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "superdeck: nothing is running with id %s\n", id)
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
