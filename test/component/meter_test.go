//go:build component

package component_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
)

// meterProc is a REAL cmd/gonk-meter subprocess (built with the testclock seam),
// talking to the shared real LiteLLM and its own fresh Postgres ledger. It is the
// thing L2 exists to exercise: the real binary, the real store, the real wire.
type meterProc struct {
	t         *testing.T
	cmd       *exec.Cmd
	URL       string
	token     string
	port      int
	ledgerDSN string
	llURL     string
	clockPath string
	logBuf    *bytes.Buffer
	client    *http.Client
}

type meterOpts struct {
	LiteLLMURL string // default: the shared LiteLLM
	LedgerDSN  string // default: a fresh ledger DB
	ListenPort int    // default: harness.PortMeter
	// SkipInitialSync leaves the meter NOT-ready (no spend sync). The skew test
	// wants this: it drives the sync itself and asserts /readyz stays 503.
	SkipInitialSync bool
}

// startMeter launches a meter subprocess, waits for /healthz, and (unless
// SkipInitialSync) drives one spend sync so it is Ready. It registers cleanup.
func (w *world) startMeter(opts meterOpts) *meterProc {
	t := w.t
	t.Helper()
	id := harnessShortID()
	if opts.LiteLLMURL == "" {
		opts.LiteLLMURL = w.LiteLLM.URL
	}
	if opts.LedgerDSN == "" {
		opts.LedgerDSN = w.PG.FreshLedger(t)
	}
	if opts.ListenPort == 0 {
		opts.ListenPort = harness.PortMeter
	}

	adminKeyFile := mustWriteSecret(t, "adminkey-"+id, w.LiteLLM.AdminKey)
	tokenVal := "meter-token-" + id
	tokenFile := mustWriteSecret(t, "metertoken-"+id, tokenVal)
	dsnFile := mustWriteSecret(t, "storedsn-"+id, opts.LedgerDSN)
	cfgFile := mustWriteSecret(t, "opercfg-"+id+".yaml", l2OperatorYAML)
	clockPath := mustWriteSecret(t, "testclock-"+id, "0")

	m := &meterProc{
		t:         t,
		URL:       "http://127.0.0.1:" + strconv.Itoa(opts.ListenPort),
		token:     tokenVal,
		port:      opts.ListenPort,
		ledgerDSN: opts.LedgerDSN,
		llURL:     opts.LiteLLMURL,
		clockPath: clockPath,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
	m.spawn(adminKeyFile, tokenFile, dsnFile, cfgFile)
	t.Cleanup(m.kill)

	if !opts.SkipInitialSync {
		m.syncSpendToReady()
	}
	return m
}

func (m *meterProc) spawn(adminKeyFile, tokenFile, dsnFile, cfgFile string) {
	cmd := exec.Command(meterBin,
		"--operator-config", cfgFile,
		"--listen", "127.0.0.1:"+strconv.Itoa(m.port),
	)
	cmd.Env = append(os.Environ(),
		"LITELLM_URL="+m.llURL,
		"LITELLM_ADMIN_KEY_FILE="+adminKeyFile,
		"GONK_METER_TOKEN_FILE="+tokenFile,
		"GONK_METER_STORE_BACKEND=postgres",
		"GONK_METER_STORE_DSN_FILE="+dsnFile,
		"GONK_TESTCLOCK_FILE="+m.clockPath,
		// Long loop intervals: the suite drives sync explicitly for determinism.
		"GONK_SYNC_SPEND_INTERVAL=1h",
		"GONK_JANITOR_INTERVAL=1h",
		"GONK_RECONCILE_KEYS_INTERVAL=1h",
		"GONK_RERESOLVE_INTERVAL=1h",
		"GONK_REFRESH_GAUGES_INTERVAL=1h",
	)
	m.logBuf = &bytes.Buffer{}
	cmd.Stdout = m.logBuf
	cmd.Stderr = m.logBuf
	if err := cmd.Start(); err != nil {
		m.t.Fatalf("start meter: %v", err)
	}
	m.cmd = cmd

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := harness.WaitFor(ctx, "meter /healthz", 30*time.Second, func() (bool, error) {
		resp, e := m.client.Get(m.URL + meterapi.HealthzPath)
		if e != nil {
			return false, nil
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200, nil
	}); err != nil {
		m.t.Fatalf("meter never became healthy: %v\n--- meter log ---\n%s", err, m.logBuf.String())
	}
}

func (m *meterProc) syncSpendToReady() {
	m.t.Helper()
	m.SyncSpend()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := harness.WaitFor(ctx, "meter /readyz", 15*time.Second, func() (bool, error) {
		return m.Readyz() == 200, nil
	}); err != nil {
		m.t.Fatalf("meter never became ready: %v\n--- meter log ---\n%s", err, m.logBuf.String())
	}
}

func (m *meterProc) kill() {
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	_ = m.cmd.Process.Kill()
	_, _ = m.cmd.Process.Wait()
	m.cmd = nil
}

// SigkillNow SIGKILLs the process without waiting for graceful shutdown -- the
// nasty mid-race crash the durability test needs.
func (m *meterProc) SigkillNow() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_, _ = m.cmd.Process.Wait()
	}
}

