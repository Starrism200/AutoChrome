package chrome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerResolveAndUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUTOCHROME_CHROME_DIR", dir)

	binaryDir := filepath.Join(dir, "1.2.3", "opt", "google", "chrome")
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binaryDir, "chrome")
	script := "#!/bin/sh\necho 'Google Chrome 1.2.3'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	v, err := m.Get("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "1.2.3" || !v.Managed {
		t.Fatalf("unexpected version: %+v", v)
	}
	if _, err := m.Use("1.2.3"); err != nil {
		t.Fatal(err)
	}
	cur, err := m.Current()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Version != "1.2.3" {
		t.Fatalf("current = %+v", cur)
	}
	path, err := m.Resolve("1.2.3", "/usr/bin/google-chrome")
	if err != nil || path != binary {
		t.Fatalf("Resolve = %q, %v", path, err)
	}
	if err := m.Remove("1.2.3"); err == nil {
		t.Fatal("expected refusal to remove current version")
	}
}

func TestSafeVersion(t *testing.T) {
	if got := safeVersion("156.0.8078.79"); got != "156.0.8078.79" {
		t.Fatalf("safeVersion = %q", got)
	}
	if got := safeVersion("../x y"); got == ".." || got == "" {
		t.Fatalf("unsafe result %q", got)
	}
}
