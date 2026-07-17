//go:build chart

package charttest

import (
	"path/filepath"
	"strings"
	"testing"
)

// Test-fixture drift found by TDD (same category as RECONCILIATION.md's Task 2
// addendum, not a chart bug): the plan text's literal was `value: "postgres"`
// (quoted), assuming the quotes the template's `| quote` puts in the RAW helm
// output survive into Object.Doc. They do not -- charttest.Objects() decodes
// each document into a map[string]any and re-marshals it with yaml.v3 to build
// Doc, and yaml.v3 omits quotes around a plain scalar like "postgres" that
// needs no escaping (verified: yaml.Marshal(map[string]any{"value":
// "postgres"}) round-trips to `value: postgres`, no quotes -- listener_test.go's
// `value: ""` assertion survives only because an EMPTY scalar DOES need
// quoting to stay a string instead of null). So every occurrence below reads
// `value: postgres` (unquoted), matching what Doc will actually contain; the
// real rendered YAML is quoted either way, and this changes nothing about
// chart behavior.

// The DEFAULT: dolt.enabled=true backs BEADS ONLY. ledger.backend=postgres,
// ledger.postgres.mode=shared is the default (RECONCILED, Task 0 -- no
// "ledger.backend=dolt" state exists any more), which renders NO Cluster CR
// (see TestSharedPostgresRendersNoDatabase in Step 5b). Minimum() already sets
// dolt.image.tag.
func TestBundledDoltRendersAStatefulSetForBeadsOnly(t *testing.T) {
	out := Render(t, Minimum()...)
	ss := MustObject(t, out, "StatefulSet", "gonk-dolt")
	if !strings.Contains(ss.Doc, "volumeClaimTemplates") {
		t.Fatal("the bundled Dolt has no persistent volume; a beads store that loses state on restart is fail-open on Gas City state")
	}
	MustObject(t, out, "Service", "gonk-dolt")
	d := MustObject(t, out, "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, `GONK_METER_STORE_BACKEND`) || !strings.Contains(d.Doc, `value: postgres`) {
		t.Fatalf("meter was not told to use postgres (the only valid backend):\n%s", d.Doc)
	}
}

// Not in the plan's verbatim Task 6 text, but the exact regression Task 0
// reconciliation item 4 (chart/RECONCILIATION.md) fixed: dolt.image.registry
// is set explicitly to docker.io in values.yaml so the per-image override in
// `gonk.image` (Task 1) wins over the global registry.orac.local default.
// Without that override this StatefulSet would render an unpullable
// registry.orac.local/agentic/gonk-project/dolthub/dolt-sql-server path for a
// third-party upstream image nobody ever builds into the private registry.
func TestBundledDoltUsesThePublicDockerHubImageNotThePrivateRegistry(t *testing.T) {
	ss := MustObject(t, Render(t, Minimum()...), "StatefulSet", "gonk-dolt")
	if !strings.Contains(ss.Doc, "image: docker.io/dolthub/dolt-sql-server:") {
		t.Fatalf("gonk-dolt StatefulSet does not use the public docker.io/dolthub image:\n%s", ss.Doc)
	}
	if strings.Contains(ss.Doc, "registry.orac.local") {
		t.Fatalf("gonk-dolt StatefulSet's image resolved to the PRIVATE registry; Dolt is a third-party image never built into registry.orac.local/agentic/gonk-project:\n%s", ss.Doc)
	}
}

// dolt.enabled=false: NO StatefulSet, and the CONTROLLER (beads) points at the
// external host. Beads has no Postgres path, so external Dolt is the only
// alternative -- and, RECONCILED (Task 0), this has NO EFFECT on the ledger,
// which is Postgres regardless.
func TestExternalDoltRendersNoStatefulSetAndDoesNotTouchLedger(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "dolt.enabled=false",
		"--set", "dolt.external.host=dolt.databases.svc")...)
	NoObject(t, out, "StatefulSet", "gonk-dolt")
	d := MustObject(t, out, "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, `value: postgres`) {
		t.Fatal("disabling the bundled Dolt (a BEADS-only toggle) must not change meter's ledger backend")
	}
}

// The Task 0b fallback is now simply "how it works." mode=cnpg renders a
// chart-managed Cluster CR, and -- RECONCILED, Task 0 -- the bundled Dolt
// StatefulSet still renders alongside it (beads is a completely separate
// component; there is no scenario where enabling the CNPG Cluster mode
// removes it).
func TestCNPGLedgerRendersAClusterAlongsideBeadsDolt(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "ledger.postgres.mode=cnpg")...)
	c := MustObject(t, out, "Cluster", "gonk-ledger-postgres")
	if c.APIVersion != "postgresql.cnpg.io/v1" {
		t.Fatalf("apiVersion = %q", c.APIVersion)
	}
	MustObject(t, out, "StatefulSet", "gonk-dolt")
	d := MustObject(t, out, "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, `value: postgres`) {
		t.Fatalf("meter is not pointed at the postgres backend:\n%s", d.Doc)
	}
}

func TestExternalPostgresRendersNoCluster(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "ledger.postgres.mode=external")...)
	NoObject(t, out, "Cluster", "gonk-ledger-postgres")
	// Beads Dolt still there -- an entirely separate component.
	MustObject(t, out, "StatefulSet", "gonk-dolt")
}

// mode: shared must add NOTHING to the cluster. If it renders a Cluster CR, it is
// about to stand up a SECOND Postgres next to the one it was told to share.
func TestSharedPostgresRendersNoDatabase(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "ledger.backend=postgres",
		"--set", "ledger.postgres.mode=shared")...)
	for _, o := range Objects(t, out) {
		if o.Kind == "Cluster" || o.Kind == "Database" {
			t.Fatalf("mode=shared rendered a %s/%s; it must consume the existing cluster, not build one",
				o.Kind, o.Metadata.Name)
		}
	}
	d := MustObject(t, out, "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, `GONK_METER_STORE_BACKEND`) || !strings.Contains(d.Doc, `value: postgres`) {
		t.Fatal("meter is not pointed at the postgres backend")
	}
}

func TestKubeconformCoreResources(t *testing.T) {
	// The default profile emits NO CRDs, so it validates against the upstream
	// Kubernetes schemas with nothing skipped. -ignore-missing-schemas is
	// deliberately NOT used: a skipped resource is an unvalidated resource.
	Kubeconform(t, Render(t, Minimum()...), "")
}

func TestKubeconformCNPGProfile(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "ledger.backend=postgres")...)
	Kubeconform(t, out, filepath.Join(ChartDir(), "tests", "crd-schemas"))
}
