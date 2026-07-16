package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// EnsureKey on an alias LiteLLM already knows about must fall back to
// /key/update (looked up via /key/info), not fail and not silently create a
// second key under the same alias.
func TestHTTPAdminEnsureKeyFallsBackToUpdateOnAlreadyExists(t *testing.T) {
	var generateCalls, infoCalls, updateCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/key/generate":
			generateCalls++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"key_alias 'gonk-a' already exists"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/key/info":
			infoCalls++
			if got := r.URL.Query().Get("key_alias"); got != "gonk-a" {
				t.Fatalf("key/info key_alias = %q, want gonk-a", got)
			}
			_, _ = w.Write([]byte(`{"key":"sk-existing","key_alias":"gonk-a"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/key/update":
			updateCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["key"] != "sk-existing" {
				t.Fatalf("key/update body key = %v, want sk-existing (must identify the record to update)", body["key"])
			}
			_, _ = w.Write([]byte(`{"key":"sk-existing","key_alias":"gonk-a"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	info, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).EnsureKey(context.Background(), KeySpec{Alias: "gonk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "sk-existing" {
		t.Fatalf("token = %q, want sk-existing", info.Token)
	}
	if generateCalls != 1 || infoCalls != 1 || updateCalls != 1 {
		t.Fatalf("calls: generate=%d info=%d update=%d, want 1 each", generateCalls, infoCalls, updateCalls)
	}
}

// RotateKey must delete the old token before generating a new one -- a rotate
// that leaves the old token live defeats the point of rotating.
func TestHTTPAdminRotateKeyDeletesThenGenerates(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		switch r.URL.Path {
		case "/key/delete":
			w.WriteHeader(http.StatusOK)
		case "/key/generate":
			_, _ = w.Write([]byte(`{"key":"sk-rotated","key_alias":"gonk-a"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	info, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).RotateKey(context.Background(), KeySpec{Alias: "gonk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "sk-rotated" {
		t.Fatalf("token = %q, want sk-rotated", info.Token)
	}
	if len(order) != 2 || order[0] != "/key/delete" || order[1] != "/key/generate" {
		t.Fatalf("call order = %v, want [/key/delete /key/generate]", order)
	}
}

func TestHTTPAdminDeleteKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/delete" {
			t.Fatalf("path = %q, want /key/delete", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewHTTPAdmin(srv.URL, "k", srv.Client()).DeleteKey(context.Background(), "gonk-a"); err != nil {
		t.Fatal(err)
	}
	aliases, _ := gotBody["key_aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != "gonk-a" {
		t.Fatalf("key_aliases = %v, want [gonk-a]", gotBody["key_aliases"])
	}
}
