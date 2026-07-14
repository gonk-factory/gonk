package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

const testToken = "sk-super-secret-meter-token-do-not-leak"

func TestMeterClientSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meterapi.ProjectResponse{State: meterapi.StateActive})
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	if _, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"}); err != nil {
		t.Fatalf("Register = %v", err)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("Authorization header = %q, want Bearer %s", gotAuth, testToken)
	}
}

// The token must never appear in any error string -- neither a transport
// failure nor a 5xx from meter should leak the credential into a log line.
func TestMeterClientTokenNeverInErrorString(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // closed before use: every request is a connection-refused error

		c := NewMeterClient(srv.URL, testToken, nil)
		_, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"})
		if err == nil {
			t.Fatal("want an error against a closed server")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("token leaked into transport error: %v", err)
		}
	})

	t.Run("5xx from meter", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(meterapi.ErrorResponse{Error: "internal error"})
		}))
		defer srv.Close()

		c := NewMeterClient(srv.URL, testToken, nil)
		_, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"})
		if err == nil {
			t.Fatal("want an error on 500")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("token leaked into 5xx error: %v", err)
		}
	})
}

// A 422 is a successful, idempotent registration of an INVALID config -- it
// must come back as *ErrInvalidConfig with meter's error text intact, not a
// generic error that throws the diagnostic text away.
func TestMeterClientRegister422ReturnsErrInvalidConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_ = json.NewEncoder(w).Encode(meterapi.ProjectResponse{
			Project: "group/repo",
			State:   meterapi.StateInvalid,
			Error:   ".gonk.yml: unknown field 'banana'",
		})
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	_, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"})
	var inv *ErrInvalidConfig
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v (%T), want *ErrInvalidConfig", err, err)
	}
	if inv.Response.Error != ".gonk.yml: unknown field 'banana'" {
		t.Fatalf("meter's error text did not survive: %+v", inv.Response)
	}
	if inv.Response.State != meterapi.StateInvalid {
		t.Fatalf("response state = %q, want invalid", inv.Response.State)
	}
}

// A 500 is a plain error, not *ErrInvalidConfig: it is not a verdict about the
// project, it is meter (or the network) being unavailable.
func TestMeterClientRegister500IsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(meterapi.ErrorResponse{Error: "db unavailable"})
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	_, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"})
	var inv *ErrInvalidConfig
	if errors.As(err, &inv) {
		t.Fatalf("a 500 must not be reported as *ErrInvalidConfig: %v", err)
	}
	if err == nil {
		t.Fatal("want a plain error on 500")
	}
}

// Register must build the path through meterapi.ProjectPath, which escapes the
// GitLab path_with_namespace's "/" -- a hand-built path would route to the
// wrong handler (or none).
func TestMeterClientRegisterEscapesProjectPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meterapi.ProjectResponse{State: meterapi.StateActive})
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	if _, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"}); err != nil {
		t.Fatalf("Register = %v", err)
	}
	want := meterapi.ProjectPath("group/repo")
	if gotPath != want {
		t.Fatalf("request path = %q, want %q (group%%2Frepo)", gotPath, want)
	}
	if !strings.Contains(gotPath, "group%2Frepo") {
		t.Fatalf("path %q does not contain the escaped project name", gotPath)
	}
}

// An oversized response body must be capped, not read into the heap. We prove
// the cap took effect indirectly: a body whose valid JSON closes only after
// 1<<20 bytes decodes successfully if read in full, but fails to decode if
// truncated at the cap -- exactly what LimitReader(resp.Body, 1<<20) produces.
func TestMeterClientCapsOversizedResponseBody(t *testing.T) {
	pad := strings.Repeat("a", 2<<20) // 2 MiB, well past the 1 MiB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// Manually built so the padding lands BEFORE the closing brace: if the
		// client only reads the first 1<<20 bytes, it reads mid-string and the
		// JSON is invalid; only reading the whole 2 MiB body would parse clean.
		_, _ = fmt.Fprintf(w, `{"project":"group/repo","state":"active","error":"%s"}`, pad)
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	_, err := c.Register(context.Background(), meterapi.ProjectRequest{Project: "group/repo"})
	if err == nil {
		t.Fatal("want a decode error: the response must have been capped before the closing brace, proving it was not read in full")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want a decode error demonstrating the cap took effect", err)
	}
}

func TestMeterClientDeregisterIsIdempotentOn204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := NewMeterClient(srv.URL, testToken, nil)
	if err := c.Deregister(context.Background(), "group/repo"); err != nil {
		t.Fatalf("Deregister = %v", err)
	}
}
