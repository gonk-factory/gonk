# syntax=docker/dockerfile:1
#
# gonk-stubmodel: the deterministic, OpenAI-compatible stub model server (Plan 06
# Task 1). Built for the component (L2) and e2e (L3) suites, where LiteLLM must
# reach it over the network. It has NO auth and must never be deployed anywhere
# real; the image is tagged with the run id and never pushed to a shared registry.
#
# DEVIATION FROM PLAN TEXT: the plan sketch used `go mod download` against a live
# proxy and a floating `golang:1.26` / distroless tag. This repo builds fully
# offline with a committed vendor/ (docs/environment.md: CI cannot reach
# proxy.golang.org) and pins EVERY base image by digest via images/versions.env
# ("NOTHING may say latest"). This Dockerfile follows the repo rule instead: it
# reads GO_VERSION / DISTROLESS_BASE as build args (supplied by `make` from
# images/versions.env, exactly like Dockerfile.intake / Dockerfile.meter) and
# builds with -mod=vendor, GOPROXY=off. There is no default and no `latest`.
ARG GO_VERSION
ARG DISTROLESS_BASE

FROM golang:${GO_VERSION} AS build
WORKDIR /src
ENV GOFLAGS=-mod=vendor GOPROXY=off
# vendor/ is committed; this build never touches the network.
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY test/ test/
RUN CGO_ENABLED=0 go build -o /out/gonk-stubmodel ./cmd/gonk-stubmodel

FROM ${DISTROLESS_BASE}
COPY --from=build /out/gonk-stubmodel /gonk-stubmodel
EXPOSE 8081
ENTRYPOINT ["/gonk-stubmodel"]
