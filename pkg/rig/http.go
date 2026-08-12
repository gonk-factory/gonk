package rig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// MaxArchiveBytes caps a served tree. A repository is unbounded and
// caller-controlled, and the archive is read into memory before it is written
// out, so this is a real limit and not a formality. Chosen to comfortably hold a
// source repository while refusing one carrying large binaries -- an agent
// reading .agent/ and build files does not need those, and a pod that cannot
// hold the tree in its ephemeral storage is worse off than one with no checkout.
const MaxArchiveBytes = 64 << 20 // 64 MiB

// DefaultGrantTTL bounds how long a registered checkout stays fetchable. Long
// enough for a slow pod start plus a re-sling, short enough that an abandoned
// session does not leave a handle on a repository for the life of the process.
const DefaultGrantTTL = 30 * time.Minute

// Handler serves the two halves of the checkout path on intake's PRIVATE
// listener:
//
//	POST /rig/{alias}       register a grant  (called by gonk-gate, controller-side)
//	GET  /rig/{alias}.tar.gz fetch the tree   (called by the agent pod)
//
// Both live on the private listener ONLY. Neither is authenticated in itself:
// the NetworkPolicy is what keeps the registration side reachable from the
// controller and the delivery side from agent pods, exactly as it does for
// POST /admin/reconcile. The grant is what stops one session fetching another's
// project, so an unguessable alias is load-bearing, not cosmetic.
type Handler struct {
	Store   *Store
	Fetch   ArchiveFetcher
	Log     *slog.Logger
	TTL     time.Duration
	MaxSize int64
}

// registerRequest is the controller -> intake body.
type registerRequest struct {
	Project   string `json:"project"`
	ProjectID int64  `json:"project_id"`
	Ref       string `json:"ref"`
}

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) ttl() time.Duration {
	if h.TTL > 0 {
		return h.TTL
	}
	return DefaultGrantTTL
}

func (h *Handler) maxSize() int64 {
	if h.MaxSize > 0 {
		return h.MaxSize
	}
	return MaxArchiveBytes
}

// Register mounts both routes on mux under /rig/.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/rig/", h.serve)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/rig/")
	switch r.Method {
	case http.MethodPost:
		h.register(w, r, rest)
	case http.MethodGet:
		h.deliver(w, r, strings.TrimSuffix(rest, ".tar.gz"))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request, alias string) {
	if !ValidAlias(alias) {
		http.Error(w, "invalid alias", http.StatusBadRequest)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed body: "+err.Error(), http.StatusBadRequest)
		return
	}
	g := Grant{
		Project:   req.Project,
		ProjectID: req.ProjectID,
		Ref:       req.Ref,
		ExpiresAt: time.Now().Add(h.ttl()),
	}
	if err := h.Store.Register(alias, g); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.logger().Info("rig: checkout granted",
		"alias", alias, "project", req.Project, "project_id", req.ProjectID, "ref", req.Ref)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deliver(w http.ResponseWriter, r *http.Request, alias string) {
	if !ValidAlias(alias) {
		http.Error(w, "invalid alias", http.StatusBadRequest)
		return
	}
	g, err := h.Store.Lookup(alias)
	if err != nil {
		// 404 for unknown AND expired alike -- see ErrNoGrant. Do not leak which.
		http.Error(w, "no checkout for that session", http.StatusNotFound)
		return
	}
	if h.Fetch == nil {
		http.Error(w, "no archive fetcher configured", http.StatusServiceUnavailable)
		return
	}

	body, ferr := h.Fetch.RepoArchive(r.Context(), g.ProjectID, g.Ref, h.maxSize())
	if ferr != nil {
		// LOUD. A checkout miss makes the agent fall back to a no-repository
		// prompt, which is safe but much less useful, and the pod cannot say why.
		h.logger().Warn("rig: archive fetch failed; the session will run without a checkout",
			"err", ferr, "alias", alias, "project", g.Project, "ref", g.Ref)
		http.Error(w, "could not fetch the repository archive", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("X-Gonk-Rig-Ref", g.Ref)
	if _, werr := w.Write(body); werr != nil {
		h.logger().Warn("rig: write failed mid-delivery", "err", werr, "alias", alias)
	}
}

// FetchURL is the URL a pod uses to fetch its checkout. base is the in-cluster
// address of intake's private listener; the alias is the session's own.
//
// Built here rather than formatted at each call site so the delivery path and
// the prompt that advertises it cannot drift apart.
func FetchURL(base, alias string) string {
	return fmt.Sprintf("%s/rig/%s.tar.gz", strings.TrimSuffix(base, "/"), alias)
}

// Client registers grants from the controller side.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// Grant registers alias for a project at a ref. Returns an error the caller is
// expected to treat as NON-FATAL: a session without a checkout still runs, on a
// prompt that says so, which is strictly better than failing the dispatch.
func (c *Client) Grant(ctx context.Context, alias, project string, projectID int64, ref string) error {
	body, err := json.Marshal(registerRequest{Project: project, ProjectID: projectID, Ref: ref})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/rig/%s", strings.TrimSuffix(c.BaseURL, "/"), alias),
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("rig: grant %s: unexpected status %s", alias, resp.Status)
	}
	return nil
}
