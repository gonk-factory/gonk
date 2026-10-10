// Command relexport builds the PUBLIC TREE of a gonk release and, in one
// separately fenced step, publishes it (gonk-cuwn).
//
// The public mirror never receives the internal commit graph. It receives one
// snapshot commit per release whose tree is derived from an internal commit:
// that commit's tree, minus the paths listed in release/internal-only.txt AS
// COMMITTED THERE, plus one generated file, EXPORT-SOURCE. The tree is
// refused unless it passes the deny-pattern guard (internal/exportguard) and
// a secret scan with zero findings.
//
//	relexport tree    --src SRC [--version vX.Y.Z]
//	relexport write   --src SRC --dir DIR [--version vX.Y.Z]
//	relexport check   --src SRC [--version vX.Y.Z] [--guard-only]
//	relexport commit  --src SRC (--root | --parent PARENT) --version vX.Y.Z [--mirror M] [--out DIR]
//	relexport record  --src SRC (--root | --parent PARENT) --version vX.Y.Z [--mirror M] [--confirm COMMIT]
//	relexport verify  --src SRC --public COMMIT|REF --mirror M
//	relexport publish --src SRC (--root | --parent PARENT --prev-src SRC1) --version vX.Y.Z
//	                  --reviewed-tree T --mirror M [--ref main|tag] [--confirm OBJECT]
//
// A snapshot is either a declared ROOT (--root: the first commit of a new
// public history, published to a mirror that is empty) or a child of the
// previous release's snapshot (--parent, fenced at publish by --prev-src).
//
// tree, write, check, commit and verify never write to any remote. record
// writes one ref, refs/export/<version>, to ORIGIN (the internal repository)
// and can write nowhere else. publish is the only subcommand that can write
// to the mirror, and it has no default mirror: see the usage text of
// `relexport publish` for everything it demands.
//
// Every subcommand works in a fresh clone of --origin (default: this
// repository's origin remote) that it deletes afterwards; nothing checked
// out, staged or configured here can enter the result.
//
// Exit status: 0 done; 1 refused or failed; 2 usage; 3 publish or record dry
// run (every check passed, nothing pushed, confirmation not given).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gonk-factory/gonk/internal/exportguard/gitexport"
)

