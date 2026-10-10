//go:build linux

package catrust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// childEnv is the variable that turns this test binary into the probe.
const childEnv = "GONK_CATRUST_PROBE"

// systemCertDir is the first element of the value the chart renders. It is a
// literal there too (gonk.caTrustEnv); internal/charttest holds the two equal.
const systemCertDir = "/etc/ssl/certs"

// goCertFiles is crypto/x509's own list (root_linux.go, certFiles): the bundle
// Go reads when SSL_CERT_FILE is unset is the first of these that exists.
var goCertFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// probe is what the child is asked and what it answers.
type probe struct {
	// URLs are dialled with a zero tls.Config: the process's own roots.
	URLs map[string]string `json:"urls,omitempty"`
	// Bundle is a PEM file of roots; the child counts how many of them its own
	// roots vouch for.
	Bundle string `json:"bundle,omitempty"`

	Trusted       map[string]bool   `json:"trusted,omitempty"`
	Errors        map[string]string `json:"errors,omitempty"`
	BundleRoots   int               `json:"bundle_roots"`
	BundleVouched int               `json:"bundle_vouched"`
}

func TestMain(m *testing.M) {
	if raw := os.Getenv(childEnv); raw != "" {
		os.Exit(runProbe(raw))
	}
	os.Exit(m.Run())
}

// runProbe is the child. It never sets RootCAs: whatever it trusts, it trusts
// because of the environment it was started in.
func runProbe(raw string) int {
	var p probe
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 2
	}
	p.Trusted, p.Errors = map[string]bool{}, map[string]string{}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	for name, u := range p.URLs {
		resp, err := client.Get(u)
		if err != nil {
			p.Errors[name] = err.Error()
			p.Trusted[name] = false
			continue
		}
		_ = resp.Body.Close()
		p.Trusted[name] = true
	}
	if p.Bundle != "" {
		data, err := os.ReadFile(p.Bundle)
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			return 2
		}
		now := time.Now()
		for {
			var b *pem.Block
			b, data = pem.Decode(data)
			if b == nil {
				break
			}
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil || now.After(c.NotAfter) || now.Before(c.NotBefore) {
				continue
			}
			p.BundleRoots++
			// Roots nil: the system pool, as this process loaded it.
			if _, err := c.Verify(x509.VerifyOptions{CurrentTime: now}); err == nil {
				p.BundleVouched++
			}
		}
	}
	out, _ := json.Marshal(p)
	fmt.Println(string(out))
	return 0
}

// ask runs the probe as a child with exactly env added to a scrubbed
// environment (no SSL_CERT_* inherited from whoever runs the gate).
func ask(t *testing.T, p probe, env map[string]string) probe {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SSL_CERT_FILE=") || strings.HasPrefix(kv, "SSL_CERT_DIR=") || strings.HasPrefix(kv, "GODEBUG=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, childEnv+"="+string(raw))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe child with %v: %v", env, err)
	}
	var got probe
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("probe child printed %q: %v", out, err)
	}
	return got
}

// site is a throwaway CA, its certificate in a directory of its own, and an
// HTTPS server whose certificate that CA signed.
type site struct {
	Dir  string // holds exactly one file, File
	File string
	URL  string
}

func newSite(t *testing.T, name string) site {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name + " throwaway CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are the point of half the cases
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "ca.crt")
	// The PEM type is assembled so this file holds no certificate header.
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTI" + "FICATE", Bytes: caDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return site{Dir: dir, File: file, URL: srv.URL}
}

