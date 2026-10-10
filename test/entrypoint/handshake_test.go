// The session-contract handshake and the mounted checkout URL (gonk-t55 plan
// section 3.3, "handshake" items 2 and 4), against the real script.
package entrypoint

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	gonkruntime "github.com/gonk-factory/gonk/pkg/runtime"
)

// supportedLine is the ONE line of entrypoint.sh that declares the contract
// versions the image serves. The script's comment says to keep this shape.
var supportedLine = regexp.MustCompile(`(?m)^GONK_SUPPORTED_SESSION_CONTRACTS="([0-9 ]*)"$`)

func supportedContracts(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(entrypointPath(t))
	if err != nil {
		t.Fatal(err)
	}
	m := supportedLine.FindAllStringSubmatch(string(b), -1)
	if len(m) != 1 {
		t.Fatalf("entrypoint.sh has %d GONK_SUPPORTED_SESSION_CONTRACTS=\"...\" lines, want exactly 1 "+
			"(the shape this test reads; see the comment above it)", len(m))
	}
	return strings.Fields(m[0][1])
}

// THE TWO SIDES CANNOT DRIFT SILENTLY. The controller's contract version is a
// Go constant; the versions the agent image serves are a line of shell. A bump
// of one without the other would make EVERY session exit 8 -- parked, never
// charged, and never running. This fails the commit that does that instead.
func TestTheEntrypointSupportsTheControllersSessionContract(t *testing.T) {
	want := strconv.Itoa(gonkruntime.SessionContract)
	got := supportedContracts(t)
	for _, v := range got {
		if v == want {
			return
		}
	}
	t.Fatalf("images/agent/entrypoint.sh supports session contracts %v, but pkg/runtime.SessionContract is %s: "+
		"bump both in the same commit", got, want)
}

// HANDSHAKE ITEM 2: jobs mode with an unset or unsupported contract exits with
// the DEDICATED code before touching anything -- not the session files (their
// absence would otherwise be exit 6), not the checkout, not opencode -- and
// logs both versions in the line the sweep quotes.
func TestJobsModeRefusesAnUnsetOrUnsupportedSessionContract(t *testing.T) {
	supported := strings.Join(supportedContracts(t), " ")
	cases := map[string]struct {
		value    *string // nil: unset
		wantSent string
	}{
		"unset":         {nil, "unset"},
		"empty":         {ptr(""), "unset"},
		"newer":         {ptr(strconv.Itoa(gonkruntime.SessionContract + 1)), strconv.Itoa(gonkruntime.SessionContract + 1)},
		"older":         {ptr("0"), "0"},
		"padded":        {ptr(" " + strconv.Itoa(gonkruntime.SessionContract)), " " + strconv.Itoa(gonkruntime.SessionContract)},
		"not a version": {ptr("jobs"), "jobs"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newJobsSession(t)
			s.writeRigURL(t, "https://intake.invalid/rig/x.tar.gz")
			// Break the session files too: the contract is checked FIRST, so
			// this must still be the contract's exit, never 6.
			if err := os.Remove(filepath.Join(s.sessionDir, "prompt")); err != nil {
				t.Fatal(err)
			}
			env := withoutEnv(s.env(t), gonkruntime.EnvSessionContract)
			if c.value != nil {
				env = append(env, gonkruntime.EnvSessionContract+"="+*c.value)
			}
			out, code := runJobs(t, env)
			if code != gonkruntime.ExitSessionContractMismatch {
				t.Fatalf("exit %d, want %d (session contract mismatch):\n%s", code, gonkruntime.ExitSessionContractMismatch, out)
			}
			wantLine := "session contract mismatch: sent=" + c.wantSent + " supported=[" + supported + "]"
			if !strings.Contains(out, wantLine) {
				t.Errorf("output lacks %q (both versions, in the shape the sweep quotes):\n%s", wantLine, out)
			}
			if !strings.Contains(out, "session end: refused (session contract mismatch)") {
				t.Errorf("no session-end refusal line:\n%s", out)
			}
			if strings.Contains(out, "session file") {
				t.Errorf("the session files were examined before the contract was checked:\n%s", out)
			}
			if calls := s.curlCalls(t); len(calls) != 0 {
				t.Errorf("curl was called %q; a skewed session must fetch nothing", calls)
			}
			for _, sub := range []string{"models", "run"} {
				if _, err := os.Stat(s.ocArgv + "." + sub); err == nil {
					t.Errorf("opencode %s was invoked despite the refusal", sub)
				}
			}
			if _, err := os.Stat(s.overlay); err == nil {
				t.Errorf("an overlay was rendered despite the refusal")
			}
		})
	}
}

// The supported contract runs, and says which one it runs under.
func TestJobsModeRunsTheSupportedSessionContract(t *testing.T) {
	s := newJobsSession(t)
	out, code := runJobs(t, s.env(t))
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "session contract: "+strconv.Itoa(gonkruntime.SessionContract)+" (supported: ") {
		t.Errorf("no session-contract line:\n%s", out)
	}
}

// HANDSHAKE ITEM 4: with no rig-url mounted there is no checkout -- a
// GONK_RIG_URL in env (what the controller set before this change) is NOT a
// fallback in jobs mode.
func TestJobsModeIgnoresARigURLInEnv(t *testing.T) {
	s := newJobsSession(t)
	out, code := runJobs(t, s.env(t, "GONK_RIG_URL="+trapRigURL))
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if calls := s.curlCalls(t); len(calls) != 0 {
		t.Fatalf("curl calls = %q; with no rig-url mounted, jobs mode fetches no checkout", calls)
	}
	for _, want := range []string{"expecting: checkout=no prompt=mounted", "no checkout for this session"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// An EMPTY rig-url is a session without a working copy, said out loud; it is
// not fatal (every checkout failure is non-fatal by design).
func TestJobsModeTreatsAnEmptyRigURLAsNoCheckout(t *testing.T) {
	s := newJobsSession(t)
	s.writeRigURL(t, "\n")
	out, code := runJobs(t, s.env(t))
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if calls := s.curlCalls(t); len(calls) != 0 {
		t.Fatalf("curl calls = %q for an empty rig-url", calls)
	}
	if !strings.Contains(out, "rig-url is empty; running without a working copy") {
		t.Errorf("no warning for the empty rig-url:\n%s", out)
	}
}

func ptr(s string) *string { return &s }

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}