// Restart respawns the meter against the SAME ledger DB (durable reservations
// must survive it) and re-syncs to Ready.
func (m *meterProc) Restart() {
	m.t.Helper()
	id := harnessShortID()
	adminKeyFile := mustWriteSecret(m.t, "adminkey-r-"+id, shLiteLLM.AdminKey)
	tokenFile := mustWriteSecret(m.t, "metertoken-r-"+id, m.token)
	dsnFile := mustWriteSecret(m.t, "storedsn-r-"+id, m.ledgerDSN)
	cfgFile := mustWriteSecret(m.t, "opercfg-r-"+id+".yaml", l2OperatorYAML)
	m.spawn(adminKeyFile, tokenFile, dsnFile, cfgFile)
	m.syncSpendToReady()
}

// ---- clock (testclock offset file, seconds) ----

func (m *meterProc) setClockSeconds(secs int64) {
	if err := os.WriteFile(m.clockPath, []byte(strconv.FormatInt(secs, 10)), 0o600); err != nil {
		m.t.Fatalf("write testclock: %v", err)
	}
}

func (m *meterProc) AdvanceClock(d time.Duration) { m.setClockSeconds(int64(d / time.Second)) }
func (m *meterProc) SetClockOffset(d time.Duration) {
	m.setClockSeconds(int64(d / time.Second))
}

// ---- HTTP surface ----

func (m *meterProc) Register(project string, projectID int64, gonkYML string) (meterapi.ProjectResponse, int) {
	m.t.Helper()
	req := meterapi.ProjectRequest{
		Project: project, ProjectID: projectID, Rig: rigFor(project), GonkYML: gonkYML,
	}
	var out meterapi.ProjectResponse
	status := m.do(http.MethodPut, meterapi.ProjectPath(project), req, &out)
	return out, status
}

func (m *meterProc) MustRegister(project string, projectID int64, gonkYML string) meterapi.ProjectResponse {
	m.t.Helper()
	resp, status := m.Register(project, projectID, gonkYML)
	if status != http.StatusOK {
		m.t.Fatalf("register %q: status %d (state %q, err %q)", project, status, resp.State, resp.Error)
	}
	if resp.State != meterapi.StateActive {
		m.t.Fatalf("register %q: state %q, want active (err %q)", project, resp.State, resp.Error)
	}
	return resp
}

func (m *meterProc) Decide(req meterapi.DecideRequest) (meterapi.DecideResponse, int) {
	m.t.Helper()
	var out meterapi.DecideResponse
	status := m.do(http.MethodPost, meterapi.DecidePath, req, &out)
	return out, status
}

func (m *meterProc) SyncSpend() meterapi.SpendSyncResponse {
	m.t.Helper()
	var out meterapi.SpendSyncResponse
	status := m.do(http.MethodPost, meterapi.AdminSpendSyncPath, struct{}{}, &out)
	if status != http.StatusOK {
		m.t.Fatalf("spend sync: status %d (err %q)\n--- meter log ---\n%s", status, out.Error, m.tailLog())
	}
	return out
}

func (m *meterProc) Readyz() int {
	m.t.Helper()
	resp, err := m.client.Get(m.URL + meterapi.ReadyzPath)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Metrics scrapes /metrics and returns the raw exposition text.
func (m *meterProc) Metrics() string {
	m.t.Helper()
	resp, err := m.client.Get(m.URL + meterapi.MetricsPath)
	if err != nil {
		m.t.Fatalf("scrape metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (m *meterProc) do(method, path string, body, out any) int {
	m.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			m.t.Fatalf("meter %s %s: marshal: %v", method, path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, m.URL+path, rdr)
	if err != nil {
		m.t.Fatalf("meter %s %s: new request: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.t.Fatalf("meter %s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

func (m *meterProc) tailLog() string {
	s := m.logBuf.String()
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}

// ---- shared helpers ----

func rigFor(project string) string { return strings.ReplaceAll(project, "/", "-") }

func mustWriteSecret(t *testing.T, name, value string) string {
	t.Helper()
	p, err := shCreds.WriteSecret(name, value)
	if err != nil {
		t.Fatalf("write secret %q: %v", name, err)
	}
	return p
}

var shortSeq int64

func harnessShortID() string {
	shortSeq++
	return fmt.Sprintf("%d-%d", time.Now().UnixNano()%1e6, shortSeq)
}
