package main

import (
	"fmt"
	"strings"
)

// ReservedLabels is the post-prefix set of label values reserved for the
// broker's OWN audit trail (T-05, closes R-02). These are written by the
// controller to record what gonk itself concluded -- gonk::fix-queued,
// gonk::needs-maintainer, gonk::denied, the gonk::verdict-* state labels --
// and an agent-proposed label that landed on one of them could forge that
// record: a human filtering the board on gonk::fix-queued is meant to be
// reading gonk's own decision to queue a fix, not a model's suggestion that it
// be queued.
//
// This is the ONE place the set is defined. The literal label constants next
// to it (broker_verdict.go's labelNeedsMaintainer/labelFixQueued,
// verdictLabel's returns, and pkg/intake's DefaultDenyLabel) must be kept in
// sync with this by hand -- there is no single Go identifier that can back
// both without either widening this package's exports past what T-05 scoped,
// or making pkg/intake import cmd/gonk-gate.
var ReservedLabels = map[string]bool{
	"fix-queued":          true,
	"needs-maintainer":    true,
	"denied":              true,
	"verdict-code-change": true,
	"verdict-close":       true,
	"verdict-reply-only":  true,
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
//  3. Longer than 255 bytes.
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
// untouched, checked against ReservedLabels as-is.
func normaliseLabel(label, prefix string) (string, error) {
	if strings.Contains(label, ",") {
		return "", fmt.Errorf("label %q contains a comma; one label effect names exactly one label", label)
	}
	l := strings.TrimSpace(label)
	if l == "" {
		return "", fmt.Errorf("label is empty or whitespace-only")
	}
	if len(l) > maxLabelBytes {
		return "", fmt.Errorf("label is %d bytes, want <= %d", len(l), maxLabelBytes)
	}
	if prefix == "" {
		if err := refuseReserved(l); err != nil {
			return "", err
		}
		return l, nil
	}
	if strings.HasPrefix(l, prefix) {
		if err := refuseReserved(l[len(prefix):]); err != nil {
			return "", err
		}
		return l, nil
	}
	// The stem is the prefix with its trailing separator run removed:
	// "gonk::" -> "gonk". Compared case-insensitively because a label is a
	// human-facing string and `Gonk/bug` is the same mistake as `gonk/bug`.
	stem := strings.TrimRight(prefix, ":/-")
	if stem != "" && len(l) > len(stem) && strings.EqualFold(l[:len(stem)], stem) {
		rest := l[len(stem):]
		if trimmed := strings.TrimLeft(rest, ":/-"); trimmed != rest && trimmed != "" {
			if err := refuseReserved(trimmed); err != nil {
				return "", err
			}
			return prefix + trimmed, nil
		}
	}
	if err := refuseReserved(l); err != nil {
		return "", err
	}
	return prefix + l, nil
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
