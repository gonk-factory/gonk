package effects

// RETRY FEEDBACK (gonk-nf9i, gonk-zxsx): what the controller tells the NEXT
// attempt about a batch a static gate refused, and the literal key lists a
// prompt states.
//
// THE REFUSED BATCH IS UNTRUSTED MODEL OUTPUT, and for some agents the text it
// was written from is attacker-influenced. A feedback sentence is therefore
// BUILT here, never copied: every word of it is one of
//
//   - a fixed sentence of this file;
//   - a value of a closed set this package owns (a kind, a class, a fact key,
//     a rule code, a JSON key of Effect);
//   - a number the controller counted;
//   - AT MOST ONE Token(): a short, length-capped, character-filtered, quoted
//     rendering of the one offending name (a misspelled key, an unknown kind).
//
// No Explain* function reads a Refusal's Detail or any error's text into its
// result, and none reads a free-text field of the batch (summary, remedy,
// reason, old, new, body, a quoted log line, a path). The tests in
// feedback_test.go put a marker in every such field and fail if any of it
// comes out.
//
// Nothing here decides anything. The gates that refused the batch are
// unchanged, and a batch is never repaired: the feedback only names what the
// gate already said.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// MaxTokenBytes bounds the one piece of untrusted text a feedback sentence
// may carry.
const MaxTokenBytes = 32

// Token renders untrusted text as a token that is safe to place in a prompt:
// at most MaxTokenBytes bytes of it, every byte outside [A-Za-z0-9_.-]
// replaced by '?', between double quotes. A name that was cut says so OUTSIDE
// the quotes. An empty name renders as "".
//
// The alphabet is that of a JSON key or a rule code and nothing more: no
// space, no newline, no control character, no quote, no backslash, no bracket
// of any kind survives, so a token cannot end the line it is on, close a
// fence, or read as a sentence.
func Token(s string) string {
	cut := false
	if len(s) > MaxTokenBytes {
		s, cut = s[:MaxTokenBytes], true // feedback:token-cap
	}
	b := []byte(s)
	for i, c := range b {
		safe := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
		if !safe { // feedback:token-filter
			b[i] = '?'
		}
	}
	out := `"` + string(b) + `"`
	if cut {
		out += " (cut)"
	}
	return out
}

// Feedback is the controller's statement about one refused batch.
type Feedback struct {
	// Code is the refusal's stable code where one exists ("FX-SHAPE(kind)",
	// "parse", "diagnosis_invalid"); "" when there is none.
	Code string
	// What is one sentence saying what was wrong.
	What string
	// Allowed is the exact allowed set for whatever What names ("" when the
	// refusal is not about a set).
	Allowed string
}

// Zero reports whether there is nothing to say.
func (f Feedback) Zero() bool { return f == Feedback{} }

// ---- the key lists, from the definitions the validator reads ------------------

// fixKindOrder is the fix vocabulary in the order a prompt lists it.
// TestFixKindOrderIsFixKindFields ties it to fixKindFields.
var fixKindOrder = []Kind{KindEdit, KindFixMeta, KindScopeRequest}

// FixKinds returns the three kinds of a fix batch, in documented order (a copy).
func FixKinds() []Kind { return append([]Kind(nil), fixKindOrder...) }

// FixVerdicts returns the two verdicts a fix batch may state (a copy):
// rule shape:verdict's set.
func FixVerdicts() []Verdict { return []Verdict{VerdictFix, VerdictNeedsScope} }

// diagnosisFields names the Effect fields a diagnosis carries, as
// fixKindFields does for the fix kinds. TestDiagnosisFieldsMatchTheValidator
// ties it to validateDiagnosis in both directions.
var diagnosisFields = map[string]bool{
	"Classification": true, "Summary": true, "Evidence": true, "Remedy": true, "Files": true,
}

