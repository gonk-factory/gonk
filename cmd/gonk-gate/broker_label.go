package main

import (
	"fmt"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
)

// gonkLabelPrefix is the literal namespace every broker-authored label
// constant below is written with (labelFixQueued, verdictLabel's returns,
// pkg/intake's DefaultDenyLabel/OnboardingIssueLabel). It is NOT the
// project's CONFIGURED prefix -- that is labelPrefix in broker_verdict.go,
// and a project can configure something else entirely -- those constants are
// hardcoded to "gonk::" regardless. It exists only so buildReservedLabels can
// strip that literal prefix back off to get the bare suffix ReservedLabels
// stores and refuseReserved checks a label's suffix against.
const gonkLabelPrefix = "gonk::"

// reservedVerdicts is every effects.Verdict that verdictLabel (broker_verdict.go)
// produces a DISTINCT gonk::verdict-* label for. It exists so
// buildReservedLabels can derive those labels from verdictLabel itself rather
// than restating its output, and so TestReservedLabelsCoverEveryVerdictLabel
// has something to range over: a verdict added to pkg/effects and given a new
// case in verdictLabel's switch, without also being added here, is exactly
// the hole that test exists to catch -- verdictLabel would start minting a
// new audit label an agent could immediately forge.
var reservedVerdicts = []effects.Verdict{
	effects.VerdictReplyOnly, effects.VerdictCodeChange, effects.VerdictClose,
}

// ReservedLabels is the post-prefix set of label values reserved for the
// broker's OWN audit trail (T-05, closes R-02). These are written by the
// CONTROLLER to record what gonk itself concluded, never proposed by a
// model: a human filtering the board on gonk::fix-queued is meant to be
// reading gonk's own decision to queue a fix, not a model's suggestion that
// it be queued. An agent-proposed label landing on one of these could forge
// that record.
//
// DERIVED, not restated. broker_verdict.go's labelFixQueued,
// labelNeedsMaintainer and verdictLabel() are the SAME PACKAGE as this file,
// and pkg/intake's DefaultDenyLabel and OnboardingIssueLabel are one field
// access away -- cmd/gonk-gate already imports pkg/intake
// (contract_test.go), and package main may import any pkg/... freely; the
// reverse direction (pkg/intake importing cmd/gonk-gate) is the one that
// would be a cycle, and this file does not need it. So every entry here has
// exactly one place it is spelled out: add a new broker-authored label by
// adding it to buildReservedLabels, not by editing this map's literal.
var ReservedLabels = buildReservedLabels()

func buildReservedLabels() map[string]bool {
	set := map[string]bool{}
	reserve := func(label string) {
		set[strings.ToLower(strings.TrimPrefix(label, gonkLabelPrefix))] = true
	}
	reserve(labelFixQueued)
	reserve(labelNeedsMaintainer)
	reserve(intake.DefaultDenyLabel)
	reserve(intake.OnboardingIssueLabel)
	for _, v := range reservedVerdicts {
		reserve(verdictLabel(v))
	}
	return set
}

// maxLabelBytes mirrors GitLab's own label length cap.
const maxLabelBytes = 255

