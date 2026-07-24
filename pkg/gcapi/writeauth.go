package gcapi

// Gas City write-authentication (the X-GC-City-Write grant).
//
// A mutation is authorized by a single-use, request-bound ed25519 grant token
// carried in the X-GC-City-Write header. The token is
//
//	base64url_nopad(payloadJSON) "." base64url_nopad(ed25519_sig)
//
// where the signature is over the EXACT payload JSON bytes transmitted, and the
// payload binds method+path+query+body via a SHA-256 "req digest" plus
// audience/city/cid/epoch/iat/exp/jti claims. The server holds only the public
// key (selected by kid); the client (this package) holds the private key.
//
// X-GC-Request: true is an independent, always-required CSRF header sent
// alongside the grant on every signed mutation.
//
// This file mirrors the gascity contract exactly: the server does
// ed25519.Verify(pub, payloadBytesAsReceived, sig) and THEN recomputes the req
// digest from the wire method/path/query/body, so we must sign the exact bytes
// we transmit and digest the exact request line that goes on the wire.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// audCityWriteV2 is the primary/v2 audience. It is accepted by every current
// gascity server and is REQUIRED whenever the server is tenancy-scoped
// (GC_CITY_WRITE_CID set). We always mint v2 so a later tenancy rollout does not
// silently start rejecting us; on an untenanted server v2 is accepted too.
const audCityWriteV2 = "gc-city-write.v2"

// TTL bounds. The server accepts exp-iat <= MaxTTL (2m) and requires exp > iat;
// its acceptance window is skew-tolerant by 30s. We default to 60s and clamp
// into [1s, 2m] so a caller cannot mint a grant the server will structurally
// reject.
const (
	defaultTTL = 60 * time.Second
	minTTL     = 1 * time.Second
	maxTTL     = 2 * time.Minute
)

// Signer mints one fresh X-GC-City-Write grant per request. It is safe for
// concurrent use: it holds only immutable configuration and each mint draws
// fresh crypto/rand bytes for the jti.
//
// A nil *Signer on a Client means write-auth is OFF: the client sends no grant
// headers, exactly as before write-auth existed. That preserves the in-pod
// loopback path (admission by network position) and back-compat.
type Signer struct {
	key   ed25519.PrivateKey
	kid   string        // selects the server verifying key; non-empty
	cid   string        // tenancy id; "" when the server is untenanted
	epoch int64         // rotation/teardown floor; 0 unless the operator raised it
	ttl   time.Duration // grant lifetime; clamped into [minTTL, maxTTL]
}

// SignerOption configures optional Signer fields.
type SignerOption func(*Signer)

// WithCID binds grants to a tenancy id. Set this to exactly the server's
// GC_CITY_WRITE_CID when the controller is tenancy-scoped; leave unset (cid "")
// for an untenanted controller.
func WithCID(cid string) SignerOption { return func(s *Signer) { s.cid = cid } }

// WithEpoch sets the rotation/teardown epoch. Leave at 0 unless the operator
// raised GC_CITY_WRITE_EPOCH_FLOOR for revocation.
func WithEpoch(epoch int64) SignerOption { return func(s *Signer) { s.epoch = epoch } }

// WithTTL sets the grant lifetime. Values are clamped into [1s, 2m]; a
// non-positive value falls back to the 60s default.
func WithTTL(d time.Duration) SignerOption { return func(s *Signer) { s.ttl = d } }

// NewSigner builds a Signer from a full ed25519 private key (64 bytes) and a
// kid. It validates the key size and a non-empty kid, then clamps the TTL.
func NewSigner(key ed25519.PrivateKey, kid string, opts ...SignerOption) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("gcapi: ed25519 private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(key))
	}
	if strings.TrimSpace(kid) == "" {
		return nil, errors.New("gcapi: signer kid is empty (kid selects the server verifying key and must be non-empty)")
	}
	s := &Signer{key: key, kid: kid, ttl: defaultTTL}
	for _, o := range opts {
		o(s)
	}
	s.ttl = clampTTL(s.ttl)
	return s, nil
}

// LoadSigner reads a private key file and builds a Signer. The file may be a
// PEM PKCS#8 ed25519 key (what `openssl genpkey -algorithm ed25519` emits) or a
// raw 32-byte seed / 64-byte private key. See ParsePrivateKey.
//
// Callers (cmd/gonk-intake, cmd/gonk-gate) pass the path to a file-mounted key
// plus the kid, and optionally WithCID/WithEpoch/WithTTL. No key material is
// embedded in this package.
func LoadSigner(path, kid string, opts ...SignerOption) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gcapi: read write-auth key %q: %w", path, err)
	}
	key, err := ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("gcapi: load write-auth key %q: %w", path, err)
	}
	return NewSigner(key, kid, opts...)
}

