package gcapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serverGrant mirrors the server's Grant just enough to read the claims we
// verify. Field order does not matter for Unmarshal; we decode, we do not
// re-encode.
type serverGrant struct {
	Kid   string `json:"kid"`
	Aud   string `json:"aud"`
	City  string `json:"city"`
	Cid   string `json:"cid"`
	Epoch int64  `json:"epoch"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
	Jti   string `json:"jti"`
	Req   string `json:"req"`
}

// serverReqDigest INDEPENDENTLY reimplements the server's ReqDigest from the
// spec (§2d) -- it deliberately does NOT call the package's reqDigest, so the
// round-trip test proves byte-compatibility with a stock gascity verifier
// rather than testing our function against itself.
func serverReqDigest(method, path, rawQuery string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	bodyHashHex := hex.EncodeToString(bodyHash[:])
	preimage := method + "\n" + path
	// canonicalizeQuery: url.ParseQuery(raw).Encode(), omitted when empty.
	if rawQuery != "" {
		if cq := canonicalizeQuery(rawQuery); cq != "" {
			preimage += "\n" + cq
		}
	}
	preimage += "\n" + bodyHashHex
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// verifyGrantAsServer replays exactly what a gascity write-auth server does with
// the X-GC-City-Write header, per spec §3/§2d, and returns the decoded grant. It
// fails the test on any check the server would reject. It reuses the request's
// method/path/query and the caller-provided body (already read from r.Body).
func verifyGrantAsServer(t *testing.T, pub ed25519.PublicKey, r *http.Request, body []byte) serverGrant {
	t.Helper()

	// CSRF gate: X-GC-Request must be present and "true".
	if got := r.Header.Get("X-GC-Request"); got != "true" {
		t.Fatalf("X-GC-Request = %q, want \"true\" (CSRF gate is always required on a signed mutation)", got)
	}

	token := r.Header.Get("X-GC-City-Write")
	if token == "" {
		t.Fatal("X-GC-City-Write header is empty")
	}

	// Split into exactly two non-empty segments and base64url-decode both.
	seg := strings.Split(token, ".")
	if len(seg) != 2 || seg[0] == "" || seg[1] == "" {
		t.Fatalf("token is not two non-empty dot-separated segments: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(seg[0])
	if err != nil {
		t.Fatalf("decode payload segment: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(seg[1])
	if err != nil {
		t.Fatalf("decode sig segment: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("sig is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	// Verify the signature over the RAW payload bytes as received, before
	// trusting any claim.
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("ed25519.Verify failed: signature does not match payload bytes on the wire")
	}

	var g serverGrant
	if err := json.Unmarshal(payload, &g); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	// Recompute req from the wire request and compare.
	wantReq := serverReqDigest(r.Method, r.URL.Path, r.URL.RawQuery, body)
	if g.Req != wantReq {
		t.Fatalf("req digest mismatch:\n  grant.req = %s\n  server    = %s", g.Req, wantReq)
	}
	return g
}

// TestRoundTripVerifiesAgainstServerAlgorithm is THE money-path test: a client
// signs a real order-run request and an independent reimplementation of the
// gascity server verifies it exactly as spec §3/§2d describe.
func TestRoundTripVerifiesAgainstServerAlgorithm(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(priv, "k1", WithCID("city_gonk"), WithEpoch(3))
	if err != nil {
		t.Fatal(err)
	}

	const city = "gonk-city"
	var verified serverGrant
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		verified = verifyGrantAsServer(t, pub, r, body)
		seen = true
		_, _ = fmt.Fprint(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, city)
	c.Signer = signer
	c.RetryBackoff = func(int) time.Duration { return 0 }

	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"rung": "qwen-local",
	}); err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if !seen {
		t.Fatal("server handler never ran")
	}

	// Claim assertions the server enforces.
	if verified.Aud != audCityWriteV2 {
		t.Fatalf("aud = %q, want %q", verified.Aud, audCityWriteV2)
	}
	if verified.City != city {
		t.Fatalf("city = %q, want %q (must equal the {cityName} path segment)", verified.City, city)
	}
	if verified.Cid != "city_gonk" {
		t.Fatalf("cid = %q, want %q", verified.Cid, "city_gonk")
	}
	if verified.Epoch != 3 {
		t.Fatalf("epoch = %d, want 3", verified.Epoch)
	}
	if verified.Kid != "k1" {
		t.Fatalf("kid = %q, want k1", verified.Kid)
	}
	if verified.Jti == "" {
		t.Fatal("jti is empty")
	}
	if verified.Exp <= verified.Iat {
		t.Fatalf("exp (%d) must be > iat (%d)", verified.Exp, verified.Iat)
	}
	if ttl := verified.Exp - verified.Iat; ttl > 120 {
		t.Fatalf("exp-iat = %ds, want <= 120s (server MaxTTL)", ttl)
	}
}

// TestPayloadFieldOrderMatchesSpec proves the marshaled payload field order is
// exactly kid, aud, city, cid, epoch, iat, exp, jti, req -- matching the
// reference minter's bytes. (Order is not load-bearing for verification, but
// matching it is the simplest way to stay byte-compatible and catch an
// accidental struct reordering.)
func TestPayloadFieldOrderMatchesSpec(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewSigner(priv, "k1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.mintGrant("POST", "/v0/city/acme/order/x/run", "", "acme", []byte(`{"vars":{}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payloadB64 := strings.SplitN(token, ".", 2)[0]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		t.Fatal(err)
	}
	got := string(payload)
	// Field keys must appear in this exact positional order.
	order := []string{`"kid"`, `"aud"`, `"city"`, `"cid"`, `"epoch"`, `"iat"`, `"exp"`, `"jti"`, `"req"`}
	prev := -1
	for _, k := range order {
		idx := strings.Index(got, k)
		if idx < 0 {
			t.Fatalf("payload missing field %s: %s", k, got)
		}
		if idx <= prev {
			t.Fatalf("field %s out of order in payload: %s", k, got)
		}
		prev = idx
	}
	// cid is emitted even when empty (no omitempty).
	if !strings.Contains(got, `"cid":""`) {
		t.Fatalf("cid must be emitted as \"\" when unset, got: %s", got)
	}
}

