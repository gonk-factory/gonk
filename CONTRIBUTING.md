# Contributing to gonk

Thank you for looking. This page says how this repository is produced, because
that decides how a change reaches it.

## This repository is a release mirror

gonk is developed in a private repository. What you see here is published one
release at a time: each commit on `main` is a **release snapshot** -- the tree
of one source commit with the maintainers' internal working files removed, and
one generated file, `EXPORT-SOURCE`, added. Nothing else is changed: every file
here is byte-for-byte the file the release was built and tested from.

Consequences worth knowing before you read the history:

- `git log` and `git blame` stop at release granularity. One commit per release.
- A snapshot commit and its tag carry two trailers: `Gonk-Source-Commit` (the
  source commit; it does not resolve here) and `Gonk-Export-Tree` (the id of
  the commit's own tree, so a snapshot can be told from anything else).
- Code and documents cite identifiers of the form `gonk-xxxx`. They are ids in
  the maintainers' issue tracker, which is not published. Treat them as opaque
  labels: the surrounding text is meant to stand on its own.
- Some comments name files that are not here (design plans, the tracker
  export, agent instructions). Those files are internal working material. If a
  comment is unclear without one, that is a documentation bug -- please open an
  issue.

The tool that produces the snapshot is in this tree (`hack/relexport`,
`internal/exportguard`); the list of removed paths and the maintainers' own
deny patterns are not.

## How a pull request flows

1. Open the pull request against `main`. CI runs the same gate the maintainers
   run (below), including a check that the change adds no private network
   address, key material or personal path.
2. A maintainer imports your commits, with your authorship, into the private
   repository, where they go through review and the gate like any change.
3. The pull request is then **closed, not merged**, with a comment naming the
   release that will carry it. A merge on this side would make `main` something
   no export produced, and the next release would refuse to publish on top of it.
4. The release that carries your change lists it in its notes and credits you
   with a `Co-authored-by` trailer on the snapshot commit.

So a change you contribute appears here at the next release, not when it is
accepted. For anything larger than a small fix, open an issue first.

## Running the gate

The build is vendored and offline (`GOFLAGS=-mod=vendor`, `GOPROXY=off` are the
Makefile's defaults), so no module proxy is needed.

```
make gate
```

That is: a Go toolchain check, `gofmt`, `go vet` under every build tag, the
export guard, `go test ./... -race`, the chart goldens, and `golangci-lint`.
It needs Go (the series `images/versions.env` pins), helm 3 for the chart
goldens (3.14 or later; helm 4 is refused because it renders different
whitespace, so if `helm` on your PATH is helm 4 run
`make gate HELM=/path/to/helm3`), and either `golangci-lint` at the pinned version or a
container runtime to run the pinned image. `docs/environment.md`, "Building
and testing this repository", says what the container-backed suites need;
`docs/testing-time.md` has the rules for tests that involve time.

One test compares the chart against the released state and needs a
`refs/remotes/origin/main` to compare with. A clone of this repository has one.
A source tarball does not; that test says so and what to do.

In this repository the export guard runs in PUBLIC mode: the generic rules only
(private and carrier-grade address ranges, IPv6 local ranges, certificates and
keys, credentials in URLs, e-mail addresses outside a short reserved list,
home-directory paths). Use reserved values in examples and fixtures:
`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, and names under
`example.internal`, `*.example` or `*.test`.

## Security

Please do not open a public issue for a vulnerability. Use GitHub's private
vulnerability reporting on this repository.

## Licence

Apache-2.0. By contributing you agree your contribution is licensed under it.
See `LICENSE` and `NOTICE`; `NOTICE` explains the provenance of `pack/`, which
must stay true.
