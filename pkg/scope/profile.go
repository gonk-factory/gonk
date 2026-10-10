package scope

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Ecosystem profiles (gonk-wj14; design: 066 section 11).
//
// A PROFILE is a named set of path patterns for ONE ecosystem (a language and
// its tooling) or ONE documentation style, covering every class it has an
// opinion on. The menu below is the whole of it: profiles are DATA, there is
// no code per language.
//
// WHERE THE DATA LIVES, AND WHY HERE. The menu is a Go table in this file,
// compiled into the binary. It is "pack-shipped" in the sense that matters --
// operator-reviewed through gonk code review and baked into the image -- and
// keeping it out of a file is deliberate:
//
//   - the contract stays what it was (contract_test.go): this package opens
//     exactly one file, the pack's effect-shape.toml, and imports nothing that
//     could carry a batch. A profile file would be a second place policy is
//     read from;
//   - the sensitive rows are a FLOOR. A floor that had to be loaded could fail
//     to load, and then either every path is unclassifiable or package.json
//     quietly stops being `deps`. Compiled data cannot be missing, stale or
//     shadowed by a /city copy;
//   - Builtin(p) stays a pure function of p.
//
// THE TWO KINDS OF ROW:
//
//   - SENSITIVE rows (deploy, test-config, deps, generated) of EVERY shipped
//     profile ALWAYS apply, whether or not anybody selected the profile. No
//     layer can turn them off: there is no key that does. package.json is
//     `deps` and conftest.py is `test-config` on a Go project too.
//   - PERMISSIVE rows (test, docs, source) apply only for the profiles a
//     project SELECTED (`.gonk.yml` scope.profiles), out of the menu the
//     operator layers allow (scope.allowed_profiles).
//
// Precedence is the class order, unchanged: floor, then every sensitive row,
// then the selected permissive rows, then `other`. So a path a sensitive row
// of ANY profile matches is never source/test/docs, whatever is selected, and
// selecting a profile can only ever turn `other` into test, docs or source
// (TestSelectingProfilesOnlyMovesOther).
//
// A profile row may not name control, secret or ci (nor agent-context or
// other): every class a profile can say anything about is matched AFTER the
// three protected classes, so no profile can move a path out of one, and none
// can pull a path INTO ci, the one class a dispatch can lift.

// ProfileKind says what a profile describes.
type ProfileKind string

const (
	// ProfileEcosystem: a language and its tooling (source, test, test-config,
	// deps, generated).
	ProfileEcosystem ProfileKind = "ecosystem"
	// ProfileDocs: a documentation style (docs).
	ProfileDocs ProfileKind = "docs"
)

// Weakening records whether a deterministic test-weakening predicate
// (SC-WEAKEN: a test removed, skipped or build-tagged away) exists for an
// ecosystem's tests. It is a PROPERTY of the profile, recorded now and
// consumed later; no predicate is built in this package.
type Weakening string

const (
	// WeakeningDeterministic: SC-WEAKEN is defined for this ecosystem's test
	// files (Go: an AST comparison of base and patched blob).
	WeakeningDeterministic Weakening = "deterministic"
	// WeakeningNone: nothing deterministic says whether a change to this
	// ecosystem's tests weakened them. A fix that touches its source or test
	// files is "not caught, human review, no auto-merge" (gonk-2usw U4).
	WeakeningNone Weakening = "none"
)

// ProfileRow is one class's patterns in a profile.
type ProfileRow struct {
	Class    Class
	Patterns []string
}

// Profile is one entry of the shipped menu.
type Profile struct {
	// Name is what scope.profiles and scope.allowed_profiles list.
	Name string
	Kind ProfileKind
	// Summary is one line for documentation and for the onboarding text.
	Summary string
	// Rows are the profile's patterns, by class. Rows of a sensitive class
	// always apply; rows of test, docs and source apply when selected.
	Rows []ProfileRow
	// Weakening is set on every ecosystem profile, explicitly, and empty on a
	// documentation profile (which has no tests).
	Weakening Weakening
	// Markers are root-level file NAMES whose presence on the default branch
	// makes onboarding PROPOSE this profile (ProposeProfiles). They are exact
	// paths, not patterns. Nothing reads them at dispatch time.
	Markers []string
}