// TestFreshGrantEachMint proves freshness/replay defense: two mints of the same
// logical request produce DIFFERENT jti (and thus different tokens), with
// iat/exp within the TTL and exp > iat.
func TestFreshGrantEachMint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewSigner(priv, "k1", WithTTL(60*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	t1, err := s.mintGrant("POST", "/p", "", "acme", []byte(`{"vars":{}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := s.mintGrant("POST", "/p", "", "acme", []byte(`{"vars":{}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if t1 == t2 {
		t.Fatal("two mints produced identical tokens: jti is not fresh (replay risk)")
	}
	g1 := decodeGrant(t, t1)
	g2 := decodeGrant(t, t2)
	if g1.Jti == g2.Jti {
		t.Fatalf("jti reused across mints: %q", g1.Jti)
	}
	if g1.Jti == "" || len(g1.Jti) != 32 { // hex of 16 bytes
		t.Fatalf("jti = %q, want 32 hex chars (16 random bytes)", g1.Jti)
	}
	if g1.Iat != now.Unix() {
		t.Fatalf("iat = %d, want %d", g1.Iat, now.Unix())
	}
	if g1.Exp != now.Add(60*time.Second).Unix() {
		t.Fatalf("exp = %d, want %d", g1.Exp, now.Add(60*time.Second).Unix())
	}
	if g1.Exp <= g1.Iat {
		t.Fatal("exp must be > iat")
	}
	if g1.Exp-g1.Iat > 120 {
		t.Fatal("ttl must be <= 2m")
	}
}

// TestTTLClamp proves the TTL is clamped into [1s, 2m] and defaults to 60s.
func TestTTLClamp(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cases := []struct {
		name string
		opt  []SignerOption
		want time.Duration
	}{
		{"default", nil, 60 * time.Second},
		{"zero-falls-back", []SignerOption{WithTTL(0)}, 60 * time.Second},
		{"over-2m-clamped", []SignerOption{WithTTL(10 * time.Minute)}, 2 * time.Minute},
		{"under-1s-clamped", []SignerOption{WithTTL(100 * time.Millisecond)}, 1 * time.Second},
		{"in-range-kept", []SignerOption{WithTTL(90 * time.Second)}, 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSigner(priv, "k1", tc.opt...)
			if err != nil {
				t.Fatal(err)
			}
			if s.ttl != tc.want {
				t.Fatalf("ttl = %v, want %v", s.ttl, tc.want)
			}
		})
	}
}

// TestReqDigestConstruction proves req matches the spec's construction for both
// the empty-query and query cases, and that the empty-body hash is sha256("").
func TestReqDigestConstruction(t *testing.T) {
	// hex(sha256("")) -- the well-known empty-string digest.
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	h := sha256.Sum256(nil)
	if hex.EncodeToString(h[:]) != emptyHash {
		t.Fatalf("sha256(empty) = %s, want %s", hex.EncodeToString(h[:]), emptyHash)
	}

	// Empty query: preimage is method"\n"path"\n"hexbody (no query line).
	{
		body := []byte(`{"vars":{}}`)
		bh := sha256.Sum256(body)
		wantPreimage := "POST\n/v0/city/acme/order/x/run\n" + hex.EncodeToString(bh[:])
		want := sha256.Sum256([]byte(wantPreimage))
		got := reqDigest("POST", "/v0/city/acme/order/x/run", "", body)
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("empty-query req = %s, want %s", got, hex.EncodeToString(want[:]))
		}
	}

	// Empty body: hex(sha256("")).
	{
		wantPreimage := "POST\n/p\n" + emptyHash
		want := sha256.Sum256([]byte(wantPreimage))
		got := reqDigest("POST", "/p", "", nil)
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("empty-body req = %s, want %s", got, hex.EncodeToString(want[:]))
		}
	}

	// Query case: canonicalized (sorted) query on its own line. Input is
	// deliberately unsorted to prove url.ParseQuery(...).Encode() canonicalization.
	{
		bh := sha256.Sum256(nil)
		wantPreimage := "GET\n/p\na=1&b=2\n" + hex.EncodeToString(bh[:])
		want := sha256.Sum256([]byte(wantPreimage))
		got := reqDigest("GET", "/p", "b=2&a=1", nil)
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("query req = %s, want %s", got, hex.EncodeToString(want[:]))
		}
	}

	// Semantically-empty query ("&") omits the query line entirely.
	{
		bh := sha256.Sum256(nil)
		wantPreimage := "POST\n/p\n" + hex.EncodeToString(bh[:])
		want := sha256.Sum256([]byte(wantPreimage))
		got := reqDigest("POST", "/p", "&", nil)
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("empty(&)-query req = %s, want %s", got, hex.EncodeToString(want[:]))
		}
	}
}

