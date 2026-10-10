package trace

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
