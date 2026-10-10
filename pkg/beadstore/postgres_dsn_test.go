// The DSN shape the work store's Secret must carry (gonk-ajzq).
//
// This file is OFFLINE on purpose -- no build tag, no server. The live defect
// it pins needed neither: pgx refuses the string before it opens a socket.
package beadstore_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livePassword is the CLASS of password Vault generates for gonk_workstore. The
// real one contains a space; the quote and backslash are here because the
// keyword/value form gives each of them a meaning, and a fixture that omits
// them proves nothing about the escaping.
const livePassword = `65c1 64ff'3fe3\9ab`

// TestTheWorkStoreDSNFormSurvivesTheLivePassword is the regression test for the
// 2026-09-19 cutover outage: the gonk-workstore Secret held a URL DSN, Vault's
// password contains a space, and pgx rejected the whole connection string with
// "failed to parse as URL (net/url: invalid userinfo)". The controller's
// bootstrap-city initContainer exited 1 in a crash-loop for 11 minutes.
//
// It asserts the three facts that decided the fix, because two of them are
// counter-intuitive and the wrong one is worse than the bug:
//
//  1. a raw space in URL userinfo is refused outright;
//  2. percent-encoding parses, but Go's url.QueryEscape encodes a space as "+",
//     which userinfo takes LITERALLY -- so the obvious "just urlquery it" fix
//     authenticates with the wrong password and fails much later, at the server;
//  3. the keyword/value form with single quotes carries it exactly.
func TestTheWorkStoreDSNFormSurvivesTheLivePassword(t *testing.T) {
	host, port, db := "postgres-rw.databases-app.svc.cluster.local", "5432", "gonk"

	t.Run("URL with a raw space is refused", func(t *testing.T) {
		dsn := "postgresql://gonk_workstore:" + livePassword + "@" + host + ":" + port + "/" + db + "?sslmode=require"
		if _, err := pgxpool.ParseConfig(dsn); err == nil {
			t.Fatal("pgx accepted a URL DSN whose password holds a space; the live failure says it does not, so this test no longer pins the defect")
		} else if !strings.Contains(err.Error(), "invalid userinfo") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	})

	t.Run("urlquery is the WRONG fix: + is literal", func(t *testing.T) {
		dsn := "postgresql://gonk_workstore:" + url.QueryEscape(livePassword) + "@" + host + ":" + port + "/" + db + "?sslmode=require"
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := cfg.ConnConfig.Password; got == livePassword {
			t.Fatal("url.QueryEscape round-tripped the password; if that is now true the warning in externalsecret-gonk-workstore.yaml is stale and should be corrected")
		}
		if !strings.Contains(cfg.ConnConfig.Password, "+") {
			t.Fatalf("expected a literal + where the space was, got %q", cfg.ConnConfig.Password)
		}
	})

	t.Run("percent-encoding parses", func(t *testing.T) {
		dsn := "postgresql://gonk_workstore:" + strings.ReplaceAll(url.QueryEscape(livePassword), "+", "%20") + "@" + host + ":" + port + "/" + db + "?sslmode=require"
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.ConnConfig.Password != livePassword {
			t.Fatalf("password = %q, want %q", cfg.ConnConfig.Password, livePassword)
		}
	})

	t.Run("keyword/value is what the Secret renders", func(t *testing.T) {
		// Exactly the escaping in the ExternalSecret template: backslashes
		// first, then single quotes.
		esc := strings.ReplaceAll(livePassword, `\`, `\\`)
		esc = strings.ReplaceAll(esc, `'`, `\'`)
		dsn := "host=" + host + " port=" + port + " user=gonk_workstore password='" + esc + "' dbname=" + db + " sslmode=require"
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.ConnConfig.Password != livePassword {
			t.Fatalf("password = %q, want %q", cfg.ConnConfig.Password, livePassword)
		}
		if cfg.ConnConfig.Host != host || cfg.ConnConfig.Database != db {
			t.Fatalf("host/db lost: %s/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Database)
		}
	})
}