const (
	exitOK = iota
	exitRefused
	exitUsage
	exitDryRun
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type common struct {
	fs       *flag.FlagSet
	origin   *string
	branch   *string
	src      *string
	out      *string
	jsonOut  *bool
	gitleaks *string
}

func newFlags(name string, stderr io.Writer) *common {
	fs := flag.NewFlagSet("relexport "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return &common{
		fs:       fs,
		origin:   fs.String("origin", "", "the internal repository to clone (default: this repository's origin remote)"),
		branch:   fs.String("branch", "main", "the branch of origin the source commit must be on"),
		src:      fs.String("src", "", "the internal source commit, as a full 40-hex id"),
		out:      fs.String("out", "", "directory to write export-report.json (and, for commit, the bundle) into"),
		jsonOut:  fs.Bool("json", false, "print the full JSON report on standard output"),
		gitleaks: fs.String("gitleaks", "", "path of the pinned gitleaks "+gitexport.GitleaksVersion+" binary (default: $RELEXPORT_GITLEAKS, then PATH)"),
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "usage: relexport tree|write|check|commit|record|verify|publish [flags]   (relexport <subcommand> -h for the flags)\n")
		return exitUsage
	}
	switch args[0] {
	case "tree":
		return cmdTree(args[1:], stdout, stderr)
	case "write":
		return cmdWrite(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "commit":
		return cmdCommit(args[1:], stdout, stderr)
	case "record":
		return cmdRecord(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stdout, stderr)
	case "publish":
		return cmdPublish(args[1:], stdout, stderr)
	}
	say(stderr, "relexport: unknown subcommand %q (want tree, write, check, commit, record, verify or publish)\n", args[0])
	return exitUsage
}

// parse parses the flags and opens the working clone.
func (c *common) parse(args []string, stderr io.Writer) (*gitexport.Repo, int) {
	if err := c.fs.Parse(args); err != nil {
		return nil, exitUsage
	}
	if c.fs.NArg() != 0 {
		say(stderr, "%s: unexpected argument %q\n", c.fs.Name(), c.fs.Arg(0))
		return nil, exitUsage
	}
	if *c.src == "" {
		say(stderr, "%s: --src is required\n", c.fs.Name())
		return nil, exitUsage
	}
	origin := *c.origin
	if origin == "" {
		out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
		origin = strings.TrimSpace(string(out))
		if err != nil || origin == "" {
			say(stderr, "%s: no --origin given and this directory has no origin remote\n", c.fs.Name())
			return nil, exitUsage
		}
	}
	repo, err := gitexport.Clone(origin, *c.branch)
	if err != nil {
		say(stderr, "%s: %v\n", c.fs.Name(), err)
		return nil, exitRefused
	}
	return repo, exitOK
}

// scanner resolves the secret scanner.
func (c *common) scanner() (gitexport.Scanner, error) {
	return resolveScanner(*c.gitleaks)
}

// resolveScanner returns the pinned gitleaks scanner. It is a variable only
// so that this command's own tests can run without the binary; nothing else
// assigns it.
var resolveScanner = pinnedScanner

func pinnedScanner(path string) (gitexport.Scanner, error) {
	if path == "" {
		path = os.Getenv("RELEXPORT_GITLEAKS")
	}
	if path == "" {
		found, err := exec.LookPath("gitleaks")
		if err != nil {
			return nil, fmt.Errorf("no gitleaks binary: pass --gitleaks, set RELEXPORT_GITLEAKS, or put gitleaks %s on PATH", gitexport.GitleaksVersion)
		}
		path = found
	}
	return gitexport.Gitleaks(path)
}

// report writes the JSON report where it was asked for.
func (c *common) report(v any, stdout, stderr io.Writer) bool {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		say(stderr, "%s: %v\n", c.fs.Name(), err)
		return false
	}
	data = append(data, '\n')
	if *c.out != "" {
		if err := os.MkdirAll(*c.out, 0o755); err != nil {
			say(stderr, "%s: %v\n", c.fs.Name(), err)
			return false
		}
		if err := os.WriteFile(filepath.Join(*c.out, "export-report.json"), data, 0o644); err != nil {
			say(stderr, "%s: %v\n", c.fs.Name(), err)
			return false
		}
	}
	if *c.jsonOut {
		_, _ = stdout.Write(data)
	}
	return true
}

// say writes one formatted message. A failed write to the terminal has
// nowhere to be reported, so it is dropped here, once, on purpose.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func fail(name string, err error, stderr io.Writer) int {
	say(stderr, "%s: %v\n", name, err)
	return exitRefused
}

func cmdTree(args []string, stdout, stderr io.Writer) int {
	c := newFlags("tree", stderr)
	version := c.fs.String("version", "", "record this vX.Y.Z in EXPORT-SOURCE (it changes the tree id)")
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	tr, err := repo.Tree(*c.src, *version)
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(tr, stdout, stderr) {
		return exitRefused
	}
	if !*c.jsonOut {
		say(stdout, "tree %s\n", tr.Tree)
	}
	say(stderr, "source %s: %d files; public tree: %d files\n", tr.Source, tr.FilesInSource, tr.FilesInTree)
	for _, r := range tr.Removed {
		say(stderr, "  removed %5d  %s\n", r.Files, r.Entry)
	}
	return exitOK
}

