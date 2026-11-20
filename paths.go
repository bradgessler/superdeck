package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	accountNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	projectNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// within reports whether p is strictly inside root. Both must be absolute and
// already symlink-resolved.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveProject turns a path relative to root into a real directory, refusing
// anything that lands outside root after symlinks are resolved.
func resolveProject(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("project must be a path relative to the root")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(rootReal, rel))
	if err != nil {
		return "", errors.New("project not found")
	}
	if !within(rootReal, p) {
		return "", errors.New("project is outside the root")
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", errors.New("project is not a directory")
	}
	return p, nil
}

// isGitRepo reports whether dir is a main Git checkout. A worktree or
// submodule has a .git file rather than a .git directory and does not count,
// so worktrees never show up as projects of their own.
func isGitRepo(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

// A folder is somewhere an instance can be launched: a Git repository, or a
// folder that holds repositories (a workspace you run agents across).
type folder struct {
	Rel   string
	Repos int // for a workspace, how many repositories sit directly inside it
}

// listProjects returns Git repositories up to three levels below root, and the
// folders that directly contain them, as paths relative to root. It does not
// follow symlinks.
func listProjects(root string) []folder {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	var out []folder
	holds := map[string]int{}
	filepath.WalkDir(rootReal, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p == rootReal {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(rootReal, p)
		if isGitRepo(p) {
			out = append(out, folder{Rel: rel})
			if parent := filepath.Dir(rel); parent != "." {
				holds[parent]++
			}
			return filepath.SkipDir
		}
		if strings.Count(rel, string(filepath.Separator)) >= 2 {
			return filepath.SkipDir
		}
		return nil
	})
	for rel, n := range holds {
		out = append(out, folder{Rel: rel, Repos: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	if len(out) > 2000 {
		out = out[:2000]
	}
	return out
}

// createProject makes a new directory directly under root and runs git init
// in it. It never overwrites an existing path.
func createProject(root, name string) (string, error) {
	if !projectNameRe.MatchString(name) {
		return "", errors.New("name must be letters, digits, dot, dash or underscore")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(rootReal, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New("git init failed: " + strings.TrimSpace(string(out)))
	}
	return name, nil
}
