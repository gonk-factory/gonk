package integration_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// ---------------------------------------------------------------- .gonk.yml builder

// gonkOpts configures buildGonkYML.
type gonkOpts struct {
	ladder     []string
	budgetUSD  *float64
	triage     bool
	quietHours string
	timezone   string
}

// projectOption tunes a project's .gonk.yml.
type projectOption func(*gonkOpts)

func withLadder(rungs ...string) projectOption {
	return func(o *gonkOpts) { o.ladder = rungs }
}

func withBudget(usd float64) projectOption {
	return func(o *gonkOpts) { o.budgetUSD = &usd }
}

func withQuietHours(window, tz string) projectOption {
	return func(o *gonkOpts) { o.quietHours, o.timezone = window, tz }
}

// buildGonkYML renders a minimal, valid .gonk.yml. An unset budget means
// UNLIMITED (gonkcfg's sentinels) -- which is what the ladder tests use so that
// escalation is affordable and no spend-staleness gate fires while the clock is
// advanced.
func buildGonkYML(o gonkOpts) string {
	var b strings.Builder
	b.WriteString("version: 1\nenabled: true\n")
	b.WriteString("actions: { triage: " + boolStr(o.triage) + " }\n")
	if len(o.ladder) > 0 {
		b.WriteString("ladder: [" + strings.Join(o.ladder, ", ") + "]\n")
	}
	if o.budgetUSD != nil {
		b.WriteString("budget: { monthly_cost_usd: " + strconv.FormatFloat(*o.budgetUSD, 'f', -1, 64) + " }\n")
	}
	if o.quietHours != "" {
		b.WriteString("schedule: { quiet_hours: \"" + o.quietHours + "\", timezone: \"" + o.timezone + "\" }\n")
	}
	return b.String()
}

// onboard registers a project THROUGH THE REAL INTAKE->METER SEAM: it seeds a
// .gonk.yml (and a .agent/ directory, so the project lands `valid` rather than
// `pending` and no scaffold auto-fires), then runs one reconcile pass. It sets
// the World's current project so w.Session binds to it.
func onboard(t *testing.T, w *World, path string, opts ...projectOption) *glabtest.Project {
	t.Helper()
	o := gonkOpts{ladder: []string{"qwen-local", "glm", "sonnet"}, triage: true}
	for _, opt := range opts {
		opt(&o)
	}
	p := w.GitLab.AddProject(path, glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte(buildGonkYML(o)))
	p.PutFile(".agent/context.md", []byte("# context\n"))
	w.Reconcile(t)
	w.project, w.rig = path, rigFor(path)
	return p
}

// ---------------------------------------------------------------- assertions

func requireMeterState(t *testing.T, w *World, project string, want meterapi.State) {
	t.Helper()
	if got := w.Meter.State(t, project); got != want {
		t.Fatalf("meter state for %q = %q, want %q", project, got, want)
	}
}

func requireIntakeState(t *testing.T, w *World, project string, want intake.State) {
	t.Helper()
	for _, id := range w.intakeCacheIDs() {
		e, ok := w.cache.Get(id)
		if !ok || e.Project.PathWithNamespace != project {
			continue
		}
		if e.Classification.State != want {
			t.Fatalf("intake state for %q = %q, want %q", project, e.Classification.State, want)
		}
		return
	}
	t.Fatalf("intake has no cache entry for %q (want state %q)", project, want)
}

func (w *World) intakeCacheIDs() []int64 { return w.cache.IDs() }

// requireOneMR asserts exactly one MR exists on sourceBranch and returns it.
func requireOneMR(t *testing.T, w *World, projectID int64, sourceBranch string) glab.MergeRequest {
	t.Helper()
	mrs, err := w.GitLab.Client().ListMergeRequests(t.Context(), projectID, glab.MRListOptions{
		SourceBranch: sourceBranch, State: "all",
	})
	if err != nil {
		t.Fatalf("list MRs: %v", err)
	}
	if len(mrs) != 1 {
		t.Fatalf("want exactly one MR on %q, got %d", sourceBranch, len(mrs))
	}
	return mrs[0]
}

// mergeOnboardingMR merges the onboarding MR and lands its .gonk.yml on the
// default branch (glabtest does not copy branch files on merge, so the harness
// writes the committed bytes onto main, exactly as a real merge would).
func mergeOnboardingMR(t *testing.T, w *World, p *glabtest.Project, mr glab.MergeRequest, ladder []string) {
	t.Helper()
	cfg, err := intake.RenderDefaultConfig(ladder)
	if err != nil {
		t.Fatalf("render default config: %v", err)
	}
	p.PutFile(".gonk.yml", cfg)
	w.GitLab.SetMRState(p.ID, mr.IID, "merged", baseClock)
}

func requireAlive(t *testing.T, w *World) {
	t.Helper()
	// Both processes still answer /healthz after every hostile input.
	if code := w.Meter.do(t, "GET", meterapi.HealthzPath, nil, nil); code != 200 {
		t.Fatalf("meter /healthz = %d after a hostile input", code)
	}
	resp, err := w.httpGet(w.intakeURL + "/healthz")
	if err != nil || resp != 200 {
		t.Fatalf("intake /healthz = %d (err %v) after a hostile input", resp, err)
	}
}

// ---------------------------------------------------------------- stub scripts

// successScript makes every model call return a tool call plus the given usage.
// The prose is what an AD-4 run varies; it lands only in the (ledger-irrelevant)
// response body, never in an assertion.
func successScript(prose string, prompt, completion int) []stubmodel.Step {
	return []stubmodel.Step{{
		Repeat: stubmodel.Forever,
		Response: stubmodel.Response{
			Content:   prose,
			ToolCalls: []stubmodel.ToolCall{{ID: "call-1", Name: "post_comment", Arguments: `{"body":"triaged"}`}},
		},
		Usage: stubmodel.Usage{PromptTokens: prompt, CompletionTokens: completion},
	}}
}

// httpGet does a bare GET and returns the status code.
func (w *World) httpGet(url string) (int, error) {
	resp, err := http.Get(url) //nolint:gosec // test-local httptest URL
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
