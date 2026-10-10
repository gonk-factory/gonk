package changejudge

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/gate"
)

// ---- the rules page's tables and this package's fixtures agree ----
//
// The tables are the product's rules and live in the reference, which is
// published with the code (they used to be sections 2.1, 2.5 and 8.1 of a
// build plan, which is not). The subtests keep the names they had.

const rulesFile = "../../docs/reference/gonk-change-rules.md"

// The headings of the sections this package reads.
const (
	rulesTrigger     = "## 1. Who may start an issue change"
	rulesEligibility = "## 2. Eligibility (I rows)"
	rulesDone        = "## 3. \"Done\" when there is no failing job to turn green"
)

// rulesSection returns the body of the section of the rules page whose
// heading line is `heading`, up to the next "## " heading.
func rulesSection(t *testing.T, heading string) string {
	t.Helper()
	b, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatalf("read the rules page: %v", err)
	}
	text := string(b)
	i := strings.Index(text, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("%s has no heading %q: its layout changed, re-read it", rulesFile, heading)
	}
	text = text[i+1+len(heading):]
	j := strings.Index(text, "\n## ")
	if j < 0 {
		t.Fatalf("%s: no heading follows %q: its layout changed, re-read it", rulesFile, heading)
	}
	return text[:j]
}

// planIRows: the first-column codes of the eligibility table.
func planIRows(t *testing.T) []string {
	t.Helper()
	var rows []string
	for _, m := range regexp.MustCompile(`(?m)^\| (I\d+) \|`).FindAllStringSubmatch(rulesSection(t, rulesEligibility), -1) {
		rows = append(rows, m[1])
	}
	return rows
}

// planKTokens: every outcome the K block names, as "<step>:<token>", in the
// block's order and with repeats.
func planKTokens(t *testing.T) []string {
	t.Helper()
	text := rulesSection(t, rulesDone)
	start := strings.Index(text, "```\nK0 identity")
	if start < 0 {
		t.Fatalf("the rules page has no K block: its section 3 changed, re-read it")
	}
	block := text[start+4:]
	end := strings.Index(block, "```")
	if end < 0 {
		t.Fatalf("the K block is not closed")
	}
	step := regexp.MustCompile(`^(K\d) `)
	token := regexp.MustCompile(`(?:-> |, else )([A-Za-z_]+(?:-[a-z]+)*(?:\([a-z-]+\))?)`)
	var out []string
	cur := ""
	for _, line := range strings.Split(block[:end], "\n") {
		if m := step.FindStringSubmatch(line); m != nil {
			cur = m[1]
		}
		for _, m := range token.FindAllStringSubmatch(line, -1) {
			out = append(out, cur+":"+m[1])
		}
	}
	return out
}

func allJudgeCases() []judgeCase {
	var all []judgeCase
	for _, cs := range [][]judgeCase{k0Cases, k1Cases, k2Cases, k3Cases, k4Cases, k5Cases} {
		all = append(all, cs...)
	}
	return all
}

// fixtureIsFor: does the fixture's EXPECTATION say what the plan's token
// says? A tag on a fixture that asserts something else is not coverage.
func fixtureIsFor(c judgeCase, key string) bool {
	step, token, _ := strings.Cut(key, ":")
	bare := strings.TrimSuffix(token, "(j)")
	switch {
	case step == "K0":
		return c.decision == DecisionStopped && string(c.state) == token
	case key == "K1:wait":
		return c.decision == DecisionWait && c.rule == RuleK1Wait
	case key == "K5:UNVERIFIABLE(tests-reduced)":
		return c.verdict == VerdictUnverifiable && c.rule == RuleK4Reduced
	case key == "K5:GREEN_UNOBSERVED":
		return c.verdict == VerdictGreenUnobserved && c.rule == RuleK5Unobserved
	case step == "K5":
		return c.decision == DecisionVerdict && string(c.verdict) == token
	case step == "K4":
		return strings.HasPrefix(string(c.tests), "K4."+bare)
	}
	return strings.HasPrefix(string(c.rule), step+"."+bare)
}

