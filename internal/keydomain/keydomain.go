// Package keydomain holds the DOMAIN of every Kubernetes label and annotation
// key gonk owns.
//
// WHY A PACKAGE FOR ONE STRING. A label key is a wire contract between a
// writer and a selector that no compiler checks against each other. Session
// Jobs are the sharp case: gonk-gate labels a Job when it creates it and
// later finds it again BY THAT LABEL (pkg/runtime/jobs). A gate that writes
// one domain and selects another creates sessions it can never list, reap or
// resolve -- and nothing errors; the list is simply empty.
//
// So the domain is spelled exactly once in Go, here, and every key is built
// from Prefix. TestKeyDomainIsSpelledOnce (internal/buildgate) keeps it from
// being re-inlined, and holds the chart (which cannot import Go) to the same
// string: Chart.yaml's annotations and the templates' labels.
//
// CHANGING THIS VALUE IS A WIRE CHANGE. Objects labelled under the previous
// domain are invisible to a binary built with the new one. Drain the session
// Jobs first; see "Upgrading" in chart/gonk/README.md.
package keydomain

// Domain is the DNS-subdomain prefix of gonk's label and annotation keys.
const Domain = "gonk-factory.github.io"

// Prefix is Domain with the separating slash: a key is Prefix + name.
const Prefix = Domain + "/"