// jsonKey is the JSON key of a struct field: the name before the first comma
// of its tag. It is what encoding/json matches a batch's keys against.
func jsonKey(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// keysOf lists the JSON keys of t's fields that own() accepts, in struct order.
func keysOf(t reflect.Type, own func(field string) bool) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); own(f.Name) {
			out = append(out, jsonKey(f))
		}
	}
	return out
}

// KindKeys returns the exact JSON keys an effect of kind k carries, "kind"
// first, then its own in Effect's field order. It is read from the SAME
// tables the validator reads -- fixKindFields for the fix kinds,
// diagnosisFields for a diagnosis -- and from Effect's own struct tags, which
// are what ParseBatch's strict decoder matches. nil for any other kind.
func KindKeys(k Kind) []string {
	own := fixKindFields[k]
	if k == KindDiagnosis {
		own = diagnosisFields
	}
	if own == nil {
		return nil
	}
	return keysOf(reflect.TypeOf(Effect{}), func(f string) bool { return f == "Kind" || own[f] })
}

// BatchKeys returns the JSON keys of the batch object itself.
func BatchKeys() []string {
	return keysOf(reflect.TypeOf(Batch{}), func(string) bool { return true })
}

// EvidenceItemKeys returns the JSON keys of one evidence item.
func EvidenceItemKeys() []string {
	return keysOf(reflect.TypeOf(EvidenceItem{}), func(string) bool { return true })
}

// kindKeysText is "the keys of an effect of kind fix_meta are: kind, reason".
func kindKeysText(k Kind) string {
	return fmt.Sprintf("the keys of an effect of kind %s are: %s", k, strings.Join(KindKeys(k), ", "))
}

// allKindKeysText lists every given kind's keys on one line.
func allKindKeysText(kinds []Kind) string {
	parts := make([]string, 0, len(kinds)+1)
	for _, k := range kinds {
		if keys := KindKeys(k); keys != nil {
			parts = append(parts, fmt.Sprintf("%s: %s", k, strings.Join(keys, ", ")))
		}
	}
	if len(parts) == 0 { // feedback:no-key-table
		// No kind of the task has a key table the validator reads (comment,
		// label, file): there is no set to state, and none is invented.
		return ""
	}
	return "the keys of each effect kind are -- " + strings.Join(parts, "; ")
}

func batchKeysText() string {
	return "the keys of the batch itself are: " + strings.Join(BatchKeys(), ", ")
}

func evidenceKeysText() string {
	return "the keys of an evidence item are: " + strings.Join(EvidenceItemKeys(), ", ")
}

// FactsText is the closed evidence-fact key set as one line of feedback,
// built from factOrder -- the enumeration knownFacts (the gate's set) is
// built from.
func FactsText() string { return factsText() }

func factsText() string {
	keys := make([]string, len(factOrder))
	for i, f := range factOrder {
		keys[i] = string(f)
	}
	return "the fact keys are: " + strings.Join(keys, ", ")
}

func classesText() string {
	cs := make([]string, len(classificationOrder))
	for i, c := range classificationOrder {
		cs[i] = string(c)
	}
	return "the classifications are: " + strings.Join(cs, ", ")
}

func filesRequiredText() string {
	var cs []string
	for _, c := range ClassesRequiringFiles() {
		cs = append(cs, string(c))
	}
	return "the classifications that need at least one entry in files are: " + strings.Join(cs, ", ")
}

func kindsText(kinds []Kind) string {
	ks := make([]string, len(kinds))
	for i, k := range kinds {
		ks[i] = string(k)
	}
	return "the effect kinds of this task are: " + strings.Join(ks, ", ")
}

// KeyReference is the compact literal list a prompt states where it describes
// the output: one line per kind with that kind's exact keys, preceded by the
// batch's own when withBatch is set. Every name in it comes from KindKeys /
// BatchKeys, which read the validator's tables.
func KeyReference(kinds []Kind, withBatch bool) string {
	var b strings.Builder
	if withBatch {
		fmt.Fprintf(&b, "- the batch: %s\n", strings.Join(BatchKeys(), ", "))
	}
	for _, k := range kinds {
		fmt.Fprintf(&b, "- %s: %s\n", k, strings.Join(KindKeys(k), ", "))
	}
	return b.String()
}

