package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// wanted is an instance a person launched and has not stopped. The list is
// saved so that Superdeck brings the same instances back after it restarts,
// for example when the Mac reboots.
type wanted struct {
	Account  string `json:"account"`
	Project  string `json:"project"`
	Spawn    string `json:"spawn"`
	Capacity int    `json:"capacity"`
}

func (a *App) wantedPath() string { return filepath.Join(a.state, "instances.json") }

func (a *App) loadWanted() []wanted {
	var list []wanted
	if b, err := os.ReadFile(a.wantedPath()); err == nil {
		json.Unmarshal(b, &list)
	}
	return list
}

// remember adds or removes one instance from the saved list.
func (a *App) remember(w wanted, keep bool) {
	a.wantMu.Lock()
	defer a.wantMu.Unlock()
	var next []wanted
	for _, o := range a.loadWanted() {
		if o.Account != w.Account || o.Project != w.Project {
			next = append(next, o)
		}
	}
	if keep {
		next = append(next, w)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Project+next[i].Account < next[j].Project+next[j].Account })
	b, _ := json.MarshalIndent(next, "", "  ")
	tmp := a.wantedPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
		os.Rename(tmp, a.wantedPath())
	}
}

// restore relaunches every remembered instance. Each one goes through the
// same validation as a fresh launch, so a hand-edited file gains nothing.
func (a *App) restore() {
	for _, w := range a.loadWanted() {
		if _, err := a.startServer(w.Account, w.Project, w.Spawn, w.Capacity, 0); err != nil {
			log.Printf("could not bring back %s as %s: %v", w.Project, w.Account, err)
		}
	}
}

// launchAgent returns a launchd property list that starts Superdeck at login
// with the given arguments and restarts it if it exits.
func launchAgent(binary string, args []string, logPath, path string) string {
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var argv strings.Builder
	for _, s := range append([]string{binary}, args...) {
		fmt.Fprintf(&argv, "\n    <string>%s</string>", esc(s))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.bradgessler.superdeck</string>
  <key>ProgramArguments</key>
  <array>%s
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, argv.String(), esc(path), esc(logPath), esc(logPath))
}