// THE MECHANISM, hermetically: two CAs that are in no system store, each in a
// directory of its own. "first" stands where the system directory stands in
// the chart's value, "private" where the mounted bundle's directory does.
func TestCertDirListIsAdditiveAndEachHalfIsNeeded(t *testing.T) {
	first, private := newSite(t, "first"), newSite(t, "private")
	empty := t.TempDir()
	urls := map[string]string{"first": first.URL, "private": private.URL}

	cases := []struct {
		name        string
		env         map[string]string
		first, priv bool
	}{
		// What the chart renders: a list. Both are trusted.
		{"the list", map[string]string{"SSL_CERT_DIR": first.Dir + ":" + private.Dir}, true, true},
		{"the list, other order", map[string]string{"SSL_CERT_DIR": private.Dir + ":" + first.Dir}, true, true},
		// Either directory alone trusts only its own CA: the variable REPLACES
		// the directory list, so naming only the mounted directory would drop
		// whatever the first one held.
		{"first only", map[string]string{"SSL_CERT_DIR": first.Dir}, true, false},
		{"private only", map[string]string{"SSL_CERT_DIR": private.Dir}, false, true},
		// Neither is in a system store: with no variable, neither is trusted.
		{"no variable", nil, false, false},
		// SSL_CERT_FILE names ONE file and the directories are still read.
		{"file and directory", map[string]string{"SSL_CERT_FILE": private.File, "SSL_CERT_DIR": first.Dir}, true, true},
		{"file alone", map[string]string{"SSL_CERT_FILE": private.File, "SSL_CERT_DIR": empty}, false, true},
		// A SSL_CERT_FILE that names NO file (what an image ENV does when no CA
		// is mounted) is not an error; the directories still decide.
		{"missing file", map[string]string{"SSL_CERT_FILE": filepath.Join(empty, "absent.crt"), "SSL_CERT_DIR": first.Dir}, true, false},
	}
	for _, tc := range cases {
		got := ask(t, probe{URLs: urls}, tc.env)
		if got.Trusted["first"] != tc.first || got.Trusted["private"] != tc.priv {
			t.Errorf("%s (%v): first trusted=%v private trusted=%v, want %v %v; errors %v",
				tc.name, tc.env, got.Trusted["first"], got.Trusted["private"], tc.first, tc.priv, got.Errors)
		}
		for name, msg := range got.Errors {
			if !strings.Contains(msg, "certificate signed by unknown authority") {
				t.Errorf("%s: %s failed for another reason than trust: %s", tc.name, name, msg)
			}
		}
	}
}

// THE CHART'S VALUE, on a real system store: with
// SSL_CERT_DIR=/etc/ssl/certs:<mounted directory> a process trusts the private
// CA AND still vouches for every root in the system bundle; and a process whose
// only root is the private CA vouches for none of them -- which is what
// "replace" would have done to every public endpoint, and what shows the count
// can be zero.
func TestTheChartValueKeepsThePublicRootsAndAddsThePrivateCA(t *testing.T) {
	bundle := ""
	for _, f := range goCertFiles {
		if _, err := os.Stat(f); err == nil {
			bundle = f
			break
		}
	}
	if bundle == "" {
		t.Fatalf("this machine has none of the system bundles Go looks for (%v), so there are no public roots to keep and the claim cannot be tested here; install the distribution's CA certificates package", goCertFiles)
	}
	private := newSite(t, "private")
	urls := map[string]string{"private": private.URL}

	plain := ask(t, probe{URLs: urls, Bundle: bundle}, nil)
	if plain.BundleRoots == 0 || plain.BundleVouched != plain.BundleRoots {
		t.Fatalf("with no variable set the process vouches for %d of the %d current roots in %s; want all of them, and more than none", plain.BundleVouched, plain.BundleRoots, bundle)
	}
	if plain.Trusted["private"] {
		t.Fatal("the throwaway CA is trusted with no variable set; the test proves nothing")
	}

	chart := ask(t, probe{URLs: urls, Bundle: bundle}, map[string]string{"SSL_CERT_DIR": systemCertDir + ":" + private.Dir})
	if !chart.Trusted["private"] {
		t.Errorf("SSL_CERT_DIR=%s:<mounted dir>: the private CA is NOT trusted: %v", systemCertDir, chart.Errors)
	}
	if chart.BundleVouched != plain.BundleRoots {
		t.Errorf("SSL_CERT_DIR=%s:<mounted dir>: the process vouches for %d of the %d public roots; the private CA must be ADDED, not put in their place", systemCertDir, chart.BundleVouched, plain.BundleRoots)
	}

	replaced := ask(t, probe{URLs: urls, Bundle: bundle}, map[string]string{"SSL_CERT_FILE": private.File, "SSL_CERT_DIR": private.Dir})
	if !replaced.Trusted["private"] || replaced.BundleVouched != 0 {
		t.Errorf("with the private CA as the ONLY root the process trusts private=%v and vouches for %d public roots; want true and 0", replaced.Trusted["private"], replaced.BundleVouched)
	}
}