// shipped is the menu, in menu order. `go` and `docs-md` together are exactly
// the built-in table as it was before profiles existed
// (TestGoAndDocsMdAloneAreTheTableBeforeProfiles); every other entry is new.
var shipped = mustProfiles([]Profile{
	{
		Name: "go", Kind: ProfileEcosystem, Summary: "Go modules",
		Weakening: WeakeningDeterministic,
		Markers:   []string{"go.mod"},
		Rows: []ProfileRow{
			{ClassTestConfig, []string{"Makefile", "**/*.mk", ".golangci.yml", "**/testmain_test.go", "go.work"}},
			{ClassDeps, []string{"go.mod", "go.sum", "vendor/**"}},
			{ClassTest, []string{"**/*_test.go", "**/testdata/**"}},
			{ClassSource, []string{"**/*.go"}},
		},
	},
	{
		Name: "python", Kind: ProfileEcosystem, Summary: "Python (pytest conventions)",
		Weakening: WeakeningNone,
		Markers:   []string{"pyproject.toml", "setup.py", "requirements.txt"},
		Rows: []ProfileRow{
			{ClassTestConfig, []string{"**/conftest.py", "**/pytest.ini", "**/tox.ini", "**/noxfile.py", "**/setup.cfg", "**/.coveragerc"}},
			{ClassDeps, []string{"**/requirements*.txt", "**/constraints*.txt", "**/pyproject.toml", "**/setup.py", "**/Pipfile*",
				"**/poetry.lock", "**/uv.lock", "**/.pypirc"}},
			{ClassGenerated, []string{"**/*_pb2.py", "**/*_pb2_grpc.py", "**/*_pb2.pyi"}},
			{ClassTest, []string{"**/tests/**", "**/test_*.py", "**/*_test.py"}},
			{ClassSource, []string{"**/*.py"}},
		},
	},
	{
		Name: "node", Kind: ProfileEcosystem, Summary: "JavaScript and TypeScript (npm, yarn, pnpm, bun)",
		Weakening: WeakeningNone,
		Markers:   []string{"package.json"},
		Rows: []ProfileRow{
			{ClassTestConfig, []string{"**/jest.config.*", "**/vitest.config.*", "**/playwright.config.*", "**/cypress.config.*",
				"**/karma.conf.*", "**/.mocharc*", "**/tsconfig*.json", "**/.eslintrc*", "**/eslint.config.*"}},
			{ClassDeps, []string{"**/package.json", "**/package-lock.json", "**/npm-shrinkwrap.json", "**/yarn.lock", "**/pnpm-lock.yaml",
				"**/pnpm-workspace.yaml", "**/bun.lockb", "**/bun.lock", "**/.npmrc", "**/.yarnrc*", "**/node_modules/**"}},
			{ClassGenerated, []string{"**/*.min.js", "**/*.min.css", "**/*.js.map", "**/*.css.map"}},
			{ClassTest, []string{
				"**/*.test.js", "**/*.test.jsx", "**/*.test.ts", "**/*.test.tsx", "**/*.test.mjs", "**/*.test.cjs",
				"**/*.spec.js", "**/*.spec.jsx", "**/*.spec.ts", "**/*.spec.tsx", "**/*.spec.mjs", "**/*.spec.cjs",
				"**/__tests__/**"}},
			{ClassSource, []string{"**/*.js", "**/*.jsx", "**/*.ts", "**/*.tsx", "**/*.mjs", "**/*.cjs"}},
		},
	},
	{
		Name: "docs-md", Kind: ProfileDocs, Summary: "Markdown, and everything under docs/",
		Rows: []ProfileRow{
			{ClassDocs, []string{"**/*.md", "docs/**", "CHANGELOG*"}},
		},
	},
	{
		// No `docs/**` here: a Sphinx tree holds conf.py, which the
		// documentation build EXECUTES, next to the prose. The always-on row
		// keeps that file sensitive even on a project that also selects
		// docs-md, whose `docs/**` would otherwise call it documentation.
		Name: "docs-rst", Kind: ProfileDocs, Summary: "reStructuredText",
		Rows: []ProfileRow{
			{ClassTestConfig, []string{"docs/**/conf.py", "doc/**/conf.py"}},
			{ClassDocs, []string{"**/*.rst", "CHANGELOG*"}},
		},
	},
	{
		// The always-on row names the *.txt file that is build configuration,
		// not prose.
		Name: "docs-txt", Kind: ProfileDocs, Summary: "plain-text documentation (*.txt)",
		Rows: []ProfileRow{
			{ClassTestConfig, []string{"**/CMakeLists.txt"}},
			{ClassDocs, []string{"**/*.txt", "CHANGELOG*"}},
		},
	},
	{
		Name: "docs-adoc", Kind: ProfileDocs, Summary: "AsciiDoc",
		Rows: []ProfileRow{
			{ClassDocs, []string{"**/*.adoc", "**/*.asciidoc", "CHANGELOG*"}},
		},
	},
})