// ---- where in the batch ---------------------------------------------------------

// effectAt is "effect 2 of 3 (fix_meta)", counting from 1. The kind is
// printed only when it is one of the closed set.
func effectAt(i, n int, k Kind) string {
	if knownKinds[k] {
		return fmt.Sprintf("effect %d of %d (%s)", i+1, n, k)
	}
	return fmt.Sprintf("effect %d of %d", i+1, n)
}

func effectOf(b Batch, i int) string {
	if i < 0 || i >= len(b.Effects) {
		return "the batch"
	}
	return effectAt(i, len(b.Effects), b.Effects[i].Kind)
}

// ---- a batch that could not be read ---------------------------------------------

// ExplainNoBatch is the feedback for output that held no fenced batch.
func ExplainNoBatch() Feedback {
	return Feedback{Code: "no-batch", What: "the output held no batch between the two fence lines shown under OUTPUT"}
}

// unknownFieldPrefix is how encoding/json reports a key DisallowUnknownFields
// refused. The package has no typed error for it.
const unknownFieldPrefix = `json: unknown field "`

// ExplainParse is the feedback for a batch ParseBatch refused. raw is the
// text between the fences, read ONLY to find which effect holds an unknown
// key; kinds are the effect kinds of the agent's task.
func ExplainParse(raw []byte, err error, kinds []Kind) Feedback {
	fb := Feedback{Code: "parse"}
	var pe *ParseError
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &pe):
		switch pe.Reason {
		case ParseUnknownKind:
			fb.What = fmt.Sprintf("effect %d has a kind that does not exist: %s", pe.Effect+1, Token(pe.Value))
			fb.Allowed = kindsText(kinds)
		case ParseUnknownClassification:
			fb.What = fmt.Sprintf("effect %d states a classification that does not exist: %s", pe.Effect+1, Token(pe.Value))
			fb.Allowed = classesText()
		case ParseUnknownVerdict:
			fb.What = "the batch states a verdict that does not exist: " + Token(pe.Value)
		}
	case errors.Is(err, ErrBatchDuplicateKey):
		fb.What = "the batch itself states the same key more than once: each of its keys is stated at most once"
		fb.Allowed = batchKeysText()
	case errors.Is(err, ErrBatchTrailingData):
		fb.What = "there is text after the end of the JSON object between the fence lines: the batch is exactly one JSON object and nothing else"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		fb.What = "the JSON between the fence lines ended before it was complete: a string, a bracket or a brace was left open, or there was no JSON at all"
	case errors.As(err, &syn):
		fb.What = fmt.Sprintf("the text between the fence lines is not valid JSON (the first error is at byte %d of %d)", syn.Offset, len(raw))
	case errors.As(err, &typ):
		fb.What = fmt.Sprintf("the value of key %s has the wrong JSON type (it is a %s)", Token(typ.Field), Token(typ.Value))
	default:
		name, ok := unknownFieldName(err)
		if !ok {
			fb.What = "the batch could not be read as the JSON object shown under OUTPUT"
			break
		}
		fb.What, fb.Allowed = locateUnknownKey(raw, name, kinds)
	}
	return fb
}

// unknownFieldName extracts the key encoding/json refused. The package
// prints it with %q, so it is unquoted here: the name is then the batch's
// own key, byte for byte, and locateUnknownKey can find it.
func unknownFieldName(err error) (string, bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		msg := e.Error()
		if !strings.HasPrefix(msg, unknownFieldPrefix) {
			continue
		}
		quoted := msg[len(unknownFieldPrefix)-1:]
		if name, uerr := strconv.Unquote(quoted); uerr == nil {
			return name, true
		}
		return strings.Trim(quoted, `"`), true
	}
	return "", false
}

