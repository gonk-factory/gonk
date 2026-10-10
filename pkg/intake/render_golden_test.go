package intake

import (
	"os"
	"testing"
)

// render_golden_test.go — run with: UPDATE_GOLDEN=1 go test ./pkg/intake/ -run TestWriteGolden
func TestWriteGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") == "" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate")
	}
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/onboarding-mr.golden.md", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