// defaultProfiles is the selection of a project whose .gonk.yml has no
// scope.profiles key.
var defaultProfiles = []string{"go", "docs-md"}

var profileNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// profileClass reports whether a profile row may name c. See the package note
// above: only classes matched after control, secret and ci, and neither of the
// two classes that are not about an ecosystem.
func profileClass(c Class) bool {
	switch c {
	case ClassDeploy, ClassTestConfig, ClassDeps, ClassGenerated, ClassTest, ClassDocs, ClassSource:
		return true
	}
	return false
}

// alwaysApplies reports whether a profile row of class c applies without the
// profile being selected: every class that is not one of the three an ordinary
// code-writing workflow is given.
func alwaysApplies(c Class) bool { return c.Sensitivity() != Normal }

// validateProfiles checks a menu. The shipped one is checked when the package
// is initialised (a menu that does not validate is a build that does not
// start) and by TestShippedProfilesAreValid.
func validateProfiles(menu []Profile) error {
	seen := map[string]bool{}
	for _, pr := range menu {
		bad := func(format string, a ...any) error {
			return fmt.Errorf("scope: profile %q: %s", pr.Name, fmt.Sprintf(format, a...))
		}
		if !profileNameRe.MatchString(pr.Name) {
			return bad("the name is not a lowercase identifier")
		}
		if seen[pr.Name] {
			return bad("listed twice")
		}
		seen[pr.Name] = true
		switch pr.Kind {
		case ProfileEcosystem:
			// Said explicitly, never by omission (066 section 11, item 6).
			if pr.Weakening != WeakeningDeterministic && pr.Weakening != WeakeningNone {
				return bad("an ecosystem profile must declare whether a test-weakening predicate exists (got %q)", pr.Weakening)
			}
		case ProfileDocs:
			if pr.Weakening != "" {
				return bad("a documentation profile has no tests and declares no test-weakening predicate")
			}
			if len(pr.Markers) > 0 {
				return bad("a documentation profile has no marker files")
			}
		default:
			return bad("unknown kind %q", pr.Kind)
		}
		if len(pr.Rows) == 0 {
			return bad("no rows")
		}
		rows := map[Class]bool{}
		for _, row := range pr.Rows {
			if !profileClass(row.Class) {
				return bad("a profile row may not name class %q: profiles speak only for deploy, test-config, deps, generated, test, docs and source", row.Class)
			}
			if pr.Kind == ProfileDocs && (row.Class == ClassTest || row.Class == ClassSource) {
				return bad("a documentation profile may not declare %s paths", row.Class)
			}
			if rows[row.Class] {
				return bad("class %s listed twice", row.Class)
			}
			rows[row.Class] = true
			if len(row.Patterns) == 0 {
				return bad("class %s has no patterns", row.Class)
			}
			for _, pat := range row.Patterns {
				if err := ValidatePattern(pat); err != nil {
					return bad("class %s: %v", row.Class, err)
				}
			}
		}
		for _, m := range pr.Markers {
			if !cleanRel(m) || strings.ContainsAny(m, "*?/") {
				return bad("marker %q is not a root-level file name", m)
			}
		}
	}
	return nil
}

func mustProfiles(menu []Profile) []Profile {
	if err := validateProfiles(menu); err != nil {
		panic(err)
	}
	return menu
}

func copyProfile(pr Profile) Profile {
	out := pr
	out.Markers = append([]string(nil), pr.Markers...)
	out.Rows = make([]ProfileRow, len(pr.Rows))
	for i, row := range pr.Rows {
		out.Rows[i] = ProfileRow{Class: row.Class, Patterns: append([]string(nil), row.Patterns...)}
	}
	return out
}

