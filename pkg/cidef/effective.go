package cidef

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Unresolved stands in the value tree where a value could not be resolved. A
// value that contains one is never equal to anything, itself included.
type Unresolved struct {
	Reason string
}

// Job is one job's effective definition: its own keys, over what it `extends`,
// over `default:` and the global `variables`, with anchors and merge keys
// expanded.
type Job struct {
	Name string
	// Stage is the effective stage (DefaultStage when the job names none).
	Stage string
	// Keys holds every effective key except `extends`. Values are nil, bool,
	// int, float64, string, []any, map[string]any, Scalar or Unresolved.
	// Read-only: values are shared between jobs.
	Keys map[string]any
	// Problems: why this job's definition as a whole could not be resolved
	// (an `extends` that names nothing, a cycle, a job that is not a mapping).
	// A job with a problem is never compared key by key.
	Problems []string
}

// Definition is the effective definition of one merged CI YAML document.
type Definition struct {
	// Jobs by name. Hidden jobs (a leading dot) are templates, not jobs.
	Jobs map[string]*Job
	// Stages is the stage list AS WRITTEN (DefaultStages when absent). In
	// GitLab's merged YAML it holds the implicit `.pre` and `.post`; Compare
	// sets those aside (ImplicitStages).
	Stages []string
	// Workflow is the top-level `workflow:` value, nil when absent.
	Workflow    any
	HasWorkflow bool
	// DefaultRetry is `default.retry`, nil when absent.
	DefaultRetry    any
	HasDefaultRetry bool
	// Variables is the top-level `variables:` value, nil when absent.
	Variables    any
	HasVariables bool
	// Problems: why the DOCUMENT could not be resolved. When non-empty
	// nothing else in the Definition is to be trusted, and Compare reports
	// only the problems.
	Problems []string
}

// maxExtendsDepth is how deep an `extends` chain may go. GitLab documents 11
// levels; a deeper chain is one GitLab rejects, so it is not resolved here.
const maxExtendsDepth = 11

// legacyDefaultKeys are the deprecated top-level spellings of `default:` keys.
var legacyDefaultKeys = []string{"image", "services", "cache", "before_script", "after_script"}

// reservedTopLevel are the top-level keys that are not jobs.
var reservedTopLevel = map[string]bool{
	"default": true, "include": true, "stages": true, "variables": true, "workflow": true,
	"image": true, "services": true, "cache": true, "before_script": true, "after_script": true,
}

// Effective resolves the merged CI YAML (the `merged_yaml` of GitLab's CI
// lint) into one effective definition per job. It never fails: what it cannot
// resolve is recorded as a Problem or an Unresolved value.
//
// Resolution order, per job, is GitLab's: `extends` (deep merge, the job's own
// keys over its templates', a later template over an earlier one), then
// `default:` for every key the job still lacks, then the global `variables`
// under the job's own -- the last two as `inherit:` allows.
func Effective(mergedYAML []byte) *Definition {
	def := &Definition{Jobs: map[string]*Job{}}
	root, problems := parseRoot(mergedYAML)
	if len(problems) > 0 {
		def.Problems = problems
		return def
	}
	problem := func(format string, args ...any) {
		def.Problems = append(def.Problems, fmt.Sprintf(format, args...))
	}

	// An include GitLab did not expand: part of the definition is elsewhere.
	// GitLab's merged YAML never has the key (it is expanded and dropped), so
	// this fires only for a document that is NOT one -- which is the point.
	if _, has := root["include"]; has {
		problem("an `include:` is left in the merged YAML: the included definition is not in this document")
	}

	defaults := map[string]any{}
	if v, has := root["default"]; has {
		m, ok := v.(map[string]any)
		if !ok {
			problem("`default:` is not a mapping")
		}
		for k, dv := range m {
			defaults[k] = dv
		}
	}
	for _, k := range legacyDefaultKeys {
		v, has := root[k]
		if !has {
			continue
		}
		if _, both := defaults[k]; both {
			problem("`%s` is set both at the top level and in `default:`", k)
			continue
		}
		defaults[k] = v
	}
	def.DefaultRetry, def.HasDefaultRetry = defaults["retry"]

	var globalVars map[string]any
	if v, has := root["variables"]; has {
		def.Variables, def.HasVariables = v, true
		m, ok := v.(map[string]any)
		if !ok {
			if _, unresolved := v.(Unresolved); !unresolved {
				problem("the top-level `variables:` is not a mapping")
			}
		}
		globalVars = m
	}

	def.Workflow, def.HasWorkflow = root["workflow"]

	def.Stages = append([]string{}, DefaultStages...)
	if v, has := root["stages"]; has {
		list, ok := v.([]any)
		if !ok {
			problem("`stages:` is not a list")
		}
		def.Stages = def.Stages[:0]
		for _, e := range list {
			s, isString := e.(string)
			if !isString {
				problem("`stages:` holds something that is not a stage name")
				continue
			}
			def.Stages = append(def.Stages, s)
		}
	}
	if len(def.Problems) > 0 {
		return def
	}

	r := &resolver{root: root, done: map[string]*resolved{}}
	for _, name := range sortedKeys(root) {
		if reservedTopLevel[name] || strings.HasPrefix(name, ".") {
			continue
		}
		job := &Job{Name: name, Stage: DefaultStage}
		def.Jobs[name] = job
		res := r.resolve(name, nil)
		if len(res.problems) > 0 {
			job.Problems = res.problems
			continue
		}
		keys := make(map[string]any, len(res.keys)+len(defaults))
		for k, v := range res.keys {
			if k != keyExtends {
				keys[k] = v
			}
		}
		inheritDefault, inheritVars, err := inheritance(keys["inherit"])
		if err != nil {
			job.Problems = append(job.Problems, err.Error())
			continue
		}
		for k, v := range defaults {
			if _, own := keys[k]; !own && inheritDefault(k) {
				keys[k] = v
			}
		}
		applyGlobalVariables(keys, globalVars, inheritVars)

		switch s := keys[KeyStage].(type) {
		case nil:
			if _, has := keys[KeyStage]; has {
				job.Problems = append(job.Problems, "`stage` is null")
				continue
			}
			keys[KeyStage] = DefaultStage
		case string:
			job.Stage = s
		default:
			job.Problems = append(job.Problems, "`stage` is not a stage name")
			continue
		}
		// A child pipeline: what it runs is defined by a file this document
		// does not hold, and nothing pins that file to the commit.
		if t, ok := keys["trigger"].(map[string]any); ok {
			if _, child := t["include"]; child {
				keys["trigger"] = Unresolved{Reason: "a child pipeline (`trigger: include`): its definition is not in this document"}
			}
		}
		job.Keys = keys
	}
	return def
}

