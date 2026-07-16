package keysink

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestSlugShape(t *testing.T) {
	got := Slug("Agentic/Gonk_Project.v2")
	if !strings.HasPrefix(got, "agentic-gonk-project-v2-") {
		t.Fatalf("Slug = %q, want prefix agentic-gonk-project-v2-", got)
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Fatalf("Slug = %q, has a leading or trailing dash", got)
	}
	if strings.Contains(got, "--") {
		t.Fatalf("Slug = %q, has a doubled dash", got)
	}
}

// THE COLLISION TEST, WHICH IS THE MONEY ONE. Two projects that flattened to
// the same alias would share one LiteLLM virtual key and therefore one hard
// USD ceiling.
func TestSlugNoCollisionBetweenSlashAndDash(t *testing.T) {
	a := Slug("a/b")
	b := Slug("a-b")
	if a == b {
		t.Fatalf("Slug(\"a/b\") == Slug(\"a-b\") == %q -- two projects would share one virtual key and one budget", a)
	}
}

// Slug is a key alias. An unstable slug orphans the previous key on every
// restart.
func TestSlugIsStable(t *testing.T) {
	const project = "agentic/gonk-project"
	first := Slug(project)
	for i := 0; i < 5; i++ {
		if got := Slug(project); got != first {
			t.Fatalf("Slug(%q) = %q on call %d, want %q (stable across calls)", project, got, i, first)
		}
	}
}

// dns1123Subdomain matches RFC 1123 subdomain names: lowercase alphanumeric
// and '-', starting and ending with an alphanumeric character.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestSlugIsLegalDNS1123Subdomain(t *testing.T) {
	for _, project := range []string{
		"Agentic/Gonk_Project.v2",
		"a/b",
		"a-b",
		"Group/Repo",
		"group/repo.with.dots",
		"group/_leading-underscore",
		strings.Repeat("seg/", 60) + "leaf",
	} {
		got := Slug(project)
		if len(got) > 63 {
			t.Fatalf("Slug(%q) = %q, len %d > 63", project, got, len(got))
		}
		if !dns1123Subdomain.MatchString(got) {
			t.Fatalf("Slug(%q) = %q, not a legal DNS-1123 subdomain", project, got)
		}
	}
}

func TestMemoryPutIsIdempotent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	ref1, err := m.Put(ctx, "group/repo", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := m.Put(ctx, "group/repo", "sk-2")
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 {
		t.Fatalf("KeyRef changed across Put calls for the same project: %+v vs %+v", ref1, ref2)
	}
	if got := m.Token("group/repo"); got != "sk-2" {
		t.Fatalf("Token = %q, want sk-2 (the second Put's token)", got)
	}
}

func TestMemoryDeleteRemovesToken(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if _, err := m.Put(ctx, "group/repo", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "group/repo"); err != nil {
		t.Fatal(err)
	}
	if got := m.Token("group/repo"); got != "" {
		t.Fatalf("Token after Delete = %q, want empty", got)
	}
	// Deleting an absent key is a no-op, not an error.
	if err := m.Delete(ctx, "group/repo"); err != nil {
		t.Fatalf("Delete of an absent project errored: %v", err)
	}
}
