// Package gitexport is the git plumbing of the release export (gonk-cuwn):
// it derives the public tree from an internal commit, checks it with
// exportguard and a secret scanner, builds the snapshot commit and tag
// deterministically, verifies a published commit against a recomputation,
// and -- in one separately fenced function -- publishes.
//
// Everything works in a FRESH clone of origin made for the purpose, never in
// a working tree: the public tree is a function of a commit, and nothing a
// developer has checked out, staged or configured can enter it.
package gitexport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Refused is the error for "the export may not proceed": a finding, a broken
// precondition, a list that rotted. It is distinct from an operational
// failure (git missing, disk full) only so that callers can say which it was;
// both stop the export.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "refused: " + r.Reason }

func refuse(format string, a ...any) error {
	return &Refused{Reason: fmt.Sprintf(format, a...)}
}

// IsRefused reports whether err is a refusal.
func IsRefused(err error) bool {
	var r *Refused
	return errors.As(err, &r)
}

// Repo is a fresh, checkout-less clone of origin plus a private scratch
// directory. Close removes both.
type Repo struct {
	root   string // scratch root; everything below is ours
	origin string // what the clone was made from; Record pushes refs/export/* there
	dir    string // the clone (a bare-style repository directory)
	branch string
	seq    int
}

// Clone makes the working clone: `git clone --no-checkout --single-branch
// --branch <branch> <origin>`. The clone runs with the caller's git
// configuration (it may need credentials); every later plumbing command runs
// isolated from it.
func Clone(origin, branch string) (*Repo, error) {
	if origin == "" {
		return nil, fmt.Errorf("no origin given")
	}
	if branch == "" {
		branch = "main"
	}
	root, err := os.MkdirTemp("", "relexport-")
	if err != nil {
		return nil, err
	}
	r := &Repo{root: root, origin: origin, dir: filepath.Join(root, "clone"), branch: branch}
	if err := os.Mkdir(filepath.Join(root, "home"), 0o700); err != nil {
		r.Close()
		return nil, err
	}
	if _, err := netGit("", "clone", "--quiet", "--no-checkout", "--single-branch", "--branch", branch, "--", origin, r.dir); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Close removes the clone and everything the export wrote beside it.
func (r *Repo) Close() {
	if r != nil && r.root != "" {
		_ = os.RemoveAll(r.root)
	}
}

// originRef is the ref SRC must be an ancestor of.
func (r *Repo) originRef() string { return "refs/remotes/origin/" + r.branch }

// tmp returns a fresh path under the scratch root.
func (r *Repo) tmp(name string) string {
	r.seq++
	return filepath.Join(r.root, fmt.Sprintf("%03d-%s", r.seq, name))
}

// isolatedEnv is the environment of every plumbing command. Nothing of the
// caller's survives except PATH: no user or system git configuration, no
// identity, no locale, no time zone. That is what makes the object ids a
// function of the inputs alone.
func (r *Repo) isolatedEnv(extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(r.root, "home"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
	return append(env, extra...)
}

type gitCmd struct {
	stdin []byte
	env   []string
}

// git runs one isolated plumbing command in the clone and returns its
// standard output with the trailing newline removed.
func (r *Repo) git(args ...string) (string, error) {
	return r.gitWith(gitCmd{}, args...)
}

func (r *Repo) gitWith(c gitCmd, args ...string) (string, error) {
	out, err := run(r.dir, r.isolatedEnv(c.env...), c.stdin, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// gitRaw is git with the output untouched (blob contents, NUL-separated
// lists).
func (r *Repo) gitRaw(args ...string) ([]byte, error) {
	return run(r.dir, r.isolatedEnv(), nil, args...)
}

// gitOK runs a command whose exit status is the answer (0 = yes, 1 = no).
func (r *Repo) gitOK(args ...string) (bool, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.isolatedEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
}

// netGit runs a git command that talks to a remote, with the caller's own
// environment (credentials, TLS settings) and no prompt.
func netGit(dir string, args ...string) (string, error) {
	out, err := run(dir, netEnv(), nil, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// netEnv is the caller's environment without the variables that point git at
// another repository, index or object store. A hook exports exactly those
// (GIT_DIR, GIT_INDEX_FILE), and with them a clone or fetch started from a
// hook would act on the hook's repository, not on the scratch clone.
func netEnv() []string {
	drop := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_COMMON_DIR": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_NAMESPACE": true, "GIT_PREFIX": true, "GIT_CEILING_DIRECTORIES": true,
	}
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func run(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// treeEntry is one line of `git ls-tree -r`.
type treeEntry struct {
	Mode, Type, ID, Path string
}

// lsTree lists every entry of a tree, recursively.
func (r *Repo) lsTree(treeish string) ([]treeEntry, error) {
	out, err := r.gitRaw("ls-tree", "-r", "-z", "--full-tree", treeish)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record")
		}
		entries = append(entries, treeEntry{Mode: f[0], Type: f[1], ID: f[2], Path: string(path)})
	}
	return entries, nil
}

// show returns the bytes of the blob at rev:path, and whether it exists.
func (r *Repo) show(rev, path string) ([]byte, bool, error) {
	ok, err := r.gitOK("rev-parse", "--verify", "--quiet", rev+":"+path)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	typ, err := r.git("cat-file", "-t", rev+":"+path)
	if err != nil || typ != "blob" {
		return nil, false, err
	}
	data, err := r.gitRaw("cat-file", "blob", rev+":"+path)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// eachBlob streams the content of every blob entry to fn, in order, through
// one `git cat-file --batch`.
func (r *Repo) eachBlob(entries []treeEntry, fn func(e treeEntry, data []byte) error) error {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = r.dir
	cmd.Env = r.isolatedEnv()
	var ids bytes.Buffer
	for _, e := range entries {
		ids.WriteString(e.ID + "\n")
	}
	cmd.Stdin = &ids
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(stdout, 1<<20)
	var ferr error
	for _, e := range entries {
		header, err := rd.ReadString('\n')
		if err != nil {
			ferr = fmt.Errorf("cat-file: %v", err)
			break
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != e.ID || f[1] != "blob" {
			ferr = fmt.Errorf("cat-file: unexpected header for %s", e.Path)
			break
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			ferr = err
			break
		}
		data := make([]byte, size+1) // the content, then one LF
		if _, err := io.ReadFull(rd, data); err != nil {
			ferr = fmt.Errorf("cat-file: %v", err)
			break
		}
		if err := fn(e, data[:size]); err != nil {
			ferr = err
			break
		}
	}
	if ferr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ferr
	}
	_, _ = io.Copy(io.Discard, rd)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("cat-file: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// resolveSource checks that src is a full commit id that exists in the clone
// and is on origin's branch.
func (r *Repo) resolveSource(src string) error {
	if !validID(src) {
		return refuse("the source commit must be a full 40-hex commit id, got %q", src)
	}
	if typ, err := r.git("cat-file", "-t", src); err != nil || typ != "commit" {
		return refuse("source %s is not a commit in origin's %s", src, r.branch)
	}
	ok, err := r.gitOK("merge-base", "--is-ancestor", src, r.originRef())
	if err != nil {
		return err
	}
	if !ok {
		return refuse("source %s is not on origin/%s", src, r.branch)
	}
	return nil
}

func validID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