// ownsKey reports whether t has a field encoding/json would match key to (it
// matches case-insensitively).
func ownsKey(t reflect.Type, key string) bool {
	for i := 0; i < t.NumField(); i++ {
		if strings.EqualFold(jsonKey(t.Field(i)), key) {
			return true
		}
	}
	return false
}

// locateUnknownKey says where the refused key sits and what is allowed
// there. It decodes raw LOOSELY, into maps, and reads nothing from it but
// key names and each effect's kind: no value of the batch is copied.
func locateUnknownKey(raw []byte, name string, kinds []Kind) (what, allowed string) {
	tok := Token(name)
	var top map[string]json.RawMessage
	if json.Unmarshal(escapeRawControlsInStrings(raw), &top) != nil {
		return "the batch has a key that does not exist: " + tok, allKindKeysText(kinds)
	}
	if _, here := top[name]; here && !ownsKey(reflect.TypeOf(Batch{}), name) { // feedback:locate-batch
		return "the batch itself has a key that does not exist: " + tok, batchKeysText()
	}
	var effs []map[string]json.RawMessage
	_ = json.Unmarshal(top["effects"], &effs)
	for i, e := range effs {
		var kind Kind
		_ = json.Unmarshal(e["kind"], &kind)
		if _, here := e[name]; here && !ownsKey(reflect.TypeOf(Effect{}), name) { // feedback:locate-effect
			what = fmt.Sprintf("%s has a key that does not exist: %s", effectAt(i, len(effs), kind), tok)
			if KindKeys(kind) != nil {
				return what, kindKeysText(kind)
			}
			return what, allKindKeysText(kinds)
		}
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(e["evidence"], &items)
		for k, it := range items {
			if _, here := it[name]; here && !ownsKey(reflect.TypeOf(EvidenceItem{}), name) {
				return fmt.Sprintf("evidence item %d of %d in %s has a key that does not exist: %s",
					k+1, len(items), effectAt(i, len(effs), kind), tok), evidenceKeysText()
			}
		}
	}
	return "the batch has a key that does not exist: " + tok, allKindKeysText(kinds)
}

// ---- the pack's shape ------------------------------------------------------------

// ExplainShape is the feedback for a batch Validate refused on its per-kind
// counts: it states the counts the shape takes. (A *Refusal from Validate --
// the verdict/kind rule -- is ExplainFix's.)
func ExplainShape(s Shape) Feedback {
	kinds := make([]string, 0, len(s.Kinds))
	for k := range s.Kinds {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		c := s.Kinds[Kind(k)]
		switch {
		case c.Max == N:
			parts = append(parts, fmt.Sprintf("%s at least %d", k, c.Min))
		case c.Min == c.Max:
			parts = append(parts, fmt.Sprintf("%s exactly %d", k, c.Min))
		default:
			parts = append(parts, fmt.Sprintf("%s %d to %d", k, c.Min, c.Max))
		}
	}
	return Feedback{Code: "shape", What: "the batch does not hold the effects this task takes: a kind is missing, repeated too often, or not one of them",
		Allowed: "the effects of one batch are -- " + strings.Join(parts, "; ")}
}

// ---- a fix batch -----------------------------------------------------------------

// fixRuleText is one fixed sentence per rule of this package: what the rule
// refuses, in words that name no cause and no remedy of any failure.
// TestEveryRuleHasFeedbackText fails when a rule is added without one.
var fixRuleText = map[Rule]string{
	RuleFixShape:      "the batch is not shaped as a fix batch",
	RuleForm:          "an edit is not well formed",
	RuleProtected:     "an edit names a file that is protected",
	RuleClass:         "an edit names a file of a kind this fix may not change",
	RuleCited:         "an edit names a file that is not in the list of files you may change",
	RuleBead:          "an edit names a file outside the paths this fix may change",
	RuleSize:          "the change is larger than a stated limit",
	RuleSecret:        "the change adds text that looks like a credential",
	RuleEditMissing:   "an edit names a file that does not exist at the commit; a file cannot be created",
	RuleEditEncoding:  "an edit names a file that is binary or not UTF-8",
	RuleEditNoMatch:   "the \"old\" text of an edit does not occur in the file exactly as written",
	RuleEditAmbiguous: "the \"old\" text of an edit occurs more than once in the file",
	RuleEditEmpties:   "the edits leave a file empty",
	RuleEditNoop:      "the edits leave a file exactly as it was",
}

