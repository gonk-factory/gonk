package pipelinectx

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeRules(t *testing.T) {
	cases := []struct {
		name string // the rule; a break of exactly that rule turns this red
		in   string
		want []string
	}{
		{"ansi CSI colour stripped", "\x1b[31;1mERROR\x1b[0;m done", []string{"ERROR done"}},
		{"ansi erase-line stripped", "\x1b[0Kplain", []string{"plain"}},
		{"ansi OSC (BEL) stripped", "\x1b]0;window title\x07after", []string{"after"}},
		{"ansi OSC (ST) stripped", "\x1b]8;;http://x\x1b\\link", []string{"link"}},
		{"section_start marker stripped", "section_start:1700000000:build_step\r\x1b[0KBuilding", []string{"Building"}},
		{"section_start with options stripped", "section_start:1700000000:build[collapsed=true]\r\x1b[0Kx", []string{"x"}},
		{"section_end marker stripped", "a\nsection_end:1700000000:build_step\r\x1b[0K\nb", []string{"a", "", "b"}},
		{"CR overwrite keeps last", "10%\r50%\r100%", []string{"100%"}},
		{"CRLF is a line end, not an overwrite", "one\r\ntwo\r\n", []string{"one", "two"}},
		{"CR progress then CRLF", "p 1\rp 2\r\n", []string{"p 2"}},
		{"control bytes dropped, tab kept", "a\x00b\x07c\td", []string{"abc\td"}},
		{"invalid utf-8 replaced", "ok \xff\xfe end", []string{"ok \uFFFD end"}},
		{"trailing whitespace trimmed", "line   \t \nnext", []string{"line", "next"}},
		{"trailing empty lines dropped", "x\n\n\n", []string{"x"}},
		{"3 repeats kept", "r\nr\nr\nz", []string{"r", "r", "r", "z"}},
		{"4 repeats collapsed", "r\nr\nr\nr\nz", []string{"r", RepeatMarker(3), "z"}},
		{"10 repeats collapsed", strings.Repeat("r\n", 10) + "z", []string{"r", RepeatMarker(9), "z"}},
		{"repeats compared after CR resolution", "a\rr\nb\rr\nr\nr", []string{"r", RepeatMarker(3)}},
		// GitLab's timestamped-trace line prefix (gonk-mf3v). Synthetic lines in
		// the shape of the real trace in testdata.
		{"trace prefix stripped (stdout)", "2026-01-02T03:04:05.123456Z 00O hello", []string{"hello"}},
		{"trace prefix stripped (stderr stream, hex stream number)", "2026-01-02T03:04:05.123456Z 0aE oops", []string{"oops"}},
		{"trace prefix stripped (no fractional seconds)", "2026-01-02T03:04:05Z 01O whole second", []string{"whole second"}},
		{"trace prefix stripped (nanoseconds)", "2026-01-02T03:04:05.123456789Z 01O nanos", []string{"nanos"}},
		{"trace prefix on an empty line", "2026-01-02T03:04:05.1Z 01O a\n2026-01-02T03:04:05.2Z 01O \n2026-01-02T03:04:05.3Z 01O b", []string{"a", "", "b"}},
		{"trace continuation joins the previous line",
			"2026-01-02T03:04:05.1Z 00O first half,\n2026-01-02T03:04:05.2Z 00O+ second half\n2026-01-02T03:04:05.3Z 00O next",
			[]string{"first half, second half", "next"}},
		{"two trace continuations join one line",
			"2026-01-02T03:04:05.1Z 00O a\n2026-01-02T03:04:05.2Z 00O+b\n2026-01-02T03:04:05.3Z 00O+c\n", []string{"abc"}},
		{"trace continuation with no line before it stays a line", "2026-01-02T03:04:05.1Z 00O+tail\n2026-01-02T03:04:05.2Z 00O x", []string{"tail", "x"}},
		{"section marker and ANSI after the trace prefix",
			"2026-01-02T03:04:05.1Z 00O section_start:1700000000:step_script\r\x1b[0K\n2026-01-02T03:04:05.2Z 00O+\x1b[0K\x1b[36;1mRunning step\x1b[0;m\n2026-01-02T03:04:05.3Z 01O \x1b[32;1m$ make\x1b[0;m",
			[]string{"Running step", "$ make"}},
		{"CRLF after the trace prefix", "2026-01-02T03:04:05.1Z 01O one\r\n2026-01-02T03:04:05.2Z 01O two\r\n", []string{"one", "two"}},
		{"trace prefix only at the start of a line", "see 2026-01-02T03:04:05.1Z 00O here", []string{"see 2026-01-02T03:04:05.1Z 00O here"}},
		{"a log line that starts with a timestamp is not a trace prefix",
			"2026-01-02T03:04:05Z widgetd: frob\n2026-01-02T03:04:05Z 00 O x\n2026-01-02T03:04:05Z 00Ox",
			[]string{"2026-01-02T03:04:05Z widgetd: frob", "2026-01-02T03:04:05Z 00 O x", "2026-01-02T03:04:05Z 00Ox"}},
		{"prefixed and plain lines mixed", "plain\n2026-01-02T03:04:05.1Z 00O stamped\n+ not a continuation", []string{"plain", "stamped", "+ not a continuation"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Normalize([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Normalize(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeGitLabFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitlab_sections.log")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Running with gitlab-runner 17.0.0 (abcdef12)",
		`Preparing the "docker" executor`,
		"Preparing environment",
		"Running on runner-synthetic-project-1-concurrent-0 via synthetic-host...",
		"",
		`Executing "step_script" stage of the job script`,
		"$ make build",
		"fetching deps 100%",
		"compiling widget",
		RepeatMarker(4),
		"./src/widget.go:12:3: undefined: FrobnicateAll",
		"make: *** [Makefile:7: build] Error 1",
		"",
		"ERROR: Job failed: exit code 2",
	}
	got := Normalize(raw)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, l := range got {
		if strings.ContainsAny(l, "\x1b\r") || strings.Contains(l, "section_") {
			t.Fatalf("residue in %q", l)
		}
	}
}

// realTraceFixture is a REAL job trace, as GET jobs/:id/trace returned it
// (gonk-mf3v): job 28617 of the synthetic e2e project 75, whose whole script is
// `M=GONK_SMOKE_DELIBERATE; echo "${M}_FAILURE <stamp>"; exit 3`. P1's fixtures
// were written by hand and none carried the per-line prefix every real trace
// has; this one is byte for byte what GitLab sent.
const realTraceFixture = "testdata/gitlab_real_trace_project75_job28617.log"

// tracePrefixResidueRe: a timestamped-trace prefix that survived, anywhere.
var tracePrefixResidueRe = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z [0-9a-f]{2}[OE]`)

func TestNormalizeRealGitLabTrace(t *testing.T) {
	raw, err := os.ReadFile(realTraceFixture)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tracePrefixResidueRe.FindAll(raw, -1)); n != 47 {
		t.Fatalf("the fixture holds %d timestamped lines, want 47: it is not the real trace any more", n)
	}
	const pod = "Waiting for pod gitlab/runner-sgtn0ede3-project-75-concurrent-0-z56dy5pc to be running on the node node-2, status is Pending"
	const notReady = "\tContainersNotReady: \"containers with unready status: [build helper]\""
	want := []string{
		"Running with gitlab-runner 19.4.0 (ac11717a)",
		"  on gitlab-gitlab-runner-696fb7b645-rcfqb sgTN0Ede3, system ID: r_Wt9gEt407YWF",
		"  UUID: 01a10391-112b-7dcd-a54d-f508f9100fd5",
		// section_start, then a '+' continuation carrying the header: ONE line.
		`Preparing the "kubernetes" executor`,
		"Using default image",
		"Using Kubernetes namespace: gitlab",
		"Using Kubernetes executor with image ubuntu:24.04 ...",
		"Using attach strategy to execute scripts...",
		"Using effective pull policy of [] for container build",
		"Using effective pull policy of [] for container helper",
		"Using effective pull policy of [] for container init-permissions",
		// section_end + section_start + header: three physical lines, ONE line.
		"Preparing environment",
		"Using FF_USE_POD_ACTIVE_DEADLINE_SECONDS, the Pod activeDeadlineSeconds will be set to the job timeout: 1h0m0s...",
		pod, pod, // stderr stream ("00E")
		notReady, notReady,
		"Running on runner-sgtn0ede3-project-75-concurrent-0-z56dy5pc via gitlab-gitlab-runner-696fb7b645-rcfqb...",
		"",
		"Getting source from Git repository",
		"Gitaly correlation ID: 01M44CX80V4RGQ785V8ZJ2KER3",
		"Fetching changes with git depth set to 20...",
		"Initialized empty Git repository in /builds/agentic/gonk-e2e-1784441480/.git/",
		"Created fresh repository.",
		"Checking out 4520721a as detached HEAD (ref is refs/merge-requests/3/head)...",
		"",
		"Skipping Git submodules setup",
		"",
		`Executing "step_script" stage of the job script`,
		"$ M=GONK_SMOKE_DELIBERATE",
		`$ echo "${M}_FAILURE 20261004T212509Z"`,
		"GONK_SMOKE_DELIBERATE_FAILURE 20261004T212509Z",
		"$ exit 3",
		"",
		"Cleaning up project directory and file based variables",
		"",
		"",
		"ERROR: Job failed: command terminated with exit code 3",
	}
	got := Normalize(raw)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, l := range got {
		if strings.ContainsAny(l, "\x1b\r") || strings.Contains(l, "section_") || tracePrefixResidueRe.MatchString(l) {
			t.Fatalf("residue in %q", l)
		}
	}
}