func TestEveryPlanRowHasAFixture(t *testing.T) {
	t.Run("2.1 eligibility rows I1-I8", func(t *testing.T) {
		want := planIRows(t)
		if len(want) != 8 {
			t.Fatalf("the rules page lists %d I rows (%v), expected 8: its section 2 changed, re-read it", len(want), want)
		}
		have := map[string]bool{}
		for _, c := range eligibleRows {
			have[c.row] = true
			if !strings.HasPrefix(string(c.code), c.row+".") {
				t.Errorf("fixture %q: code %q is not of row %s", c.name, c.code, c.row)
			}
		}
		var got []string
		for r := range have {
			got = append(got, r)
		}
		sort.Strings(got)
		sorted := append([]string(nil), want...)
		sort.Strings(sorted)
		if !reflect.DeepEqual(got, sorted) {
			t.Errorf("rows with a fixture: %v\nrows in the plan:     %v", got, sorted)
		}
	})

	t.Run("2.1 the trigger is the owner's decision 8.1 Q2", func(t *testing.T) {
		decided := strings.Join(strings.Fields(rulesSection(t, rulesTrigger)), " ")
		for _, phrase := range []string{
			"THREE trigger forms",
			"project OWNER or MAINTAINER",
			"the implement label applied by such a member",
			"a slash-style command in an issue note by such a member",
			"an at-mention of the bot asking for implementation by such a member",
			"The triage-verdict trigger stays out of v1",
		} {
			if !strings.Contains(decided, phrase) {
				t.Errorf("the rules page's section 1 no longer says %q: the owner's decision changed, re-read it", phrase)
			}
		}
		// The three forms, written out: Forms() is compared with the
		// decision, not with itself.
		owners := []Form{"label", "command", "mention"}
		if !reflect.DeepEqual(Forms(), owners) {
			t.Fatalf("Forms() = %v; the owner decided %v", Forms(), owners)
		}
		// One fixture per form, refused for the actor and for nothing else.
		forms := map[string]bool{}
		for _, c := range eligibleRows {
			if c.code != I2ActorCannotRequest {
				continue
			}
			for _, form := range owners {
				if strings.Contains(c.text, "`"+string(form)+"` request") || (form == FormLabel && strings.Contains(c.name, "label")) {
					forms[string(form)] = true
				}
			}
		}
		if len(forms) != len(owners) {
			t.Errorf("forms with a refused-actor fixture: %v; want every one of %v", forms, owners)
		}
	})

	t.Run("2.5 the K block K0-K5", func(t *testing.T) {
		want := planKTokens(t)
		if len(want) != 22 {
			t.Fatalf("the K block names %d outcomes (%v), expected 22: section 3 of the rules page changed, re-read it", len(want), want)
		}
		steps := map[string]bool{}
		need := map[string]int{}
		for _, k := range want {
			need[k]++
			steps[k[:2]] = true
		}
		if len(steps) != 6 {
			t.Fatalf("the K block has steps %v, expected K0-K5", steps)
		}
		// have: per token, the distinct rules its fixtures assert. A token the
		// plan names twice (NEW_NOT_RUN) needs two fixtures of different rules.
		have := map[string]map[Rule]bool{}
		for _, c := range allJudgeCases() {
			for _, key := range c.plan {
				if need[key] == 0 {
					t.Errorf("fixture %q is tagged %q, which the plan's K block does not name", c.name, key)
					continue
				}
				if !fixtureIsFor(c, key) {
					t.Errorf("fixture %q is tagged %q and asserts decision=%q verdict=%q state=%q rule=%q tests=%q",
						c.name, key, c.decision, c.verdict, c.state, c.rule, c.tests)
					continue
				}
				if have[key] == nil {
					have[key] = map[Rule]bool{}
				}
				rule := c.rule
				if strings.HasPrefix(key, "K4:") {
					rule = c.tests
				}
				have[key][rule] = true
			}
		}
		for key, n := range need {
			if len(have[key]) < n {
				t.Errorf("the plan names %s %d time(s); fixtures tagged for it assert %d distinct rule(s)", key, n, len(have[key]))
			}
		}
	})

	t.Run("2.5 the outcome table", func(t *testing.T) {
		text := rulesSection(t, rulesDone)
		header := "| Verdict | State | Meter | Escalates | MR label |"
		i := strings.Index(text, header)
		if i < 0 {
			t.Fatalf("the rules page has no outcome table: its section 3 changed, re-read it")
		}
		lines := strings.Split(text[i:], "\n")[2:] // past the header and its rule
		ticked := regexp.MustCompile("`([^`]+)`")
		upper := regexp.MustCompile(`[A-Z][A-Z_]+`)
		seen := map[Verdict]bool{}
		rows, callerRows := 0, 0
		for _, line := range lines {
			if !strings.HasPrefix(line, "|") {
				break
			}
			rows++
			cells := strings.Split(strings.Trim(line, "|"), "|")
			if len(cells) != 5 {
				t.Fatalf("row %q has %d cells", line, len(cells))
			}
			for j := range cells {
				cells[j] = strings.TrimSpace(cells[j])
			}
			meter := strings.Fields(cells[2])[0]
			escalates := strings.HasPrefix(cells[3], "yes")
			if cells[1] == "--" {
				// A row with no state: a signal the CALLER sets on
				// gate.Signals. Asserted here so the table stays whole.
				callerRows++
				sig := gate.Signals{ModelTokens: 1000}
				if strings.HasPrefix(cells[0], "ours") {
					sig.ArtifactUnknown = true
				}
				if got := gate.Classify(sig); got != meter || gate.Escalates(got) != escalates {
					t.Errorf("row %q: gate.Classify(%+v) = %q, escalates %v", cells[0], sig, got, gate.Escalates(got))
				}
				continue
			}
			m := ticked.FindStringSubmatch(cells[1])
			if m == nil {
				t.Fatalf("row %q: no state", line)
			}
			state := State(m[1])
			if !KnownState(state) {
				t.Errorf("row %q: state %q is not one of States()", cells[0], state)
			}
			out, reported := MeterOutcome(state, 1000)
			if !reported || out != meter || gate.Escalates(out) != escalates {
				t.Errorf("row %q: MeterOutcome(%q) = %q (reported %v), escalates %v; the plan says %q, escalates %v",
					cells[0], state, out, reported, gate.Escalates(out), meter, escalates)
			}
			for _, word := range upper.FindAllString(cells[0], -1) {
				v := Verdict(word)
				if _, isVerdict := StateFor(v); !isVerdict {
					t.Errorf("row %q names %q, which is not a verdict", cells[0], word)
					continue
				}
				seen[v] = true
				if st, _ := StateFor(v); st != state {
					t.Errorf("row %q: StateFor(%q) = %q, the plan says %q", cells[0], v, st, state)
				}
			}
			var mr []string
			needsHuman := false
			for _, l := range ticked.FindAllStringSubmatch(cells[4], -1) {
				if l[1] == LabelNeedsHuman {
					needsHuman = true
					continue
				}
				mr = append(mr, l[1])
			}
			got := LabelsFor(state, TestsObserved)
			if !reflect.DeepEqual(got.MR, mr) || got.IssueNeedsHuman != needsHuman {
				t.Errorf("row %q: LabelsFor(%q) = %+v; the plan says MR %v, issue needs-human %v", cells[0], state, got, mr, needsHuman)
			}
		}
		if rows != 7 || callerRows != 2 {
			t.Fatalf("the outcome table has %d rows, %d without a state; expected 7 and 2: section 2.5 changed, re-read it", rows, callerRows)
		}
		for _, v := range Verdicts() {
			if !seen[v] {
				t.Errorf("verdict %q is in no row of the plan's outcome table", v)
			}
		}
		if len(seen) != len(Verdicts()) {
			t.Errorf("the plan's table names verdicts %v; Verdicts() is %v", seen, Verdicts())
		}
	})
}