// fixShapeText narrows FX-SHAPE and SC-FORM by their sub code.
var fixShapeText = map[string]string{
	"verdict":                   "the batch does not state one of the two verdicts of this task",
	"fix-with-request":          "a batch with verdict fix carries a scope_request",
	"fix-needs-edit":            "a batch with verdict fix carries no edit",
	"fix-needs-meta":            "a batch with verdict fix must carry exactly one fix_meta",
	"needs-scope-with-edit":     "a batch with verdict needs-scope carries an edit",
	"needs-scope-with-meta":     "a batch with verdict needs-scope carries a fix_meta",
	"needs-scope-needs-request": "a batch with verdict needs-scope must carry exactly one scope_request",
	"edit-needs-fix":            "an edit is only valid under verdict fix",
	"meta-needs-fix":            "a fix_meta is only valid under verdict fix",
	"request-needs-needs-scope": "a scope_request is only valid under verdict needs-scope",
	"edit-count":                "the batch carries more edits than the stated limit",
	"kind":                      "an effect is of a kind that does not belong in this batch",
	"foreign-field":             "an effect carries a key that belongs to another kind",
	"paths-count":               "the scope_request names more paths than the stated limit",
	"paths-form":                "a path of the scope_request is not a clean repository-relative path",
	"reason-empty":              "the fix_meta or scope_request has no reason",
	"reason-cap":                "the reason is longer than the stated limit",
	"path":                      "the path of an edit is not a clean repository-relative path",
	"old-empty":                 "the \"old\" text of an edit is empty",
	"identical":                 "the \"old\" and \"new\" text of an edit are the same",
	"nul":                       "the \"old\" or \"new\" text of an edit holds a NUL byte",
	"utf8":                      "the \"old\" or \"new\" text of an edit is not valid UTF-8",
}

// RuleText returns the fixed sentence of one of this package's rules, "" for
// a rule it does not own.
func RuleText(r Rule) string { return fixRuleText[r] }

// ExplainFix is the feedback for a fix batch a *Refusal refused. It reads the
// refusal's Rule, Sub and Effect index and the KIND of that effect -- never
// its Detail or Path, which quote the batch.
func ExplainFix(r *Refusal, b Batch) Feedback {
	fb := Feedback{Code: r.Code()}
	what := fixRuleText[r.Rule]
	if r.Rule == RuleFixShape || r.Rule == RuleForm {
		if t := fixShapeText[r.Sub]; t != "" { // feedback:sub-text
			what = t
		}
	}
	if what == "" {
		what = "the answer broke a rule of this task"
	}
	if r.Effect >= 0 && r.Effect < len(b.Effects) {
		what += " (" + effectOf(b, r.Effect) + ")"
		if (r.Rule == RuleFixShape || r.Rule == RuleForm) && KindKeys(b.Effects[r.Effect].Kind) != nil {
			fb.Allowed = kindKeysText(b.Effects[r.Effect].Kind)
		}
	}
	if r.Rule == RuleFixShape && r.Sub == "kind" {
		fb.Allowed = kindsText(fixKindOrder)
	}
	if r.Rule == RuleFixShape && r.Sub == "verdict" {
		fb.Allowed = fmt.Sprintf("the verdicts are: %s, %s", VerdictFix, VerdictNeedsScope)
	}
	fb.What = what
	return fb
}

// ---- a diagnosis -----------------------------------------------------------------

