package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// EnsureKey on an alias LiteLLM already knows about must fall back to
// /key/update, resolving the record's hashed token id by alias via /key/list
// (the ONLY alias filter real v1.92.0 offers). It must NOT create a second key
// under the same alias, and it must NOT try /key/info?key_alias= (which real
// v1.92.0 silently answers with the CALLER's own key).
//
// This mock speaks the verified v1.92.0 contract:
//   - /key/generate on a dup alias -> 400 with the real "already exists" wording.
//   - /key/list?key_alias= -> {"keys":[<hashed id>], ...} (hashed id, NOT sk-).
//   - /key/update identifies the record by that hashed id and echoes it back as
//     "key" -- it NEVER returns a usable plaintext token (revealed once, at
//     create). So EnsureKey's update path yields an EMPTY KeyInfo.Token.
//   - /key/info?key_alias= is a trap: if the adapter ever calls it the test
//     fails, because doing so returns the wrong key on the real proxy.
func TestHTTPAdminEnsureKeyUpdatesByAliasViaKeyList(t *testing.T) {
	const hashedID = "e7cb1b05ec35c2bf7813250158b58c5ea675ce4932f89bb2e991fb3c9cfaf188"
	var generateCalls, listCalls, updateCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/key/info":
			// Real v1.92.0 ignores key_alias here and returns the caller's own
			// key -- a silent wrong answer. The adapter must never come here.
			t.Fatalf("adapter called /key/info (query %q) -- alias lookups must go through /key/list", r.URL.RawQuery)
		case r.Method == http.MethodPost && r.URL.Path == "/key/generate":
			generateCalls++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Key with alias 'gonk-a' already exists. Unique key aliases across all keys are required.","type":"bad_request_error","code":"400"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/key/list":
			listCalls++
			if got := r.URL.Query().Get("key_alias"); got != "gonk-a" {
				t.Fatalf("key/list key_alias = %q, want gonk-a", got)
			}
			_, _ = w.Write([]byte(`{"keys":["` + hashedID + `"],"total_count":1,"current_page":1,"total_pages":1}`))
		case r.Method == http.MethodPost && r.URL.Path == "/key/update":
			updateCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["key"] != hashedID {
				t.Fatalf("key/update body key = %v, want the hashed id %q (identifies the record)", body["key"], hashedID)
			}
			// Real /key/update echoes the hashed id back as "key", never sk-.
			_, _ = w.Write([]byte(`{"key":"` + hashedID + `","key_alias":"gonk-a","max_budget":7.0}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	info, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).EnsureKey(context.Background(), KeySpec{Alias: "gonk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Alias != "gonk-a" {
		t.Fatalf("alias = %q, want gonk-a", info.Alias)
	}
	// The crux: the update path CANNOT recover a usable plaintext token, so it
	// returns an empty one meaning "unchanged". Returning the hashed id here
	// (as the old code would) hands the project an unusable 401 credential.
	if info.Token != "" {
		t.Fatalf("token = %q, want empty -- the plaintext is unrecoverable on the update path", info.Token)
	}
	if generateCalls != 1 || listCalls != 1 || updateCalls != 1 {
		t.Fatalf("calls: generate=%d list=%d update=%d, want 1 each", generateCalls, listCalls, updateCalls)
	}
}

// A fresh alias creates: /key/generate returns the plaintext secret (flat,
// top-level "key") exactly once, and EnsureKey returns it as the usable Token.
func TestHTTPAdminEnsureKeyCreatesAndReturnsPlaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/key/generate" {
			_, _ = w.Write([]byte(`{"key":"sk-FAKE-TEST-TOKEN-NOT-REAL","key_alias":"gonk-a","max_budget":5.0,"token":"e7cb1b05..."}`))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	info, err := NewHTTPAdmin(srv.URL, "k", srv.Client()).EnsureKey(context.Background(), KeySpec{Alias: "gonk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Token != "sk-FAKE-TEST-TOKEN-NOT-REAL" {
		t.Fatalf("token = %q, want the plaintext secret", info.Token)
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