// ParsePrivateKey decodes an ed25519 private key from either:
//   - a PEM PKCS#8 block (BEGIN PRIVATE KEY), the openssl genpkey format; or
//   - raw bytes: a 32-byte ed25519 seed, or a 64-byte full private key.
//
// It never accepts a public key or a non-ed25519 key.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 PEM: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM key is %T, want ed25519.PrivateKey", parsed)
		}
		return key, nil
	}
	// TRIM ONLY IF TRIMMING IS NEEDED. bytes.TrimSpace over RAW key material is
	// a corruption bug: an ed25519 key is 32/64 bytes of uniform random data, so
	// its first or last byte is an ASCII whitespace value (\t \n \v \f \r space
	// -- 6 of 256) about 4.6% of the time, and trimming those bytes turns a
	// perfectly good key into a 63-byte "unrecognized private key" that no
	// operator could diagnose. Exact-length input is therefore taken verbatim,
	// and the trim is kept only as a fallback for the common text case (a file
	// written with a trailing newline). Found by TestParsePrivateKeyRaw64, which
	// generates a fresh key each run and so trips this roughly 1 run in 22.
	raw := data
	if len(raw) != ed25519.SeedSize && len(raw) != ed25519.PrivateKeySize {
		raw = bytes.TrimSpace(data)
	}
	switch len(raw) {
	case ed25519.SeedSize: // 32-byte seed
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize: // 64-byte full private key
		return ed25519.PrivateKey(append([]byte(nil), raw...)), nil
	default:
		return nil, fmt.Errorf("unrecognized ed25519 private key: not PEM PKCS#8, and raw length %d is neither a %d-byte seed nor a %d-byte key", len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// grant is the signed payload. Field order is load-bearing only for matching
// the reference minter's bytes (json.Marshal emits struct fields in declaration
// order); it is kid, aud, city, cid, epoch, iat, exp, jti, req. cid has NO
// omitempty: the struct always emits "cid":"" when unset, exactly like the
// server's Grant.
type grant struct {
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

// mintGrant produces one fresh token for a single outgoing request. Every call
// draws a new jti and stamps iat/exp from now, so retries re-mint (the server's
// replay guard is single-use per jti). method/path/rawQuery/body MUST be the
// exact values that go on the wire, because the server recomputes the req
// digest from what it receives.
func (s *Signer) mintGrant(method, path, rawQuery, city string, body []byte, now time.Time) (string, error) {
	jti, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("gcapi: generate jti: %w", err)
	}
	g := grant{
		Kid:   s.kid,
		Aud:   audCityWriteV2,
		City:  city,
		Cid:   s.cid,
		Epoch: s.epoch,
		Iat:   now.Unix(),
		Exp:   now.Add(s.ttl).Unix(),
		Jti:   jti,
		Req:   reqDigest(method, path, rawQuery, body),
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("gcapi: marshal grant: %w", err)
	}
	sig := ed25519.Sign(s.key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// reqDigest is the request binding hex(sha256(preimage)) where
//
//	preimage = method "\n" path [ "\n" canonicalQuery ] "\n" hex(sha256(body))
//
// The query line is omitted entirely when the canonical query is empty, which
// preserves the query-less preimage the server computes for an unqueried route.
func reqDigest(method, path, rawQuery string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	var p strings.Builder
	p.WriteString(method)
	p.WriteByte('\n')
	p.WriteString(path)
	if cq := canonicalizeQuery(rawQuery); cq != "" {
		p.WriteByte('\n')
		p.WriteString(cq)
	}
	p.WriteByte('\n')
	p.WriteString(hex.EncodeToString(bodyHash[:]))
	sum := sha256.Sum256([]byte(p.String()))
	return hex.EncodeToString(sum[:])
}

// canonicalizeQuery mirrors the server's url.ParseQuery(raw).Encode(): a sorted,
// percent-encoded form. A raw query that fails to parse falls back to its raw
// bytes; a semantically-empty query ("" or "&") canonicalizes to "".
func canonicalizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	return vals.Encode()
}

// randomHex returns hex of n crypto/rand bytes.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func clampTTL(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return defaultTTL
	case d < minTTL:
		return minTTL
	case d > maxTTL:
		return maxTTL
	default:
		return d
	}
}
