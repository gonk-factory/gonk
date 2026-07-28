package main

import "testing"

// Regression guard for gonk-4td (P0): Gas City sets GC_WEBHOOK_ARG_<name> with
// the [order.params] key VERBATIM (lower_snake_case), not uppercased. envArg
// once uppercased unconditionally, so every dispatch arg read back "" and
// gonk-dispatch died with trigger="" AFTER meter had already been asked --
// burning a budget decision and pouring nothing. envArg must read the verbatim
// name first.
func TestEnvArgReadsVerbatimName(t *testing.T) {
	t.Setenv("GC_WEBHOOK_ARG_trigger", "issue-triage")
	if got := envArg("trigger"); got != "issue-triage" {
		t.Fatalf("envArg(trigger) = %q, want issue-triage (the verbatim lower_snake_case var)", got)
	}
}

// The uppercase spelling is kept only as a fallback (a future Gas City that
// normalizes keys), and verbatim must win when both are set.
func TestEnvArgUppercaseIsOnlyAFallback(t *testing.T) {
	t.Setenv("GC_WEBHOOK_ARG_MODEL", "from-uppercase")
	if got := envArg("model"); got != "from-uppercase" {
		t.Fatalf("envArg(model) = %q, want the uppercase fallback when only it is set", got)
	}
	t.Setenv("GC_WEBHOOK_ARG_model", "from-verbatim")
	if got := envArg("model"); got != "from-verbatim" {
		t.Fatalf("envArg(model) = %q, want the verbatim spelling to win over uppercase", got)
	}
}

// gonk-uvv: the controller's gonk-gate exec orders (dispatch/sweep) run under
// Gas City, which strips TOKEN-marked env -- so GONK_GITLAB_TOKEN_FILE arrives
// empty and cfg.gl() would build a tokenless client that 401s on every broker
// forge write. loadGateConfig must fall back to the marker-free GONK_BOT_FILE
// (the same bot PAT path the controller mounts), mirroring the meter bearer-file
// fallback.
func TestGitLabTokenFileFallsBackToBotFile(t *testing.T) {
	t.Setenv("GONK_CITY", "gonk-city")
	t.Setenv("GONK_METER_TOKEN_FILE", "/secrets/meter/token")
	t.Setenv("GONK_GITLAB_TOKEN_FILE", "") // stripped in an exec order
	t.Setenv("GONK_BOT_FILE", "/secrets/gitlab-bot/token")

	cfg, err := loadGateConfig()
	if err != nil {
		t.Fatalf("loadGateConfig: %v", err)
	}
	if cfg.GitLabTokenFile != "/secrets/gitlab-bot/token" {
		t.Fatalf("GitLabTokenFile = %q, want the marker-free GONK_BOT_FILE fallback", cfg.GitLabTokenFile)
	}
}

func TestGitLabTokenFilePrefersExplicit(t *testing.T) {
	t.Setenv("GONK_CITY", "gonk-city")
	t.Setenv("GONK_METER_TOKEN_FILE", "/secrets/meter/token")
	t.Setenv("GONK_GITLAB_TOKEN_FILE", "/secrets/explicit/token")
	t.Setenv("GONK_BOT_FILE", "/secrets/gitlab-bot/token")

	cfg, err := loadGateConfig()
	if err != nil {
		t.Fatalf("loadGateConfig: %v", err)
	}
	if cfg.GitLabTokenFile != "/secrets/explicit/token" {
		t.Fatalf("GitLabTokenFile = %q, want the explicit GONK_GITLAB_TOKEN_FILE to win", cfg.GitLabTokenFile)
	}
}

func TestEnvArgInt64(t *testing.T) {
	t.Setenv("GC_WEBHOOK_ARG_project_id", "75")
	if got := envArgInt64("project_id"); got != 75 {
		t.Fatalf("envArgInt64(project_id) = %d, want 75", got)
	}
	// A missing/blank arg is 0, never a parse panic.
	if got := envArgInt64("nope"); got != 0 {
		t.Fatalf("envArgInt64(nope) = %d, want 0", got)
	}
}