// ExplainDiagnosis is the feedback for a batch ValidateDiagnosis refused. It
// reads WHICH rule (errors.Is), which item (itemError) and counts; of the
// batch's values it prints only a classification already proven to be in the
// closed set, and one Token of an unknown fact key.
func ExplainDiagnosis(err error, b Batch) Feedback {
	fb := Feedback{Code: "diagnosis"}
	var d Effect
	for _, e := range b.Effects {
		if e.Kind == KindDiagnosis {
			d = e
			break
		}
	}
	item := -1
	var ie *itemError
	if errors.As(err, &ie) {
		item = ie.item
	}
	evidenceAt := func() string {
		if item < 0 || item >= len(d.Evidence) {
			return "an evidence item"
		}
		return fmt.Sprintf("evidence item %d of %d", item+1, len(d.Evidence))
	}
	switch {
	case errors.Is(err, ErrDiagnosisForeignField):
		fb.What, fb.Allowed = "the diagnosis carries a key that belongs to another kind", kindKeysText(KindDiagnosis)
	case errors.Is(err, ErrDiagnosisClassification):
		fb.What, fb.Allowed = "the classification is not one that exists: "+Token(string(d.Classification)), classesText()
	case errors.Is(err, ErrDiagnosisSummaryEmpty):
		fb.What = "the summary is empty"
	case errors.Is(err, ErrDiagnosisSummaryTooLong):
		fb.What = fmt.Sprintf("the summary is longer than %d characters", MaxDiagnosisSummaryChars)
	case errors.Is(err, ErrDiagnosisRemedyEmpty):
		fb.What = "the remedy is empty"
	case errors.Is(err, ErrDiagnosisRemedyTooLong):
		fb.What = fmt.Sprintf("the remedy is longer than %d characters", MaxDiagnosisRemedyChars)
	case errors.Is(err, ErrDiagnosisEvidenceCount):
		fb.What = fmt.Sprintf("evidence holds %d items; it takes %d to %d", len(d.Evidence), MinDiagnosisEvidence, MaxDiagnosisEvidence)
	case errors.Is(err, ErrDiagnosisEvidenceShape):
		fb.What = evidenceAt() + " must carry exactly one of line and fact, and it must not be blank"
		fb.Allowed = evidenceKeysText()
	case errors.Is(err, ErrDiagnosisEvidenceFact):
		fb.What = evidenceAt() + " names a fact key that does not exist"
		if item >= 0 && item < len(d.Evidence) { // feedback:fact-token
			fb.What += ": " + Token(string(d.Evidence[item].Fact))
		}
		fb.Allowed = factsText()
	case errors.Is(err, ErrDiagnosisEvidenceJob):
		fb.What = evidenceAt() + " names a job that is not one of the failed jobs, copied exactly from its heading"
	case errors.Is(err, ErrDiagnosisNoLineEvidence):
		fb.What = "no evidence item quotes a log line, although a failed job has a log excerpt"
	case errors.Is(err, ErrDiagnosisFileCount):
		fb.What = fmt.Sprintf("files holds %d paths; it takes at most %d", len(d.Files), MaxDiagnosisFiles)
	case errors.Is(err, ErrDiagnosisFilePath):
		fb.What = "an entry of files is not a clean repository-relative path"
		if item >= 0 && item < len(d.Files) {
			fb.What = fmt.Sprintf("entry %d of %d in files is not a clean repository-relative path", item+1, len(d.Files))
		}
	case errors.Is(err, ErrDiagnosisFilesRequired):
		fb.What = "the classification needs at least one entry in files, and files is empty"
		if knownClassifications[d.Classification] && classesRequiringFiles[d.Classification] {
			fb.What = fmt.Sprintf("classification %s needs at least one entry in files, and files is empty", d.Classification)
		}
		fb.Allowed = filesRequiredText()
	default:
		fb.What = "the diagnosis broke a rule of this task"
	}
	return fb
}