// TestNoSignerSendsNoGrantHeaders proves the no-signer path is unchanged: no
// X-GC-City-Write and no X-GC-Request, and the request still succeeds.
func TestNoSignerSendsNoGrantHeaders(t *testing.T) {
	var hadGrant, hadCSRF bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadGrant = r.Header["X-Gc-City-Write"]
		_, hadCSRF = r.Header["X-Gc-Request"]
		_, _ = fmt.Fprint(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "gonk-city") // Signer is nil by default
	c.RetryBackoff = func(int) time.Duration { return 0 }
	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if hadGrant {
		t.Fatal("no-signer path sent an X-GC-City-Write header")
	}
	if hadCSRF {
		t.Fatal("no-signer path sent an X-GC-Request header")
	}
}

// TestErrorMapping is a table test that each auth/host status maps to the right
// typed predicate and NOT to IsNotFound, so a misconfig is diagnosable and never
// looks like "order not found". It also asserts these are not retried.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		pred    func(error) bool
		notPred func(error) bool
	}{
		{"401-missing-grant", http.StatusUnauthorized, `{"detail":"missing X-GC-City-Write grant"}`, IsWriteAuthRequired, IsNotFound},
		{"403-grant-rejected", http.StatusForbidden, `{"detail":"write grant rejected"}`, IsWriteGrantRejected, IsNotFound},
		{"403-csrf", http.StatusForbidden, `{"detail":"csrf: X-GC-Request header required on mutation endpoints"}`, IsCSRFRejected, IsWriteGrantRejected},
		{"403-read-only", http.StatusForbidden, `{"detail":"read_only: mutations disabled: server bound to non-localhost address"}`, IsReadOnly, IsWriteGrantRejected},
		{"421-host-not-allowed", http.StatusMisdirectedRequest, `{"detail":"host_not_allowed: supervisor Host header is not allowed"}`, IsHostNotAllowed, IsNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			c := New(srv.URL, "gonk-city")
			c.RetryBackoff = func(int) time.Duration { return 0 }
			_, err := c.RunOrder(context.Background(), "gonk-dispatch", nil)
			if err == nil {
				t.Fatal("want error")
			}
			if !tc.pred(err) {
				t.Fatalf("predicate false for err = %v", err)
			}
			if tc.notPred(err) {
				t.Fatalf("wrong predicate also matched err = %v", err)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1 (an auth/host rejection must not be retried into an infinite loop)", calls)
			}
			// The error text must carry a diagnostic hint.
			if !strings.Contains(err.Error(), "write-auth:") {
				t.Fatalf("error lacks a diagnostic hint: %v", err)
			}
		})
	}
}

