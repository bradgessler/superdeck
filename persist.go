package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

func (a *App) loadWanted() []wanted {
	list, err := a.data().Wanted()
	if err != nil {
		log.Printf("reading saved instances: %v", err)
	}
	return list
}

// remember adds or removes one instance from the saved list.
func (a *App) remember(w wanted, keep bool) {
	if err := a.data().SetWanted(w, keep); err != nil {
		log.Printf("saving instances: %v", err)
	}
}

// importLegacy brings in accounts.json and instances.json from versions that
// kept plain files, then sets them aside so it only happens once.
func (a *App) importLegacy() {
	namesFile := filepath.Join(a.state, "accounts.json")
	if b, err := os.ReadFile(namesFile); err == nil {
		names := map[string]string{}
		json.Unmarshal(b, &names)
		for _, id := range a.accountDirs() {
			name := names[id]
			if !accountNameRe.MatchString(name) {
				name = id
			}
			if err := a.store.AddAccount(id, name); err != nil {
				a.store.AddAccount(id, id) // name clash: fall back to the directory name
			}
		}
		os.Rename(namesFile, namesFile+".imported")
		log.Printf("imported account names from %s", namesFile)
	}
	instFile := filepath.Join(a.state, "instances.json")
	if b, err := os.ReadFile(instFile); err == nil {
		var list []wanted
		json.Unmarshal(b, &list)
		for _, w := range list {
			a.store.SetWanted(w, true)
		}
		os.Rename(instFile, instFile+".imported")
		log.Printf("imported %d saved instances from %s", len(list), instFile)
	}
}

// running reports whether an instance for this account and folder is alive.
func (a *App) running(accountID, folder string) bool {
	for _, v := range a.mgr.List() {
		if v.Kind == "server" && !v.Done && v.Account == accountID && v.Project == folder {
			return true
		}
	}
	return false
}

// restore launches every remembered instance that is not already running, for
// example after the Mac has rebooted. Each one goes through the same
// validation as a fresh launch, so edited data gains nothing.
func (a *App) restore() {
	for _, w := range a.loadWanted() {
		if !filepath.IsAbs(w.Project) && len(a.roots) > 0 {
			// Saved by an older version, relative to its one projects folder.
			old := w
			w.Project = filepath.Join(a.roots[0], w.Project)
			a.remember(old, false)
			a.remember(w, true)
		}
		if a.running(w.Account, w.Project) {
			continue // its holder kept it going while Superdeck was away
		}
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
  <key>AbandonProcessGroup</key>
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