// Profiles returns the shipped menu, in menu order. The result is a copy.
func Profiles() []Profile {
	out := make([]Profile, len(shipped))
	for i, pr := range shipped {
		out[i] = copyProfile(pr)
	}
	return out
}

// ProfileNames returns the names of the shipped profiles, in menu order.
func ProfileNames() []string {
	out := make([]string, len(shipped))
	for i, pr := range shipped {
		out[i] = pr.Name
	}
	return out
}

// LookupProfile returns the shipped profile of that name.
func LookupProfile(name string) (Profile, bool) {
	for _, pr := range shipped {
		if pr.Name == name {
			return copyProfile(pr), true
		}
	}
	return Profile{}, false
}

// DefaultProfiles returns the selection of a project that selected nothing:
// `go` and `docs-md`.
func DefaultProfiles() []string { return append([]string(nil), defaultProfiles...) }

// WeakeningPredicate reports whether a deterministic test-weakening predicate
// exists for the named profile's tests. It is true for `go` alone. A
// documentation profile, an unknown name and "" are all false: only an
// ecosystem that SAYS it has one has one.
func WeakeningPredicate(profile string) bool {
	for _, pr := range shipped {
		if pr.Name == profile {
			return pr.Weakening == WeakeningDeterministic
		}
	}
	return false
}

// checkProfileNames validates a list of profile names from configuration.
func checkProfileNames(key string, names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if _, ok := LookupProfile(n); !ok {
			return fmt.Errorf("scope: %s: unknown profile %q (the shipped profiles are %s)", key, n, strings.Join(ProfileNames(), ", "))
		}
		if seen[n] {
			return fmt.Errorf("scope: %s: profile %q is listed twice", key, n)
		}
		seen[n] = true
	}
	return nil
}