// cmdWrite writes the public tree of a source commit into a directory. It is
// `tree` plus the files: what a contributor would clone, with no .git.
func cmdWrite(args []string, stdout, stderr io.Writer) int {
	c := newFlags("write", stderr)
	version := c.fs.String("version", "", "record this vX.Y.Z in EXPORT-SOURCE (it changes the tree id)")
	dir := c.fs.String("dir", "", "the directory to write into: it must not exist yet, or be empty (required)")
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	if *dir == "" {
		say(stderr, "%s: --dir is required\n", c.fs.Name())
		return exitUsage
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	tr, err := repo.Tree(*c.src, *version)
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if err := repo.WriteTree(tr, abs); err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(tr, stdout, stderr) {
		return exitRefused
	}
	if !*c.jsonOut {
		say(stdout, "tree %s\ndir %s\n", tr.Tree, abs)
	}
	say(stderr, "write: %d files of the public tree of %s written; the directory hashes to the tree id. It was NOT checked by the guard or the scanner (relexport check does that)\n", tr.FilesInTree, tr.Source)
	return exitOK
}

// parentOrRoot holds the one rule the snapshot subcommands share: a snapshot
// is a declared root or has a named parent, never both and never neither.
func parentOrRoot(name string, root bool, parent string, stderr io.Writer) bool {
	if root == (parent != "") {
		say(stderr, "%s: give exactly one of --root (the first commit of a new public history) and --parent\n", name)
		return false
	}
	return true
}

func printSummary(chk *gitexport.CheckResult, stderr io.Writer) {
	say(stderr, "deny-pattern guard, per class (matches / files):\n")
	total := 0
	for _, s := range chk.Summary {
		say(stderr, "  %s  %6d / %d\n", s.Class, s.Matches, len(s.Files))
		total += s.Matches
	}
	say(stderr, "  total %d findings, %d problems\n", total, len(chk.Problems))
	if chk.ScannerRan {
		say(stderr, "secret scanner (X1): %d findings\n", len(chk.ScanFindings))
	} else {
		say(stderr, "secret scanner (X1): NOT RUN\n")
	}
	say(stderr, "public files that mention an internal-only path: %d\n", chk.DanglingRefs)
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	c := newFlags("check", stderr)
	version := c.fs.String("version", "", "record this vX.Y.Z in EXPORT-SOURCE")
	guardOnly := c.fs.Bool("guard-only", false, "run the deny-pattern guard without the secret scanner; exits 0 when the guard alone is clean, and says the scanner was not run")
	quiet := c.fs.Bool("quiet", false, "do not print one line per finding (the summary and the report still have them)")
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()

	opt := gitexport.CheckOptions{SkipScanner: *guardOnly}
	if !*guardOnly {
		s, err := c.scanner()
		if err != nil {
			return fail(c.fs.Name(), err, stderr)
		}
		opt.Scanner = s
	}
	tr, err := repo.Tree(*c.src, *version)
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	chk, err := repo.Check(tr, opt)
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(chk, stdout, stderr) {
		return exitRefused
	}
	if !*c.jsonOut {
		say(stdout, "tree %s\n", tr.Tree)
	}
	if !*quiet {
		say(stderr, "%s", chk.Refusal())
	}
	printSummary(chk, stderr)
	switch {
	case chk.OK:
		say(stderr, "check: PASS (guard and scanner)\n")
		return exitOK
	case *guardOnly && chk.GuardClean():
		say(stderr, "check: guard PASS; the secret scanner was NOT run, so this tree is NOT cleared for export\n")
		return exitOK
	}
	say(stderr, "check: REFUSED\n")
	return exitRefused
}

func cmdCommit(args []string, stdout, stderr io.Writer) int {
	c := newFlags("commit", stderr)
	version := c.fs.String("version", "", "the release version, vX.Y.Z (required)")
	parent := c.fs.String("parent", "", "the public commit the snapshot goes on top of, full 40-hex id")
	root := c.fs.Bool("root", false, "build a ROOT snapshot: the first commit of a new public history, with no parent")
	mirror := c.fs.String("mirror", "", "fetch the parent from this mirror (read-only); not needed when the parent is in origin's history, nor for a root")
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	if *version == "" {
		say(stderr, "%s: --version is required\n", c.fs.Name())
		return exitUsage
	}
	if !parentOrRoot(c.fs.Name(), *root, *parent, stderr) {
		return exitUsage
	}
	s, err := c.scanner()
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	res, err := repo.Commit(*c.src, gitexport.CommitOptions{
		Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, OutDir: *c.out,
		Check: gitexport.CheckOptions{Scanner: s},
	})
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(res, stdout, stderr) {
		return exitRefused
	}
	if !*c.jsonOut {
		say(stdout, "tree %s\ncommit %s\ntag %s\norigin-tag %s\n", res.Tree, res.Commit, res.Tag, res.OriginTag)
	}
	if res.Bundle != "" {
		say(stderr, "bundle %s\n", res.Bundle)
	}
	say(stderr, "commit: built in a scratch clone; NOTHING was pushed anywhere\n")
	return exitOK
}

const recordUsage = `
relexport record pushes the snapshot commit to ORIGIN (the internal
repository this clone is made from) as refs/export/<version>, so the object
the release review reads is addressable before anything is public. It cannot
write to a mirror. It refuses unless the snapshot passes everything commit
demands, origin is not the public host, and origin does not already hold the
ref at another commit. Without --confirm <the commit id> it pushes nothing
and exits 3.
`

func cmdRecord(args []string, stdout, stderr io.Writer) int {
	c := newFlags("record", stderr)
	version := c.fs.String("version", "", "the release version, vX.Y.Z (required)")
	parent := c.fs.String("parent", "", "the public commit the snapshot goes on top of, full 40-hex id")
	root := c.fs.Bool("root", false, "the snapshot is a ROOT: the first commit of a new public history")
	mirror := c.fs.String("mirror", "", "fetch the parent from this mirror (read-only); never written to")
	confirm := c.fs.String("confirm", "", "the exact id of the snapshot commit; without it this is a dry run")
	usage := c.fs.Usage
	c.fs.Usage = func() {
		usage()
		say(stderr, "%s", recordUsage)
	}
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	if *version == "" {
		say(stderr, "%s: --version is required\n", c.fs.Name())
		return exitUsage
	}
	if !parentOrRoot(c.fs.Name(), *root, *parent, stderr) {
		return exitUsage
	}
	s, err := c.scanner()
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	res, err := repo.Record(*c.src, gitexport.RecordOptions{
		Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, Confirm: *confirm,
		Check: gitexport.CheckOptions{Scanner: s},
	})
	var nc *gitexport.NotConfirmed
	if errors.As(err, &nc) {
		if !c.report(res, stdout, stderr) {
			return exitRefused
		}
		say(stdout, "would-push %s\nto %s\non %s\n", nc.Object, nc.Target, nc.Mirror)
		say(stderr, "%s: %v\n", c.fs.Name(), err)
		return exitDryRun
	}
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(res, stdout, stderr) {
		return exitRefused
	}
	if res.Already {
		say(stdout, "already-recorded %s\nat %s\non %s\n", res.Object, res.Ref, res.Origin)
		return exitOK
	}
	say(stdout, "pushed %s\nto %s\non %s\n", res.Object, res.Ref, res.Origin)
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	c := newFlags("verify", stderr)
	public := c.fs.String("public", "", "the published commit: a full commit id, refs/heads/main or refs/tags/<name> (required)")
	mirror := c.fs.String("mirror", "", "the mirror to read it from, read-only (required)")
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	if *public == "" || *mirror == "" {
		say(stderr, "%s: --public and --mirror are required\n", c.fs.Name())
		return exitUsage
	}
	res, err := repo.Verify(*c.src, *mirror, *public)
	if res != nil {
		if !c.report(res, stdout, stderr) {
			return exitRefused
		}
		if !*c.jsonOut {
			say(stdout, "public-commit %s\npublic-tree %s\nrecomputed-tree %s\n", res.PublicCommit, res.PublicTree, res.Recomputed)
		}
	}
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	say(stderr, "verify: EQUAL -- %s is the export of %s\n", res.PublicCommit, res.Source)
	return exitOK
}

const publishUsage = `
relexport publish writes to the public mirror. It is the ONLY subcommand that
can, and it pushes exactly one object to exactly one ref. It refuses unless:

  - --mirror is given (there is no default), and, when the mirror is not a
    local directory, the environment has ` + gitexport.PublishEnv + `=<the version>;
  - the public tree passes the guard and the secret scanner, and the commit
    and tag messages pass the guard;
  - the computed tree is --reviewed-tree (what the release review recorded);
  - EITHER --root is given and the mirror is EMPTY (no branch, no tag, no ref
    at all): the snapshot is the first commit of a new public history. A
    mirror that already holds an export refuses a root;
    OR --parent and --prev-src are given, the mirror's main is still --parent
    (nobody pushed or merged there), and --parent is, tree for tree, the
    export of --prev-src. No parent is trusted on any other ground;
  - --confirm is the exact id of the object to push.

Without --confirm it does everything else, pushes nothing, prints the object
it would push, and exits 3. It never uses --force.
`

func cmdPublish(args []string, stdout, stderr io.Writer) int {
	c := newFlags("publish", stderr)
	version := c.fs.String("version", "", "the release version, vX.Y.Z (required)")
	parent := c.fs.String("parent", "", "the public commit the snapshot goes on top of (with --prev-src)")
	root := c.fs.Bool("root", false, "publish a ROOT snapshot to an EMPTY mirror: the first commit of a new public history")
	mirror := c.fs.String("mirror", "", "the mirror to publish to (required; no default)")
	reviewed := c.fs.String("reviewed-tree", "", "the tree id the release review recorded (required)")
	prev := c.fs.String("prev-src", "", "the previous release's internal source commit (required with --parent; never with --root)")
	ref := c.fs.String("ref", "main", "what to push: main (the snapshot commit) or tag (the tag object, after main)")
	confirm := c.fs.String("confirm", "", "the exact id of the object to push; without it this is a dry run")
	usage := c.fs.Usage
	c.fs.Usage = func() {
		usage()
		say(stderr, "%s", publishUsage)
	}
	repo, code := c.parse(args, stderr)
	if repo == nil {
		return code
	}
	defer repo.Close()
	if *version == "" || *mirror == "" || *reviewed == "" {
		say(stderr, "%s: --version, --mirror and --reviewed-tree are required\n", c.fs.Name())
		return exitUsage
	}
	if !parentOrRoot(c.fs.Name(), *root, *parent, stderr) {
		return exitUsage
	}
	s, err := c.scanner()
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	res, err := repo.Publish(*c.src, gitexport.PublishOptions{
		Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, ReviewedTree: *reviewed,
		PrevSource: *prev, Ref: *ref, Confirm: *confirm,
		Check: gitexport.CheckOptions{Scanner: s},
	})
	var nc *gitexport.NotConfirmed
	if errors.As(err, &nc) {
		if !c.report(res, stdout, stderr) {
			return exitRefused
		}
		say(stdout, "would-push %s\nto %s\non %s\n", nc.Object, nc.Target, nc.Mirror)
		say(stderr, "%s: %v\n", c.fs.Name(), err)
		return exitDryRun
	}
	if err != nil {
		return fail(c.fs.Name(), err, stderr)
	}
	if !c.report(res, stdout, stderr) {
		return exitRefused
	}
	say(stdout, "pushed %s\nto %s\non %s\n", res.Object, map[string]string{"main": "refs/heads/main", "tag": "refs/tags/" + *version}[*ref], res.Mirror)
	return exitOK
}