// TestLoadSignerPEM proves a PKCS#8 PEM ed25519 key (openssl genpkey format)
// loads and produces grants that verify against the derived public key.
func TestLoadSignerPEM(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	dir := t.TempDir()
	path := filepath.Join(dir, "city.ed25519")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSigner(path, "k1")
	if err != nil {
		t.Fatalf("LoadSigner = %v", err)
	}
	token, err := s.mintGrant("POST", "/p", "", "acme", []byte(`{"vars":{}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertTokenVerifies(t, pub, token)
}

// TestParsePrivateKeyRawSeed proves the raw 32-byte seed form loads and derives
// the same key as the seed's owner.
func TestParsePrivateKeyRawSeed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	seed := priv.Seed() // 32 bytes
	got, err := ParsePrivateKey(seed)
	if err != nil {
		t.Fatalf("ParsePrivateKey(seed) = %v", err)
	}
	gotPub := got.Public().(ed25519.PublicKey)
	if !gotPub.Equal(pub) {
		t.Fatal("seed-derived key has a different public key than expected")
	}
}

// TestParsePrivateKeyRaw64 proves the raw 64-byte full private key form loads.
func TestParsePrivateKeyRaw64(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	got, err := ParsePrivateKey(priv) // 64 bytes
	if err != nil {
		t.Fatalf("ParsePrivateKey(64) = %v", err)
	}
	if !got.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("64-byte key round-trip changed the public key")
	}
}

// TestNewSignerRejectsBadInput proves construction validates the key size and kid.
func TestNewSignerRejectsBadInput(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := NewSigner(ed25519.PrivateKey{1, 2, 3}, "k1"); err == nil {
		t.Fatal("want error for a short key")
	}
	if _, err := NewSigner(priv, "  "); err == nil {
		t.Fatal("want error for an empty kid")
	}
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Fatal("want error for unrecognized key bytes")
	}
}

// --- test helpers ---

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := readCapped(r.Body, 1<<20)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func decodeGrant(t *testing.T, token string) serverGrant {
	t.Helper()
	payloadB64 := strings.SplitN(token, ".", 2)[0]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var g serverGrant
	if err := json.Unmarshal(payload, &g); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	return g
}

func assertTokenVerifies(t *testing.T, pub ed25519.PublicKey, token string) {
	t.Helper()
	seg := strings.Split(token, ".")
	if len(seg) != 2 {
		t.Fatalf("bad token: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(seg[0])
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(seg[1])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("token signature does not verify against the derived public key")
	}
}