// normaliseLabel forces an agent-proposed label into the project's configured
// label namespace, or REFUSES it outright when it cannot be trusted to mean
// what it appears to (T-05, closes R-02).
//
// APPLYING A LABEL IS THE BROKER'S JOB, NOT THE MODEL'S. What a label looks
// like is a property of the ISSUE STORE -- GitLab's `gonk::bug`, and something
// else entirely on Jira or GitHub -- so the namespace is ours to impose and not
// the model's to choose. On issue !58 the agent proposed `gonk/backend`,
// `gonk/bug` and `gonk/paging`, with a SLASH, against a project configured for
// `gonk::`. The broker applied them verbatim, so the project's label namespace
// became whatever the model happened to type and a filter written against the
// configured prefix silently missed them (gonk-prr).
//
// A malformed or dangerous label is REFUSED, not repaired, and refusing it
// refuses the whole batch (see applyBrokerBatch's label gate) rather than
// dropping just that effect -- a batch that half-applies is one a human cannot
// reconstruct the refusal reason for from the issue afterward. The checks, in
// order:
//  1. A comma anywhere in the raw label. AddIssueLabel used to send add_labels
//     as a query string, which GitLab splits on commas, so a label value like
//     "bug,security::critical" applied BOTH labels though the batch named one
//     effect (gonk-hnw / R-02). AddIssueLabel now sends a JSON array instead,
//     but a single label effect still names exactly one label -- a comma
//     inside it is refused rather than silently taken as one opaque string.
//  2. Whitespace-only, or empty after trimming.
//  3. The label that would actually be SENT -- after namespace repair, so a
//     bare label plus a multi-byte prefix is measured too -- is longer than
//     255 bytes, GitLab's own label length cap. Checked post-repair, not on
//     the raw input: a 255-byte label plus "gonk::" is 261 bytes on the wire,
//     which is over the cap this check exists to enforce even though the
//     raw label alone was not.
//  4. The value that would land in the namespace, after prefix repair, is in
//     ReservedLabels -- see its doc comment.
//
// Namespace repair, once a label clears those checks:
//  1. Already in the namespace -> unchanged.
//  2. Starts with the namespace's STEM and a separator (gonk/, gonk:, gonk-)
//     -> that leading form is replaced by the configured prefix. This is the
//     observed failure and the one worth handling precisely.
//  3. Anything else -> the prefix is prepended, so a bare `bug` becomes
//     `gonk::bug` and nothing an agent proposes can land outside the namespace.
//
// An empty prefix disables namespace repair (but not the refusal checks
// above): a project that configures no namespace gets its labels through
// untouched, checked against ReservedLabels and the length cap as-is.
func normaliseLabel(label, prefix string) (string, error) {
	if strings.Contains(label, ",") {
		return "", fmt.Errorf("label %q contains a comma; one label effect names exactly one label", label)
	}
	l := strings.TrimSpace(label)
	if l == "" {
		return "", fmt.Errorf("label is empty or whitespace-only")
	}

	final, suffix := repairNamespace(l, prefix)

	if len(final) > maxLabelBytes {
		return "", fmt.Errorf("label %q is %d bytes after applying the %q prefix, want <= %d",
			final, len(final), prefix, maxLabelBytes)
	}
	if err := refuseReserved(suffix); err != nil {
		return "", err
	}
	return final, nil
}

// repairNamespace applies the namespace-repair rules documented on
// normaliseLabel to an already-trimmed, comma-free, non-empty label. It
// returns both the FINAL label (what would be sent to GitLab) and the
// SUFFIX -- final with any namespace prefix stripped back off -- that
// refuseReserved checks and normaliseLabel measures for length against.
// Splitting this out of normaliseLabel lets both of those checks run against
// the label as it will actually be applied, not the raw input.
func repairNamespace(l, prefix string) (final, suffix string) {
	if prefix == "" {
		return l, l
	}
	if strings.HasPrefix(l, prefix) {
		return l, l[len(prefix):]
	}
	// The stem is the prefix with its trailing separator run removed:
	// "gonk::" -> "gonk". Compared case-insensitively because a label is a
	// human-facing string and `Gonk/bug` is the same mistake as `gonk/bug`.
	stem := strings.TrimRight(prefix, ":/-")
	if stem != "" && len(l) > len(stem) && strings.EqualFold(l[:len(stem)], stem) {
		rest := l[len(stem):]
		if trimmed := strings.TrimLeft(rest, ":/-"); trimmed != rest && trimmed != "" {
			return prefix + trimmed, trimmed
		}
	}
	return prefix + l, l
}

// refuseReserved errors when suffix -- the label value with any namespace
// prefix already stripped -- names one of the broker's own audit labels.
// Compared case-insensitively for the same reason normaliseLabel folds case
// elsewhere: a label is a human-facing string, and `Gonk::Fix-Queued` is the
// same mint attempt as `gonk::fix-queued`.
func refuseReserved(suffix string) error {
	if ReservedLabels[strings.ToLower(suffix)] {
		return fmt.Errorf("label %q is reserved for gonk's own audit trail", suffix)
	}
	return nil
}