// parseRoot parses one YAML document into its root mapping.
func parseRoot(mergedYAML []byte) (map[string]any, []string) {
	if len(bytes.TrimSpace(mergedYAML)) == 0 {
		return nil, []string{"no merged CI YAML"}
	}
	dec := yaml.NewDecoder(bytes.NewReader(mergedYAML))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []string{"the merged CI YAML holds no document"}
		}
		return nil, []string{"the merged CI YAML does not parse: " + err.Error()}
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		// A second document, or bytes after the first that do not parse.
		return nil, []string{"the merged CI YAML holds more than one YAML document"}
	}
	c := newConverter()
	v := c.convert(&doc)
	if len(c.fatal) > 0 {
		return nil, c.fatal
	}
	switch root := v.(type) {
	case map[string]any:
		return root, nil
	case Unresolved:
		return nil, []string{"the root of the merged CI YAML: " + root.Reason}
	}
	return nil, []string{"the root of the merged CI YAML is not a mapping"}
}

type resolved struct {
	keys     map[string]any
	depth    int // 0 for a definition that extends nothing
	problems []string
}

// resolver follows `extends`. Results are memoised per top-level key: whether
// a definition resolves, and to what, does not depend on who asked.
type resolver struct {
	root map[string]any
	done map[string]*resolved
}

// resolve returns the definition of the top-level key name with its `extends`
// applied. chain is the path of names being resolved, for cycle detection.
func (r *resolver) resolve(name string, chain []string) *resolved {
	if res, ok := r.done[name]; ok {
		return res
	}
	for _, c := range chain {
		if c == name {
			return &resolved{problems: []string{
				"an `extends` cycle: " + strings.Join(append(append([]string{}, chain...), name), " -> ")}}
		}
	}
	res := r.resolveUncached(name, append(append([]string{}, chain...), name))
	r.done[name] = res
	return res
}

func (r *resolver) resolveUncached(name string, chain []string) *resolved {
	raw, has := r.root[name]
	if !has {
		return &resolved{problems: []string{fmt.Sprintf("`extends` names %q, which the merged YAML does not define", name)}}
	}
	own, ok := raw.(map[string]any)
	if !ok {
		if u, unresolved := raw.(Unresolved); unresolved {
			return &resolved{problems: []string{fmt.Sprintf("%q: %s", name, u.Reason)}}
		}
		return &resolved{problems: []string{fmt.Sprintf("%q is not a mapping", name)}}
	}
	ext, has := own[keyExtends]
	if !has {
		return &resolved{keys: own}
	}
	names, ok := extendsNames(ext)
	if !ok {
		return &resolved{problems: []string{fmt.Sprintf("the `extends` of %q is not a name or a list of names", name)}}
	}
	merged := map[string]any{}
	depth := 0
	for _, n := range names {
		parent := r.resolve(n, chain)
		if len(parent.problems) > 0 {
			return &resolved{problems: parent.problems}
		}
		merged = deepMerge(merged, parent.keys)
		if parent.depth+1 > depth {
			depth = parent.depth + 1
		}
	}
	if depth > maxExtendsDepth {
		return &resolved{problems: []string{fmt.Sprintf("the `extends` chain of %q is deeper than %d levels", name, maxExtendsDepth)}}
	}
	return &resolved{keys: deepMerge(merged, own), depth: depth}
}

