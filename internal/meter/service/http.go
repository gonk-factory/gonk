package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
	"gitlab.orac.local/agentic/gonk-project/pkg/trace"
)

// maxBodyBytes bounds every request body. A .gonk.yml is tiny; nothing on
// this API legitimately needs more than 1 MiB.
const maxBodyBytes = 1 << 20

// NewMux builds gonk-meter's HTTP surface. token is the bearer credential
// this API verifies (required); prevToken is an optional second rotation
// slot, accepted equally (Decision 12). An empty token is a fatal
// configuration error -- not an open door -- so it is refused here rather
// than silently accepting every request.
//
// metricsHandler serves GET /metrics -- normally
// promhttp.HandlerFor(reg, promhttp.HandlerOpts{}) against the same private
// registry a metrics.Metrics was built with (see cmd/gonk-meter/main.go). It
// is UNAUTHENTICATED, like /healthz and /readyz (meterapi.MetricsPath is
// already exempt in bearerAuth below) -- that is standard Prometheus scrape
// practice, and this endpoint carries no per-project secret, only aggregate
// counters. A nil metricsHandler leaves the route unmounted (a 404), which is
// fine for a test that does not care about metrics.
func NewMux(svc *Service, token, prevToken string, metricsHandler http.Handler) (http.Handler, error) {
	if token == "" {
		return nil, errors.New("service: bearer token is empty; refusing to start with an open door")
	}
	tokens := [][]byte{[]byte(token)}
	if prevToken != "" {
		tokens = append(tokens, []byte(prevToken))
	}

	h := &handler{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/projects/{project}", h.putProject)
	mux.HandleFunc("GET /v1/projects/{project}", h.getProject)
	mux.HandleFunc("DELETE /v1/projects/{project}", h.deleteProject)
	mux.HandleFunc("POST /v1/policy/decide", h.decide)
	mux.HandleFunc("POST /v1/policy/outcome", h.outcome)
	mux.HandleFunc("GET /v1/cost/bead/{bead_id}", h.costBead)
	mux.HandleFunc("GET /v1/cost/session/{session_key}", h.costSession)
	mux.HandleFunc("GET /v1/cost/project/{project}", h.costProject)
	mux.HandleFunc("GET /v1/cost/instance", h.costInstance)
	// prompt-by-reference (gonk-mzd). GET is exempt from the bearer in
	// bearerAuth; PUT and status are not.
	mux.HandleFunc("PUT "+meterapi.PromptPathPrefix+"{alias}", h.putPrompt)
	mux.HandleFunc("GET "+meterapi.PromptPathPrefix+"{alias}", h.takePrompt)
	mux.HandleFunc("GET "+meterapi.PromptPathPrefix+"{alias}/status", h.promptStatus)
	// Trajectory evidence ingest (gonk-p8j). Bearer-authenticated by the default
	// rule below -- ONLY the prompt GET is exempt, and this must never join it:
	// the agent pod can reach this meter, so an unauthenticated trace endpoint
	// would let the subject of the evidence write the evidence.
	mux.HandleFunc("POST "+meterapi.TracePath, h.putTrace)
	mux.HandleFunc("GET "+meterapi.TraceReadPathPrefix+"{session_key}", h.getTrace)
	// POST /admin/spend/sync IS bearer-authenticated (unlike /healthz,
	// /readyz, /metrics): it is on the same listener as everything else --
	// meter has one port, unlike intake's public/private split -- and
	// forcing a spend poll is not something an unauthenticated caller gets to
	// do. Go 1.22+'s ServeMux answers a non-POST request to this path with
	// 405 on its own (Task 9 Step 3b's "any other method -> 405"), so there
	// is no manual method check here.
	mux.HandleFunc("POST "+meterapi.AdminSpendSyncPath, h.adminSpendSync)
	mux.HandleFunc(meterapi.HealthzPath, h.healthz)
	mux.HandleFunc(meterapi.ReadyzPath, h.readyz)
	if metricsHandler != nil {
		mux.Handle(meterapi.MetricsPath, metricsHandler)
	}

	return bearerAuth(tokens, mux), nil
}

// bearerAuth compares the presented token against every configured slot with
// crypto/subtle.ConstantTimeCompare, ORING the results rather than
// short-circuiting: a short-circuit would leak, via timing, which slot
// matched and how many are configured. /healthz, /readyz, and /metrics are
// exempt.
func bearerAuth(tokens [][]byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case meterapi.HealthzPath, meterapi.ReadyzPath, meterapi.MetricsPath:
			next.ServeHTTP(w, r)
			return
		}
		// THE ONE UNAUTHENTICATED ROUTE (gonk-mzd). The agent pod holds no
		// credentials by design, so it cannot present a bearer; the 128-bit
		// alias in its GC_ALIAS is the capability instead.
		//
		// METHOD-AWARE ON PURPOSE. The switch above matches on path alone,
		// which is correct for /healthz and friends but would hand WRITE access
		// away here -- an unauthenticated PUT to this path would let anyone
		// replace the prompt a session is about to run. Only GET is exempt;
		// PUT and DELETE fall through to the bearer check below.
		//
		// SHAPE IS CHECKED BEFORE THE STORE IS TOUCHED. The handler cannot
		// measure entropy, only form, so a nonce-free alias is refused here.
		// That also stops this route becoming a free database probe on the same
		// listener that serves budget decisions.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, meterapi.PromptPathPrefix) {
			rest := strings.TrimPrefix(r.URL.Path, meterapi.PromptPathPrefix)
			// EXACT PATH ONLY. A sub-path under this prefix -- /status today,
			// anything added later -- must NOT inherit the exemption just by
			// living under it. Caught by the route table test: without this the
			// status route was swallowed here and 401'd even WITH a bearer,
			// which is the benign direction of a mistake whose other direction
			// hands operator data away.
			if !strings.Contains(rest, "/") {
				if aliasHasNonce(rest) {
					next.ServeHTTP(w, r)
					return
				}
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		presented := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		matched := 0
		for _, t := range tokens {
			matched |= subtle.ConstantTimeCompare(presented, t)
		}
		if matched != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handler struct {
	svc *Service
}

// ---------------------------------------------------------------- projects

func (h *handler) putProject(w http.ResponseWriter, r *http.Request) {
	project, ok := pathProject(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req meterapi.ProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	// The request is malformed (intake's bug): nothing is recorded.
	switch {
	case req.Project != project:
		writeError(w, http.StatusBadRequest, "project in body does not match the URL path")
		return
	case req.Project == "":
		writeError(w, http.StatusBadRequest, "project is required")
		return
	case req.ProjectID == 0:
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	case req.Rig == "":
		writeError(w, http.StatusBadRequest, "rig is required")
		return
	case !validProjectPath(project):
		writeError(w, http.StatusBadRequest, "project path contains unsafe characters")
		return
	}

	resp, status, err := h.svc.Register(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, status, resp)
}

func (h *handler) getProject(w http.ResponseWriter, r *http.Request) {
	project, ok := pathProject(w, r)
	if !ok {
		return
	}
	resp, found, err := h.svc.Get(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "project not registered")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	project, ok := pathProject(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), project); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Idempotent: deleting an unknown project is 204, not 404 -- intake retries.
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- decide / outcome

func (h *handler) decide(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req meterapi.DecideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	switch {
	case req.Project == "":
		writeError(w, http.StatusBadRequest, "project is required")
		return
	case req.BeadID == "":
		writeError(w, http.StatusBadRequest, "bead_id is required")
		return
	case req.SessionKey == "":
		writeError(w, http.StatusBadRequest, "session_key is required")
		return
	case req.Trigger == "":
		writeError(w, http.StatusBadRequest, "trigger is required")
		return
	case !validProjectPath(req.Project):
		writeError(w, http.StatusBadRequest, "project contains unsafe characters")
		return
	}

	d, extras, err := h.svc.Decide(r.Context(), req)
	if err != nil {
		// A hostile/invalid tag value (tagmint) is the caller's fault, not an
		// internal failure -- everything else on this path is a store or
		// LiteLLM failure and must fail closed as a 500 (no run, no reservation).
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	bud, rem, spendAsOf, err := h.svc.BudgetSnapshot(r.Context(), req.Project, req.BeadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := meterapi.DecideResponse{
		Decision:   string(d.Kind),
		Rung:       d.Rung,
		Model:      d.Model,
		Attempt:    d.Attempt,
		Reason:     d.Reason,
		Detail:     d.Detail,
		RetryAfter: d.RetryAfter,
		Metadata:   extras.Metadata,
		KeyRef:     meterapi.KeyRef{SecretName: extras.KeyRef.SecretName, SecretKey: extras.KeyRef.SecretKey},
		Budget:     wireBudget(bud.MonthlyCostUSD, bud.MonthlyTokens, bud.PerTaskTokens),
		Remaining:  wireBudget(rem.MonthlyCostUSD, rem.MonthlyTokens, rem.PerTaskTokens),
		SpendAsOf:  spendAsOf,
	}
	if resp.Metadata == nil {
		resp.Metadata = map[string]string{}
	}
	if extras.Reservation.ID != "" {
		resp.ReservationID = extras.Reservation.ID
		resp.ReservationExpiresAt = extras.Reservation.ExpiresAt
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) outcome(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req meterapi.OutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	switch {
	case req.Project == "":
		writeError(w, http.StatusBadRequest, "project is required")
		return
	case req.BeadID == "":
		writeError(w, http.StatusBadRequest, "bead_id is required")
		return
	case req.ReservationID == "":
		writeError(w, http.StatusBadRequest, "reservation_id is required")
		return
	}

	resp, err := h.svc.Outcome(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownReservation), errors.Is(err, ErrBadOutcome):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- cost

func (h *handler) costBead(w http.ResponseWriter, r *http.Request) {
	beadID, err := url.PathUnescape(r.PathValue("bead_id"))
	if err != nil || beadID == "" {
		writeError(w, http.StatusBadRequest, "invalid bead_id")
		return
	}
	resp, found, err := h.svc.BeadCost(r.Context(), beadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no attempts recorded for this bead")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) costSession(w http.ResponseWriter, r *http.Request) {
	sessionKey, err := url.PathUnescape(r.PathValue("session_key"))
	if err != nil || sessionKey == "" {
		writeError(w, http.StatusBadRequest, "invalid session_key")
		return
	}
	resp, err := h.svc.SessionCost(r.Context(), sessionKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) costProject(w http.ResponseWriter, r *http.Request) {
	project, ok := pathProject(w, r)
	if !ok {
		return
	}
	resp, err := h.svc.ProjectCost(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) costInstance(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.InstanceCost(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- admin

// adminSpendSync forces one spend-log poll and blocks until it has completed
// (Task 9 Step 3b, Plan 06 hand-back HB-2). The HTTP status is ALWAYS 200: a
// poll failure is reported in the body (Synced: false, Error set), not as an
// HTTP error, because the endpoint itself did its job -- it ran a sync
// attempt and is honestly reporting what happened. This mirrors /v1/policy/
// decide's own convention: defer and deny are 200s too, because they are
// normal answers, not failures of the API surface.
func (h *handler) adminSpendSync(w http.ResponseWriter, r *http.Request) {
	asOf, rowsIngested, unattributed, err := h.svc.ForceSpendSync(r.Context())
	resp := meterapi.SpendSyncResponse{SpendAsOf: asOf, Synced: err == nil}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.RowsIngested = &rowsIngested
		resp.Unattributed = &unattributed
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- health

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *handler) readyz(w http.ResponseWriter, _ *http.Request) {
	if !h.svc.Ready() {
		writeError(w, http.StatusServiceUnavailable, "not synced or clock skew exceeded")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------- helpers

// pathProject reads {project}, URL-path-unescaped -- a project name is
// untrusted input from GitLab -- and validates it before it is handed
// anywhere downstream (tagmint, the keysink slug, LiteLLM's key alias).
func pathProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.PathValue("project")
	project, err := url.PathUnescape(raw)
	if err != nil || project == "" || !validProjectPath(project) {
		writeError(w, http.StatusBadRequest, "invalid project path")
		return "", false
	}
	return project, true
}

// validProjectPath is the same charset gate tagmint applies to a project tag
// value (segments of alnum/._- joined by /), checked again here so a
// malformed path is a 400 at the door rather than a 500 surfacing from
// tagmint deep inside Decide.
func validProjectPath(project string) bool {
	if project == "" || len(project) > 200 || strings.Contains(project, "..") {
		return false
	}
	for _, seg := range strings.Split(project, "/") {
		if seg == "" {
			return false
		}
		for i, r := range seg {
			alnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if i == 0 && !alnum {
				return false
			}
			if !alnum && r != '.' && r != '_' && r != '-' {
				return false
			}
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, meterapi.ErrorResponse{Error: msg})
}

// wireBudget converts pkg/budget's ceiling types (which each know how to
// marshal themselves as null-means-unlimited) into meterapi.Budget's
// pointer-based wire shape.
func wireBudget(cost budget.CostLimit, monthly, perTask budget.TokenLimit) meterapi.Budget {
	out := meterapi.Budget{}
	if !cost.Unlimited() {
		v := float64(cost)
		out.MonthlyCostUSD = &v
	}
	if !monthly.Unlimited() {
		v := int64(monthly)
		out.MonthlyTokens = &v
	}
	if !perTask.Unlimited() {
		v := int64(perTask)
		out.PerTaskTokens = &v
	}
	return out
}

// ---------------------------------------------------------------- cost (service side)

// rungBreakdown groups rows matching `match` by rung, in first-seen order.
func rungBreakdown(rows []spend.Row, catalog map[string]opercfg.RungSpec, match func(spend.Row) bool) []meterapi.RungCost {
	var order []string
	seen := map[string]bool{}
	for _, r := range rows {
		if match(r) && !seen[r.Tags.Rung] {
			seen[r.Tags.Rung] = true
			order = append(order, r.Tags.Rung)
		}
	}
	out := make([]meterapi.RungCost, 0, len(order))
	for _, rg := range order {
		t := spend.Sum(rows, func(r spend.Row) bool { return match(r) && r.Tags.Rung == rg })
		kind := "cloud"
		if catalog[rg].Kind == opercfg.KindLocal {
			kind = "local"
		}
		out = append(out, meterapi.RungCost{
			Rung: rg, Kind: kind, CostUSD: t.CostUSD, SyntheticCostUSD: t.SyntheticCostUSD,
			CostSynthetic: kind == "local", TotalTokens: t.TotalTokens(), Calls: t.Calls,
		})
	}
	return out
}

// triggerBreakdown groups rows matching `match` by trigger, in first-seen order.
func triggerBreakdown(rows []spend.Row, match func(spend.Row) bool) []meterapi.TriggerCost {
	var order []string
	seen := map[string]bool{}
	for _, r := range rows {
		if match(r) && !seen[r.Tags.Trigger] {
			seen[r.Tags.Trigger] = true
			order = append(order, r.Tags.Trigger)
		}
	}
	out := make([]meterapi.TriggerCost, 0, len(order))
	for _, tr := range order {
		t := spend.Sum(rows, func(r spend.Row) bool { return match(r) && r.Tags.Trigger == tr })
		out = append(out, meterapi.TriggerCost{
			Trigger: tr, CostUSD: t.CostUSD, SyntheticCostUSD: t.SyntheticCostUSD,
			TotalTokens: t.TotalTokens(), Calls: t.Calls,
		})
	}
	return out
}

// BeadCost is lifetime, not windowed: the per-task ceiling is a property of
// the work item, not of the calendar.
// BeadCost's wire path (meterapi.CostBeadPath) carries ONLY bead_id, no
// project -- the BeadAnchor is derived from the project id, so it is
// effectively unique on its own. That means the project has to be
// DISCOVERED, not supplied: scan registrations for one whose attempt history
// mentions this bead. found is false if no project has ever recorded an
// attempt for it (the caller maps that to 404).
func (s *Service) BeadCost(ctx context.Context, beadID string) (meterapi.BeadCostResponse, bool, error) {
	regs, err := s.store.ListRegistrations(ctx)
	if err != nil {
		return meterapi.BeadCostResponse{}, false, err
	}
	var project string
	var prior []rung.Attempt
	for _, reg := range regs {
		attempts, err := s.store.Attempts(ctx, reg.Project, beadID)
		if err != nil {
			return meterapi.BeadCostResponse{}, false, err
		}
		if len(attempts) > 0 {
			project, prior = reg.Project, attempts
			break
		}
	}
	if project == "" {
		return meterapi.BeadCostResponse{}, false, nil
	}

	rows, err := s.store.SpendRows(ctx, project)
	if err != nil {
		return meterapi.BeadCostResponse{}, false, err
	}
	totals := spend.BeadTotals(rows, project, beadID)

	attempts := make([]meterapi.AttemptView, len(prior))
	for i, a := range prior {
		attempts[i] = meterapi.AttemptView{Attempt: a.Attempt, Rung: a.Rung, Outcome: string(a.Outcome)}
	}

	now := s.now()
	open, err := s.store.OpenReservations(ctx, project, now)
	if err != nil {
		return meterapi.BeadCostResponse{}, false, err
	}
	complete := true
	for _, r := range open {
		if r.BeadID == beadID {
			complete = false
			break
		}
	}

	asOf, err := s.store.SyncedAt(ctx)
	if err != nil {
		return meterapi.BeadCostResponse{}, false, err
	}

	match := func(r spend.Row) bool { return r.Tags.Project == project && r.Tags.BeadID == beadID }
	resp := meterapi.BeadCostResponse{
		BeadID: beadID, Project: project,
		CostUSD: totals.CostUSD, SyntheticCostUSD: totals.SyntheticCostUSD,
		PromptTokens: totals.PromptTokens, CompletionTokens: totals.CompletionTokens,
		TotalTokens: totals.TotalTokens(),
		ByRung:      rungBreakdown(rows, s.Config().Catalog, match),
		Attempts:    attempts,
		AsOf:        asOf, Complete: complete,
	}
	return resp, true, nil
}

// SessionCost is what commit provenance trailers read, while the session is
// still open -- so it will frequently report Complete: false.
func (s *Service) SessionCost(ctx context.Context, sessionKey string) (meterapi.SessionCostResponse, error) {
	rows, err := s.store.AllSpendRows(ctx)
	if err != nil {
		return meterapi.SessionCostResponse{}, err
	}
	totals := spend.SessionTotals(rows, sessionKey)

	var project, beadID string
	for _, r := range rows {
		if r.Tags.SessionKey == sessionKey {
			project, beadID = r.Tags.Project, r.Tags.BeadID
			break
		}
	}

	complete := true
	if project != "" {
		open, err := s.store.OpenReservations(ctx, project, s.now())
		if err != nil {
			return meterapi.SessionCostResponse{}, err
		}
		for _, r := range open {
			if r.SessionKey == sessionKey {
				complete = false
				break
			}
		}
	}

	asOf, err := s.store.SyncedAt(ctx)
	if err != nil {
		return meterapi.SessionCostResponse{}, err
	}

	match := func(r spend.Row) bool { return r.Tags.SessionKey == sessionKey }
	return meterapi.SessionCostResponse{
		SessionKey: sessionKey, Project: project, BeadID: beadID,
		CostUSD: totals.CostUSD, SyntheticCostUSD: totals.SyntheticCostUSD,
		PromptTokens: totals.PromptTokens, CompletionTokens: totals.CompletionTokens,
		TotalTokens: totals.TotalTokens(),
		ByRung:      rungBreakdown(rows, s.Config().Catalog, match),
		AsOf:        asOf, Complete: complete,
	}, nil
}

// ProjectCost is windowed by the current budget window, and carries the
// project's ceiling and remaining headroom.
func (s *Service) ProjectCost(ctx context.Context, project string) (meterapi.ProjectCostResponse, error) {
	rows, err := s.store.SpendRows(ctx, project)
	if err != nil {
		return meterapi.ProjectCostResponse{}, err
	}
	w, err := s.store.Window(ctx)
	if err != nil {
		return meterapi.ProjectCostResponse{}, err
	}
	totals := spend.ProjectTotals(rows, w, project)

	match := func(r spend.Row) bool { return r.Tags.Project == project && w.Contains(r.At) }

	now := s.now()
	open, err := s.store.OpenReservations(ctx, project, now)
	if err != nil {
		return meterapi.ProjectCostResponse{}, err
	}
	complete := len(open) == 0

	bud, rem, asOf, err := s.BudgetSnapshot(ctx, project, "")
	if err != nil {
		return meterapi.ProjectCostResponse{}, err
	}
	stale := !bud.AllUnlimited() && !asOf.IsZero() && now.Sub(asOf) > s.Config().Meter.MaxSpendStaleness
	if asOf.IsZero() {
		stale = !bud.AllUnlimited()
	}

	return meterapi.ProjectCostResponse{
		Project: project,
		Window:  meterapi.Window{Start: w.Start, End: w.End},
		CostUSD: totals.CostUSD, SyntheticCostUSD: totals.SyntheticCostUSD,
		PromptTokens: totals.PromptTokens, CompletionTokens: totals.CompletionTokens,
		TotalTokens: totals.TotalTokens(),
		ByRung:      rungBreakdown(rows, s.Config().Catalog, match),
		ByTrigger:   triggerBreakdown(rows, match),
		Budget:      wireBudget(bud.MonthlyCostUSD, bud.MonthlyTokens, bud.PerTaskTokens),
		Remaining:   wireBudget(rem.MonthlyCostUSD, rem.MonthlyTokens, rem.PerTaskTokens),
		AsOf:        asOf, Complete: complete, Stale: stale,
	}, nil
}

// InstanceCost rolls up every registered project.
func (s *Service) InstanceCost(ctx context.Context) (meterapi.InstanceCostResponse, error) {
	regs, err := s.store.ListRegistrations(ctx)
	if err != nil {
		return meterapi.InstanceCostResponse{}, err
	}
	w, err := s.store.Window(ctx)
	if err != nil {
		return meterapi.InstanceCostResponse{}, err
	}
	asOf, err := s.store.SyncedAt(ctx)
	if err != nil {
		return meterapi.InstanceCostResponse{}, err
	}

	byProject := make([]meterapi.ProjectCostResponse, 0, len(regs))
	var costUSD, syntheticUSD float64
	var tokens int64
	complete := true
	for _, reg := range regs {
		pc, err := s.ProjectCost(ctx, reg.Project)
		if err != nil {
			return meterapi.InstanceCostResponse{}, err
		}
		byProject = append(byProject, pc)
		costUSD += pc.CostUSD
		syntheticUSD += pc.SyntheticCostUSD
		tokens += pc.TotalTokens
		if !pc.Complete {
			complete = false
		}
	}

	return meterapi.InstanceCostResponse{
		Window:  meterapi.Window{Start: w.Start, End: w.End},
		CostUSD: costUSD, SyntheticCostUSD: syntheticUSD, TotalTokens: tokens,
		ByProject: byProject, AsOf: asOf, Complete: complete,
	}, nil
}

// --- prompt-by-reference (gonk-mzd) -----------------------------------------

// base32Alphabet is RFC 4648, upper-case, no padding -- what
// base32.StdEncoding produces for the alias nonce.
const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// aliasHasNonce is the shape gate on the one unauthenticated route.
//
// The handler cannot measure entropy, only form. Requiring the final
// dot-separated label to be exactly 26 base32 characters is what distinguishes
// a capability-bearing alias (gonk.triage.p75.i35.a1.<26 chars of crypto/rand>)
// from the old deterministic one (gonk.triage.p75.i35.a1), which anyone could
// reconstruct from an issue number. Without this, an unauthenticated GET would
// be guessable, and the alias would not be a capability at all.
//
// 26 base32 chars is 130 bits, which is how 128 bits of randomness encodes.
func aliasHasNonce(alias string) bool {
	i := strings.LastIndex(alias, ".")
	if i < 0 {
		return false
	}
	nonce := alias[i+1:]
	if len(nonce) != 26 {
		return false
	}
	// RFC 4648 base32 alphabet, upper-case, no padding.
	for _, c := range nonce {
		if !strings.ContainsRune(base32Alphabet, c) {
			return false
		}
	}
	return true
}

func (h *handler) putPrompt(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if !aliasHasNonce(alias) {
		writeError(w, http.StatusBadRequest, "alias must carry a 26-character base32 nonce")
		return
	}
	var req meterapi.PromptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed prompt body")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is empty")
		return
	}
	if err := h.svc.PutPrompt(r.Context(), alias, req); err != nil {
		writeError(w, http.StatusInternalServerError, "store prompt")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putTrace records what a collector observed for one (session, attempt).
//
// APPEND SEMANTICS: a report adds to what was already seen rather than
// replacing it, because a collector reports incrementally as a session runs.
// The response reports the STORED completeness after folding, which may be
// worse than what was sent -- completeness only ever degrades.
func (h *handler) putTrace(w http.ResponseWriter, r *http.Request) {
	var req meterapi.TraceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed trace body")
		return
	}
	if req.SessionKey == "" {
		writeError(w, http.StatusBadRequest, "session_key is required")
		return
	}
	if req.Attempt <= 0 {
		// Attempt is part of the identity: a trace filed against attempt 0 would
		// merge evidence from runs that must stay separate.
		writeError(w, http.StatusBadRequest, "attempt must be a positive attempt number")
		return
	}
	// A REPORT WITH NO COMPLETENESS IS NOT ACCEPTED. Defaulting it would make an
	// unobserved session indistinguishable from an idle one, which is the exact
	// confusion this field exists to prevent -- so the collector must state it.
	if !trace.Completeness(req.Completeness).Valid() {
		writeError(w, http.StatusBadRequest, "completeness must be one of complete, partial, absent")
		return
	}
	stored, err := h.svc.AppendTrace(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store trace")
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// getTrace returns the evidence recorded for one (session, attempt).
//
// A SESSION WITH NO ROW IS REPORTED AS ABSENT, NOT AS 404. The caller must be
// able to tell "we observed nothing" from "we could not ask", and those are
// different failures with different correct responses: absent is a verdict
// input, a transport error is a reason to retry. Returning 404 would push that
// distinction into HTTP status handling at every call site.
func (h *handler) getTrace(w http.ResponseWriter, r *http.Request) {
	sessionKey := r.PathValue("session_key")
	if sessionKey == "" {
		writeError(w, http.StatusBadRequest, "session_key is required")
		return
	}
	attempt, err := strconv.Atoi(r.URL.Query().Get("attempt"))
	if err != nil || attempt <= 0 {
		writeError(w, http.StatusBadRequest, "attempt must be a positive attempt number")
		return
	}
	got, err := h.svc.GetTrace(r.Context(), sessionKey, attempt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read trace")
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// takePrompt is the unauthenticated one. It CONSUMES: a second GET is 410, not
// a replay, because the alias travels in pod env and process listings and a
// replayable unauthenticated read is the whole risk of exempting this route.
func (h *handler) takePrompt(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if !aliasHasNonce(alias) {
		// Unauthorized rather than 400: this route is reached without a bearer,
		// so a malformed alias should look exactly like a wrong one.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	p, found, consumed, err := h.svc.TakePrompt(r.Context(), alias)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "take prompt")
	case !found:
		// Never stored, or expired. The pod can legitimately beat the
		// controller here, so its entrypoint retries this.
		writeError(w, http.StatusNotFound, "no prompt for alias")
	case consumed:
		// Already taken. Opposite diagnosis from 404: a respawn into the same
		// pod, or a theft. The entrypoint exits with a distinct code so the
		// logs say which.
		writeError(w, http.StatusGone, "prompt already consumed")
	default:
		writeJSON(w, http.StatusOK, meterapi.PromptResponse{
			Prompt: p.Prompt, Model: p.Model, Metadata: p.Metadata,
		})
	}
}

func (h *handler) promptStatus(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if !aliasHasNonce(alias) {
		writeError(w, http.StatusBadRequest, "alias must carry a 26-character base32 nonce")
		return
	}
	p, found, err := h.svc.PromptStatus(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prompt status")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no prompt for alias")
		return
	}
	writeJSON(w, http.StatusOK, meterapi.PromptStatusResponse{
		Fetched:   !p.FetchedAt.IsZero(),
		FetchedAt: p.FetchedAt,
	})
}
