package main

import (
	"errors"
	"fmt"
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

// expandRoots turns the folders and glob patterns given on the command line
// into the list of real directories Superdeck may launch Claude in. Nothing
// outside these is ever reachable.
func expandRoots(args []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				arg = filepath.Join(home, arg[2:])
			}
		}
		matches := []string{arg}
		isGlob := strings.ContainsAny(arg, "*?[")
		if isGlob {
			m, err := filepath.Glob(arg)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", arg, err)
			}
			matches = m
		}
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err == nil {
				abs, err = filepath.EvalSymlinks(abs)
			}
			st, serr := os.Stat(abs)
			if err != nil || serr != nil || !st.IsDir() {
				if isGlob {
					continue // a pattern may match files; only folders count
				}
				return nil, fmt.Errorf("%s is not a folder", m)
			}
			if abs == string(filepath.Separator) {
				return nil, errors.New("refusing to allow the whole disk; name the folders you work in")
			}
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("name at least one folder to allow, for example: superdeck ~/Projects")
	}
	sort.Strings(out)
	return out, nil
}

// resolveProject checks that path is a real directory at or inside one of the
// allowed roots, after symlinks are resolved.
func resolveProject(roots []string, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("that folder is not in the allowed list")
	}
	p, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", errors.New("that folder no longer exists")
	}
	ok := false
	for _, root := range roots {
		ok = ok || p == root || within(root, p)
	}
	if !ok {
		return "", errors.New("that folder is outside the folders Superdeck was started with")
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", errors.New("that is not a folder")
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
	Path  string // absolute
	Repos int    // for a workspace, how many repositories sit directly inside it
}

// listProjects returns the Git repositories at or up to three levels below
// each root, and the folders that directly contain them. It does not follow
// symlinks.
func listProjects(roots []string) []folder {
	var out []folder
	holds := map[string]int{}
	seen := map[string]bool{}
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if p != root {
				if name := d.Name(); strings.HasPrefix(name, ".") || name == "node_modules" {
					return filepath.SkipDir
				}
			}
			if isGitRepo(p) {
				if !seen[p] {
					seen[p] = true
					out = append(out, folder{Path: p})
					if p != root {
						holds[filepath.Dir(p)]++
					}
				}
				return filepath.SkipDir
			}
			if rel, _ := filepath.Rel(root, p); strings.Count(rel, string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		})
	}
	for p, n := range holds {
		if !seen[p] {
			seen[p] = true
			out = append(out, folder{Path: p, Repos: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > 3000 {
		out = out[:3000]
	}
	return out
}

// createProject makes a new directory directly under root and runs git init
// in it. It never overwrites an existing path. It returns the new folder's path.
func createProject(root, name string) (string, error) {
	if !projectNameRe.MatchString(name) {
		return "", errors.New("folder names use letters, digits, dots, dashes and underscores")
	}
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return "", errors.New("there is already a folder with that name")
		}
		return "", err
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	cmd.Env = childEnv("") // the same explicit environment everything else gets
	if out, err := cmd.CombinedOutput(); err != nil {
		// Do not leave the empty folder behind: it would never be listed, and
		// would block trying the same name again.
		os.RemoveAll(dir)
		why, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if why == "" {
			why = err.Error()
		}
		return "", errors.New("git init failed: " + why)
	}
	// Claude starts each session in a Git worktree, and a repository with no
	// commit has nothing to base one on: every session would fail to start.
	// So the folder begins with an empty commit, made as the owner if Git
	// knows who that is and as Superdeck if not.
	if err := firstCommit(dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func firstCommit(dir string) error {
	var why string
	for _, who := range [][]string{nil, {"-c", "user.name=Superdeck", "-c", "user.email=superdeck@localhost"}} {
		args := append(append([]string{}, who...), "commit", "--allow-empty", "--no-verify", "-q", "-m", "Start")
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = childEnv("")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if why, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n"); why == "" {
			why = err.Error()
		}
	}
	return errors.New("git commit failed: " + why)
}

// noCommits reports whether dir is a Git repository whose current branch has
// no commit yet. It reads Git's files rather than running Git, and says no
// whenever it is not sure: it only decides what a page says.
func noCommits(dir string) bool {
	git := filepath.Join(dir, ".git")
	head, err := os.ReadFile(filepath.Join(git, "HEAD"))
	if err != nil {
		return false // not a repository, or a worktree whose .git is a file
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: ")
	if !ok || !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return false // a detached HEAD names a commit
	}
	if _, err := os.Stat(filepath.Join(git, ref)); err == nil {
		return false
	} else if !os.IsNotExist(err) {
		return false
	}
	packed, err := os.ReadFile(filepath.Join(git, "packed-refs"))
	if err != nil {
		return os.IsNotExist(err)
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if strings.HasSuffix(line, " "+ref) {
			return false
		}
	}
	return true
}