// SelectProfiles returns the profiles whose permissive rows apply to a
// project, in menu order: the project's scope.profiles, out of the menu its
// operator layers allow.
//
//   - The allowed menu is the shipped menu INTERSECTED with every operator
//     layer's allowed_profiles that is set (nil says nothing; an empty list
//     allows no profile).
//   - A project that names a profile outside the allowed menu is REFUSED. It
//     asked for rows its operator does not offer; quietly dropping them would
//     leave a project whose files are all `other` with nothing saying why.
//   - A project with no scope.profiles key gets the default (go, docs-md)
//     intersected with the allowed menu. That is not a refusal: the project
//     asked for nothing, and the operator's restriction only removes rows.
//
// It is called by Fold, and by the meter when a project registers and on every
// re-resolve, which is what makes a disallowed selection an invalid
// configuration at dispatch (no key, no decision, no spend) rather than
// something a judge discovers.
func SelectProfiles(project Policy, operator ...Policy) ([]string, error) {
	allowed := map[string]bool{}
	for _, n := range ProfileNames() {
		allowed[n] = true
	}
	for _, op := range operator {
		if op.AllowedProfiles == nil {
			continue
		}
		if err := checkProfileNames("allowed_profiles", op.AllowedProfiles); err != nil {
			return nil, err
		}
		next := map[string]bool{}
		for _, n := range op.AllowedProfiles {
			if allowed[n] {
				next[n] = true
			}
		}
		allowed = next
	}
	want := map[string]bool{}
	if project.Profiles == nil {
		for _, n := range defaultProfiles {
			if allowed[n] {
				want[n] = true
			}
		}
	} else {
		if err := checkProfileNames("profiles", project.Profiles); err != nil {
			return nil, err
		}
		for _, n := range project.Profiles {
			if !allowed[n] {
				return nil, fmt.Errorf("scope: profiles: %q is not in the operator's allowed_profiles; a project selects from the menu its operator offers", n)
			}
			want[n] = true
		}
	}
	out := []string{}
	for _, n := range ProfileNames() {
		if want[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// ProposeProfiles is onboarding's proposal (066 section 11, item 5:
// "onboarding proposes, a human disposes"). Given the root-level marker files
// FOUND on the default branch, it returns the selection to pre-fill in the
// generated .gonk.yml and, for each ecosystem it proposes, the marker that
// was the reason.
//
// It is a pure function, called ONCE, when the onboarding merge request is
// rendered. Nothing calls it at dispatch time: repository content never
// chooses its own scope, a human merges the proposal or edits it.
//
// No ecosystem marker found means the default selection, with no evidence.
// The documentation style is not detected: `docs-md` is always proposed.
func ProposeProfiles(found []string) (profiles []string, evidence []ProfileEvidence) {
	have := map[string]bool{}
	for _, f := range found {
		have[f] = true
	}
	for _, pr := range shipped {
		for _, m := range pr.Markers {
			if have[m] {
				profiles = append(profiles, pr.Name)
				evidence = append(evidence, ProfileEvidence{Profile: pr.Name, Marker: m})
				break
			}
		}
	}
	if len(profiles) == 0 {
		return DefaultProfiles(), nil
	}
	return append(profiles, "docs-md"), evidence
}

// ProfileEvidence is why onboarding proposed a profile.
type ProfileEvidence struct {
	Profile string
	Marker  string // the root-level file that was found
}

// ProfileMarkers returns every marker file onboarding looks for, in menu
// order.
func ProfileMarkers() []string {
	var out []string
	for _, pr := range shipped {
		out = append(out, pr.Markers...)
	}
	return out
}

// ---- the compiled table ----

// tablePattern is one pattern of the table with where it came from ("" for the
// rows no profile owns).
type tablePattern struct {
	pattern string
	profile string
	segs    []string
	// base is set when the pattern is exactly `**/<base>`: a file-name pattern
	// matched anywhere in the tree, which most of the table is. It then
	// matches a path iff <base> matches the path's last segment, and match
	// answers without walking the segments.
	base string
}

// match reports whether the pattern matches the path whose segments are segs.
// It is matchSegs (glob.go), with the one shortcut described on base.
func (tp tablePattern) match(segs []string) bool {
	if tp.base != "" {
		ok, err := path.Match(tp.base, segs[len(segs)-1])
		return ok && err == nil
	}
	return matchSegs(tp.segs, segs)
}

type tableRow struct {
	class Class
	pats  []tablePattern
}

// table is the built-in pattern table for one profile selection: one row per
// class, in class order.
type table struct{ rows []tableRow }

// buildTable assembles the table for a menu and a selection. Per class, in
// class order: the core patterns, then every menu profile's row for that class
// -- unconditionally when the class always applies, and only for a selected
// profile otherwise.
func buildTable(menu []Profile, selected []string) *table {
	sel := map[string]bool{}
	for _, n := range selected {
		sel[n] = true
	}
	t := &table{}
	for _, c := range classOrder {
		if c == ClassOther {
			continue
		}
		row := tableRow{class: c}
		add := func(pattern, profile string) {
			tp := tablePattern{pattern: pattern, profile: profile, segs: strings.Split(pattern, "/")}
			if len(tp.segs) == 2 && tp.segs[0] == "**" && tp.segs[1] != "**" {
				tp.base = tp.segs[1]
			}
			row.pats = append(row.pats, tp)
		}
		for _, cr := range core {
			if cr.class == c {
				for _, pat := range cr.patterns {
					add(pat, "")
				}
			}
		}
		for _, pr := range menu {
			if !alwaysApplies(c) && !sel[pr.Name] {
				continue
			}
			for _, r := range pr.Rows {
				if r.Class == c {
					for _, pat := range r.Patterns {
						add(pat, pr.Name)
					}
				}
			}
		}
		t.rows = append(t.rows, row)
	}
	return t
}

// defaultTable is the table of the default selection: what Builtin uses.
var defaultTable = buildTable(shipped, defaultProfiles)

// classify returns p's built-in class under this table and these base facts,
// and the profile whose row decided it ("" when no profile's did).
func (t *table) classify(p string, base BaseFacts) (Class, string) {
	if !cleanRel(p) {
		return ClassControl, ""
	}
	segs := strings.Split(p, "/")
	for _, row := range t.rows {
		// The two fact-derived classes slot in at their place in the order.
		if row.class == ClassGenerated && base.Generated[p] {
			return ClassGenerated, ""
		}
		if row.class == ClassCI && included(base.CILocalIncludes, p) {
			return ClassCI, ""
		}
		for _, tp := range row.pats {
			if tp.match(segs) {
				return row.class, tp.profile
			}
		}
	}
	return ClassOther, ""
}
