package main

import "strings"

// normaliseLabel forces an agent-proposed label into the project's configured
// label namespace.
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
// Normalise rather than reject. Rejecting would be more in keeping with a gate
// that validates rather than repairs, but a re-sling onto a pricier rung is a
// steep price for a cosmetic mistake in a field the model should not have had a
// say in anyway.
//
// The rules, in order:
//  1. Already in the namespace -> unchanged.
//  2. Starts with the namespace's STEM and a separator (gonk/, gonk:, gonk-)
//     -> that leading form is replaced by the configured prefix. This is the
//     observed failure and the one worth handling precisely.
//  3. Anything else -> the prefix is prepended, so a bare `bug` becomes
//     `gonk::bug` and nothing an agent proposes can land outside the namespace.
//
// An empty prefix disables all of it: a project that configures no namespace
// gets its labels through untouched.
func normaliseLabel(label, prefix string) string {
	l := strings.TrimSpace(label)
	if l == "" || prefix == "" {
		return l
	}
	if strings.HasPrefix(l, prefix) {
		return l
	}
	// The stem is the prefix with its trailing separator run removed:
	// "gonk::" -> "gonk". Compared case-insensitively because a label is a
	// human-facing string and `Gonk/bug` is the same mistake as `gonk/bug`.
	stem := strings.TrimRight(prefix, ":/-")
	if stem != "" && len(l) > len(stem) && strings.EqualFold(l[:len(stem)], stem) {
		rest := l[len(stem):]
		if trimmed := strings.TrimLeft(rest, ":/-"); trimmed != rest && trimmed != "" {
			return prefix + trimmed
		}
	}
	return prefix + l
}
