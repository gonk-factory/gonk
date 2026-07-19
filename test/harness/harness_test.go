package harness_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// WaitFor is the ONLY way anything in this harness waits. A sleep is a bug.
func TestWaitForSucceedsOnPredicate(t *testing.T) {
	start := time.Now()
	var n int
	err := harness.WaitFor(context.Background(), "counter reaches 3", 2*time.Second, func() (bool, error) {
		n++
		return n >= 3, nil
	})
	if err != nil {
		t.Fatalf("WaitFor = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("WaitFor slept instead of polling: took %s", time.Since(start))
	}
}

// The failure message must name the thing that never happened, or a 25-minute e2e
// failure is undiagnosable.
func TestWaitForTimesOutWithADescription(t *testing.T) {
	err := harness.WaitFor(context.Background(), "gitlab becomes ready", 100*time.Millisecond,
		func() (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !strings.Contains(err.Error(), "gitlab becomes ready") {
		t.Fatalf("useless timeout message: %v", err)
	}
	if !errors.Is(err, harness.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// A predicate that errors hard (not "not yet") must abort immediately: waiting two
// more minutes on a 401 is a waste of everyone's time.
func TestWaitForAbortsOnFatalPredicateError(t *testing.T) {
	boom := errors.New("401 unauthorized")
	err := harness.WaitFor(context.Background(), "x", 5*time.Second, func() (bool, error) {
		return false, harness.Fatal(boom)
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fatal error", err)
	}
}

// Podman under WSL has broken CNI bridge networking. Every container run must
// carry --network=host, and the harness must decide that itself.
func TestRuntimeAddsHostNetworkForPodman(t *testing.T) {
	rt := harness.Runtime{Bin: "podman", Kind: harness.Podman, HostNetwork: true}
	args := rt.RunArgs("gonk-stub", "gonk/stubmodel:test", nil, []string{"-addr", ":8081"})
	if !containsArg(args, "--network=host") {
		t.Fatalf("podman run must use host networking (broken CNI bridge): %v", args)
	}
	// ...and therefore must NOT publish ports: -p is meaningless (and an error)
	// on the host network.
	if containsArg(args, "-p") {
		t.Fatalf("--network=host and -p are mutually exclusive: %v", args)
	}
}

func TestRuntimePublishesPortsForDocker(t *testing.T) {
	rt := harness.Runtime{Bin: "docker", Kind: harness.Docker}
	args := rt.RunArgs("gonk-stub", "gonk/stubmodel:test", map[int]int{8081: 8081}, nil)
	if !containsArg(args, "-p") {
		t.Fatalf("docker should publish ports: %v", args)
	}
}

// The credential file must never be readable by anyone else, and must never live
// inside the repo. Both are cheap to get wrong and expensive to discover.
func TestCredsAreOutsideTheRepoAndMode0600(t *testing.T) {
	dir := t.TempDir()
	c, err := harness.NewCreds(dir, "run123")
	if err != nil {
		t.Fatalf("NewCreds = %v", err)
	}
	p, err := c.WriteSecret("gitlab-root.token", "glpat-"+strings.Repeat("x", 20))
	if err != nil {
		t.Fatalf("WriteSecret = %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	wd, _ := os.Getwd()
	if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("secret written INSIDE the repo tree: %s", p)
	}
}

// Generated, never fixed: a committed test token is a committed secret.
func TestGeneratedTokensAreRandom(t *testing.T) {
	a, _ := harness.RandomToken(32)
	b, _ := harness.RandomToken(32)
	if a == b {
		t.Fatal("RandomToken is not random")
	}
	if len(a) < 32 {
		t.Fatalf("token too short for ghook.MinSecretLen: %d", len(a))
	}
}

// ---------------------------------------------------------------- write-auth (D1)

// The load-bearing D1 test: a per-run minted keypair must produce an
// X-GC-City-Write grant that a verifier holding ONLY the public "kid:base64"
// verifyKey accepts. This is what lets the harness dispatch through the
// grant-gated controller; without it, every dispatch is a 401.
func TestMintedWriteAuthKeySignsAGrantAStubVerifierAccepts(t *testing.T) {
	dir := t.TempDir()
	c, err := harness.NewCreds(dir, "wa-run")
	if err != nil {
		t.Fatalf("NewCreds = %v", err)
	}
	wa, err := c.NewWriteAuth("gonk-e2e", "")
	if err != nil {
		t.Fatalf("NewWriteAuth = %v", err)
	}

	// The verifier is the controller: it knows only the PUBLIC verifyKey.
	pub := parseVerifyKey(t, wa.VerifyKey(), "gonk-e2e")
	verifier := newStubGasCityVerifier(t, pub)

	// L1 path: an in-process signer over the minted private key.
	signer, err := wa.Signer()
	if err != nil {
		t.Fatalf("Signer = %v", err)
	}
	client := gcapi.New(verifier.URL, "gonk-city")
	client.Signer = signer

	if _, err := client.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"bead_anchor": "gonk:42:issue:3",
	}); err != nil {
		t.Fatalf("grant-gated RunOrder rejected a validly-signed request: %v", err)
	}
	if verifier.accepted != 1 {
		t.Fatalf("verifier accepted %d grants, want 1", verifier.accepted)
	}

	// And the L2/L3 path: the SAME key loaded from the PEM file gcapi.LoadSigner
	// reads must verify identically.
	loaded, err := gcapi.LoadSigner(wa.KeyFile(), wa.KID)
	if err != nil {
		t.Fatalf("LoadSigner(%s) = %v", wa.KeyFile(), err)
	}
	client2 := gcapi.New(verifier.URL, "gonk-city")
	client2.Signer = loaded
	if _, err := client2.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("file-loaded signer rejected: %v", err)
	}
	if verifier.accepted != 2 {
		t.Fatalf("verifier accepted %d grants total, want 2", verifier.accepted)
	}
}

// A grant signed by a DIFFERENT key must be rejected -- otherwise the "acceptance"
// above proves nothing.
func TestStubVerifierRejectsAForeignKey(t *testing.T) {
	dir := t.TempDir()
	c, _ := harness.NewCreds(dir, "wa-foreign")
	good, _ := c.NewWriteAuth("gonk-e2e", "")
	other, _ := c.NewWriteAuth("gonk-e2e", "other.pem")

	verifier := newStubGasCityVerifier(t, good.PublicKey())
	signer, _ := other.Signer() // signs with the WRONG private key
	client := gcapi.New(verifier.URL, "gonk-city")
	client.Signer = signer

	_, err := client.RunOrder(context.Background(), "gonk-dispatch", nil)
	if err == nil {
		t.Fatal("verifier accepted a grant signed by a foreign key")
	}
	if verifier.accepted != 0 {
		t.Fatalf("verifier accepted %d, want 0", verifier.accepted)
	}
}

// The verify key must be exactly "kid:base64(std of the raw 32-byte pubkey)", the
// format chart gascity.writeAuth.verifyKey / GC_CITY_WRITE_PUBKEY require, and the
// kid must be recoverable by splitting on the first ':' (how the chart derives
// GONK_GC_WRITE_KEY_ID).
func TestVerifyKeyFormatMatchesTheChartContract(t *testing.T) {
	dir := t.TempDir()
	c, _ := harness.NewCreds(dir, "vk-fmt")
	wa, _ := c.NewWriteAuth("k1", "")

	vk := wa.VerifyKey()
	kid, b64, ok := strings.Cut(vk, ":")
	if !ok {
		t.Fatalf("verifyKey %q has no ':' separator", vk)
	}
	if kid != "k1" {
		t.Fatalf("kid = %q, want k1", kid)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("verifyKey base64 is not STANDARD base64: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Fatalf("decoded pubkey is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	if !ed25519.PublicKey(raw).Equal(wa.PublicKey()) {
		t.Fatal("verifyKey does not encode the actual public key")
	}
}

// The minted private key must be a real 0600 PEM PKCS#8 file that gcapi.LoadSigner
// accepts, and it must live outside the repo tree (secrets-scan territory).
func TestWriteAuthKeyFileIsPEMAndMode0600OutsideRepo(t *testing.T) {
	dir := t.TempDir()
	c, _ := harness.NewCreds(dir, "wa-file")
	wa, err := c.NewWriteAuth("gonk-e2e", "")
	if err != nil {
		t.Fatalf("NewWriteAuth = %v", err)
	}
	fi, err := os.Stat(wa.KeyFile())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(string(wa.PrivateKeyPEM()), "BEGIN PRIVATE KEY") {
		t.Fatal("PrivateKeyPEM is not a PKCS#8 PEM block")
	}
	if _, err := gcapi.LoadSigner(wa.KeyFile(), wa.KID); err != nil {
		t.Fatalf("gcapi.LoadSigner cannot read the minted key file: %v", err)
	}
}

// ---------------------------------------------------------------- clock (HB-3)

func TestTestClockAdvanceIsMonotone(t *testing.T) {
	dir := t.TempDir()
	c, _ := harness.NewCreds(dir, "clock")
	tc, err := harness.NewTestClock(c)
	if err != nil {
		t.Fatalf("NewTestClock = %v", err)
	}
	if got := readClock(t, tc.Path()); got != 0 {
		t.Fatalf("initial offset = %d, want 0", got)
	}
	if err := tc.Advance(90 * time.Second); err != nil {
		t.Fatalf("Advance = %v", err)
	}
	if got := readClock(t, tc.Path()); got != 90 {
		t.Fatalf("offset = %d, want 90", got)
	}
	if err := tc.Advance(-1 * time.Second); err == nil {
		t.Fatal("Advance accepted a backwards jump; it must be monotone")
	}
	// SetOffset is the one escape hatch for the backwards-clock test.
	if err := tc.SetOffset(10 * time.Second); err != nil {
		t.Fatalf("SetOffset = %v", err)
	}
	if got := readClock(t, tc.Path()); got != 10 {
		t.Fatalf("offset after SetOffset = %d, want 10", got)
	}
}

// ---------------------------------------------------------------- ports

func TestCheckPortsFreeNamesEveryOccupiedPort(t *testing.T) {
	ln1 := mustListen(t)
	defer func() { _ = ln1.Close() }()
	ln2 := mustListen(t)
	defer func() { _ = ln2.Close() }()
	p1 := portOf(t, ln1)
	p2 := portOf(t, ln2)

	err := harness.CheckPortsFree(p1, p2)
	if err == nil {
		t.Fatal("CheckPortsFree returned nil for two occupied ports")
	}
	// It must name EVERY occupied port, not just the first.
	if !strings.Contains(err.Error(), strconv.Itoa(p1)) || !strings.Contains(err.Error(), strconv.Itoa(p2)) {
		t.Fatalf("error must name both %d and %d: %v", p1, p2, err)
	}
}

func TestCheckPortsFreePassesForAFreePort(t *testing.T) {
	ln := mustListen(t)
	p := portOf(t, ln)
	_ = ln.Close() // now free
	if err := harness.CheckPortsFree(p); err != nil {
		t.Fatalf("CheckPortsFree(%d) = %v, want nil", p, err)
	}
}

// ---------------------------------------------------------------- doctor

// The doctor must fail fast, name the missing tool, and hand back a remedy --
// never "something went wrong".
func TestDoctorReportsMissingToolsWithARemedy(t *testing.T) {
	missing := map[string]bool{"kind": true, "helm": true}
	opts := harness.DoctorOptions{
		LookPath: func(bin string) (string, error) {
			if missing[bin] {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + bin, nil
		},
		Ports:         []int{}, // nothing to check; keep the doctor deterministic
		AvailMemBytes: func() (uint64, bool) { return 32 * 1024 * 1024 * 1024, true },
		LedgerBackend: "postgres",
	}
	rep := harness.Doctor(context.Background(), opts)

	byName := map[string]harness.Check{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	for _, tool := range []string{"kind", "helm"} {
		c, ok := byName[tool]
		if !ok {
			t.Fatalf("doctor did not check for %s", tool)
		}
		if c.OK {
			t.Fatalf("%s check passed but the tool is missing", tool)
		}
		if c.Remedy == "" {
			t.Fatalf("%s failure has no remedy -- 'something went wrong' is banned", tool)
		}
	}
	if c := byName["kubectl"]; !c.OK {
		t.Fatalf("kubectl is present but its check failed: %+v", c)
	}
}

// An unset/invalid ledger backend is a Required failure with the D2 remedy.
func TestDoctorFailsOnMissingLedgerBackend(t *testing.T) {
	opts := harness.DoctorOptions{
		LookPath:      func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		Ports:         []int{},
		AvailMemBytes: func() (uint64, bool) { return 32 * 1024 * 1024 * 1024, true },
		LedgerBackend: "", // unset
		SkipL3:        true,
	}
	rep := harness.Doctor(context.Background(), opts)
	if rep.OK() {
		t.Fatal("doctor passed with no ledger backend set")
	}
	var found bool
	for _, c := range rep.Failures() {
		if strings.Contains(c.Name, "GONK_METER_STORE_BACKEND") {
			found = true
			if !strings.Contains(c.Remedy, "postgres") {
				t.Fatalf("ledger remedy must name postgres: %q", c.Remedy)
			}
		}
	}
	if !found {
		t.Fatal("doctor did not flag the missing ledger backend")
	}
}

// A low-memory box is a Required failure (gitlab-ce alone wants ~4 GiB).
func TestDoctorFailsOnLowMemory(t *testing.T) {
	opts := harness.DoctorOptions{
		LookPath:      func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		Ports:         []int{},
		AvailMemBytes: func() (uint64, bool) { return 2 * 1024 * 1024 * 1024, true }, // 2 GiB
		LedgerBackend: "postgres",
		SkipL3:        true,
	}
	rep := harness.Doctor(context.Background(), opts)
	if rep.OK() {
		t.Fatal("doctor passed with only 2 GiB free")
	}
}

// ---------------------------------------------------------------- SyntheticSession

// SyntheticSession must drive the REAL meterapi /decide -> model calls -> /outcome
// cycle: the right requests, in order, against the wire contract.
func TestSyntheticSessionDrivesDecideCallOutcome(t *testing.T) {
	model := stubmodel.New()
	model.SetScript([]stubmodel.Step{{
		Response: stubmodel.Response{Content: "ok"},
		Usage:    stubmodel.Usage{PromptTokens: 1000, CompletionTokens: 250},
	}, {
		Response: stubmodel.Response{Content: "ok"},
		Usage:    stubmodel.Usage{PromptTokens: 1000, CompletionTokens: 250},
	}})
	modelSrv := httptest.NewServer(model)
	defer modelSrv.Close()

	var gotDecide meterapi.DecideRequest
	var gotOutcome meterapi.OutcomeRequest
	var decides, outcomes int
	meter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case meterapi.DecidePath:
			decides++
			_ = json.NewDecoder(r.Body).Decode(&gotDecide)
			writeJSON(t, w, meterapi.DecideResponse{
				Decision:      meterapi.DecisionRun,
				Rung:          "qwen-local",
				Model:         "stub-qwen",
				Attempt:       1,
				ReservationID: "rsv-1",
				Metadata:      map[string]string{"gonk_bead_id": "gk-1", "gonk_rung": "qwen-local"},
				KeyRef:        meterapi.KeyRef{SecretName: "vk-acme", SecretKey: "key"},
			})
		case meterapi.OutcomePath:
			outcomes++
			_ = json.NewDecoder(r.Body).Decode(&gotOutcome)
			writeJSON(t, w, meterapi.OutcomeResponse{OK: true, RecordedAttempt: 1, Next: "done"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer meter.Close()

	var resolved bool
	sess := &harness.SyntheticSession{
		MeterURL:   meter.URL,
		ModelURL:   modelSrv.URL,
		Project:    "acme/widget",
		Rig:        "acme-widget",
		BeadID:     "gk-1",
		SessionKey: "sess-1",
		Trigger:    "scaffold",
		KeyResolver: func(ref meterapi.KeyRef) (string, error) {
			resolved = true
			if ref.SecretName != "vk-acme" {
				t.Errorf("resolver got key_ref %+v", ref)
			}
			return "sk-virtual", nil
		},
	}

	d := sess.Run(t, 2, meterapi.OutcomeSuccess)

	if d.Decision != meterapi.DecisionRun {
		t.Fatalf("decision = %q", d.Decision)
	}
	if decides != 1 || outcomes != 1 {
		t.Fatalf("decides=%d outcomes=%d, want 1 and 1", decides, outcomes)
	}
	if !resolved {
		t.Fatal("KeyResolver was never called on a run decision")
	}
	// The decide request must carry the attribution tuple verbatim and NO attempt
	// field (there is no such field on the wire).
	if gotDecide.BeadID != "gk-1" || gotDecide.Trigger != "scaffold" || gotDecide.Rig != "acme-widget" {
		t.Fatalf("decide request = %+v", gotDecide)
	}
	// The stub saw exactly 2 calls, each carrying the meter-minted metadata verbatim.
	if got := len(model.Log().Calls()); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
	if model.Log().ScriptExhausted() {
		t.Fatal("the session made an unscripted model call")
	}
	// The outcome must bind the reservation the decide opened.
	if gotOutcome.ReservationID != "rsv-1" || gotOutcome.Outcome != meterapi.OutcomeSuccess || gotOutcome.Attempt != 1 {
		t.Fatalf("outcome request = %+v", gotOutcome)
	}
}

// On a defer, the session must make ZERO model calls and report nothing: there is
// no reservation to settle and no money to spend.
func TestSyntheticSessionOnDeferSpendsNothing(t *testing.T) {
	model := stubmodel.New()
	modelSrv := httptest.NewServer(model)
	defer modelSrv.Close()

	var outcomes int
	meter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case meterapi.DecidePath:
			writeJSON(t, w, meterapi.DecideResponse{
				Decision:   meterapi.DecisionDefer,
				Reason:     meterapi.ReasonMonthlyCostExhausted,
				RetryAfter: time.Now().Add(time.Hour),
			})
		case meterapi.OutcomePath:
			outcomes++
			writeJSON(t, w, meterapi.OutcomeResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer meter.Close()

	sess := &harness.SyntheticSession{MeterURL: meter.URL, ModelURL: modelSrv.URL, BeadID: "gk-2"}
	d := sess.Run(t, 3, meterapi.OutcomeSuccess)
	if d.Decision != meterapi.DecisionDefer {
		t.Fatalf("decision = %q, want defer", d.Decision)
	}
	if got := len(model.Log().Calls()); got != 0 {
		t.Fatalf("a deferred session made %d model calls, want 0", got)
	}
	if outcomes != 0 {
		t.Fatalf("a deferred session reported %d outcomes, want 0", outcomes)
	}
}

// ---------------------------------------------------------------- helpers

// stubGasCityVerifier is the controller half of the write-auth handshake: it
// holds ONLY the public key and does exactly what the real server does --
// ed25519.Verify(pub, payloadBytesAsReceived, sig) -- answering 200 on a valid
// grant and 401 otherwise. It proves the harness's minted key is one a
// grant-gated controller would accept.
type stubGasCityVerifier struct {
	URL      string
	accepted int
}

func newStubGasCityVerifier(t *testing.T, pub ed25519.PublicKey) *stubGasCityVerifier {
	t.Helper()
	v := &stubGasCityVerifier{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GC-Request") != "true" {
			http.Error(w, "missing CSRF header", http.StatusForbidden)
			return
		}
		tok := r.Header.Get("X-GC-City-Write")
		payloadB64, sigB64, ok := strings.Cut(tok, ".")
		if !ok {
			http.Error(w, "malformed grant", http.StatusUnauthorized)
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
		if err != nil {
			http.Error(w, "bad payload", http.StatusUnauthorized)
			return
		}
		sig, err := base64.RawURLEncoding.DecodeString(sigB64)
		if err != nil {
			http.Error(w, "bad sig", http.StatusUnauthorized)
			return
		}
		if !ed25519.Verify(pub, payload, sig) {
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
			return
		}
		v.accepted++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gcapi.RunResult{Status: "queued", TrackingID: "trk-1"})
	}))
	t.Cleanup(srv.Close)
	v.URL = srv.URL
	return v
}

func parseVerifyKey(t *testing.T, verifyKey, wantKid string) ed25519.PublicKey {
	t.Helper()
	kid, b64, ok := strings.Cut(verifyKey, ":")
	if !ok || kid != wantKid {
		t.Fatalf("verifyKey %q kid mismatch (want %q)", verifyKey, wantKid)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("verifyKey base64: %v", err)
	}
	return ed25519.PublicKey(raw)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func readClock(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read clock: %v", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("parse clock %q: %v", b, err)
	}
	return n
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	p, _ := strconv.Atoi(portStr)
	return p
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