// extendsNames reads an `extends` value: one name or a list of names. false
// when it is anything else.
func extendsNames(v any) ([]string, bool) {
	switch x := v.(type) {
	case string:
		return []string{x}, true
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// ExtendsOf returns the template names a job's `extends` lists, in order, with
// aliases and merge keys followed. It is the lenient reading used to SHOW a
// definition (cmd/gonk-gate's pipeline prompt): entries that are not names are
// skipped. Effective is strict about the same value.
func ExtendsOf(job *yaml.Node) []string {
	m, ok := newConverter().convert(job).(map[string]any)
	if !ok {
		return nil
	}
	switch x := m[keyExtends].(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// inheritance reads a job's `inherit:` value into two predicates: is this
// `default:` key inherited, is this global variable inherited. Absent means
// everything is.
func inheritance(v any) (defaultKey, variable func(string) bool, err error) {
	all := func(string) bool { return true }
	if v == nil {
		return all, all, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, errors.New("`inherit` is not a mapping")
	}
	pick := func(key string) (func(string) bool, error) {
		switch x := m[key].(type) {
		case nil:
			if _, has := m[key]; has {
				return nil, fmt.Errorf("`inherit.%s` is null", key)
			}
			return all, nil
		case bool:
			return func(string) bool { return x }, nil
		case []any:
			set := map[string]bool{}
			for _, e := range x {
				s, isString := e.(string)
				if !isString {
					return nil, fmt.Errorf("`inherit.%s` lists something that is not a name", key)
				}
				set[s] = true
			}
			return func(k string) bool { return set[k] }, nil
		}
		return nil, fmt.Errorf("`inherit.%s` is neither true, false nor a list of names", key)
	}
	for k := range m {
		if k != "default" && k != "variables" {
			return nil, nil, fmt.Errorf("`inherit.%s` is not something this package knows how to apply", k)
		}
	}
	if defaultKey, err = pick("default"); err != nil {
		return nil, nil, err
	}
	if variable, err = pick("variables"); err != nil {
		return nil, nil, err
	}
	return defaultKey, variable, nil
}

// applyGlobalVariables puts the inherited global variables under the job's
// own. keys is the job's fresh key map and is modified. (A global block that
// is itself unresolved has no names to inherit; Compare reports it as the
// document-level key it is.)
func applyGlobalVariables(keys map[string]any, global map[string]any, inherit func(string) bool) {
	inherited := map[string]any{}
	for name, v := range global {
		if inherit(name) {
			inherited[name] = v
		}
	}
	if len(inherited) == 0 {
		return
	}
	switch own := keys["variables"].(type) {
	case nil:
		if _, has := keys["variables"]; !has {
			keys["variables"] = inherited
		}
	case map[string]any:
		out := make(map[string]any, len(inherited)+len(own))
		for k, v := range inherited {
			out[k] = v
		}
		for k, v := range own {
			out[k] = v
		}
		keys["variables"] = out
	}
	// The job's `variables` is something else (Unresolved, malformed): it is
	// left as written, and compares as written.
}

var (
	parallelIndex  = regexp.MustCompile(`^(.+) \d+/\d+$`)
	parallelMatrix = regexp.MustCompile(`^(.+): \[.*\]$`)
)

// DefinitionName maps a PIPELINE job name to the definition's job name. They
// differ for `parallel:` jobs, which GitLab names "<job> 1/3" or
// "<job>: [a, b]". An exact match always wins; a suffix is stripped only when
// the job it leaves has a `parallel` key. false when no job matches.
func DefinitionName(def *Definition, pipelineJobName string) (string, bool) {
	if def == nil {
		return "", false
	}
	if _, ok := def.Jobs[pipelineJobName]; ok {
		return pipelineJobName, true
	}
	for _, re := range []*regexp.Regexp{parallelIndex, parallelMatrix} {
		m := re.FindStringSubmatch(pipelineJobName)
		if m == nil {
			continue
		}
		if j, ok := def.Jobs[m[1]]; ok {
			if _, parallel := j.Keys["parallel"]; parallel {
				return m[1], true
			}
		}
	}
	return "", false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
