package cleaner

import (
	"os"
	"path/filepath"
	"testing"

	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestContainerPreservesLogin(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "default-test")
	meta, err := envdir.CreateAt(dir, "default", "test", fingerprint.Generate("test"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Default", "Cache", "data"), 100)
	writeFile(t, filepath.Join(dir, "Default", "History"), 50)
	writeFile(t, filepath.Join(dir, "Default", "Cookies"), 20)
	writeFile(t, filepath.Join(dir, "Default", "Local Storage", "leveldb", "data"), 30)
	writeFile(t, filepath.Join(dir, "Default", "Network", "Cookies"), 25)
	writeFile(t, filepath.Join(dir, "Default", "Preferences"), 15)
	writeFile(t, filepath.Join(dir, "Local State"), 10)
	writeFile(t, filepath.Join(dir, "downloads", "a.bin"), 40)
	writeFile(t, filepath.Join(dir, "screenshots", "a.png"), 50)
	writeFile(t, filepath.Join(dir, "attachments", "a.txt"), 60)

	res, err := Container(meta, Options{Cache: true, Downloads: true, Screenshots: true, Attachments: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.FreedBytes == 0 {
		t.Fatalf("expected freed bytes: %+v", res)
	}
	for _, path := range []string{
		filepath.Join(dir, "Default", "Cache"),
		filepath.Join(dir, "Default", "History"),
		filepath.Join(dir, "downloads", "a.bin"),
		filepath.Join(dir, "screenshots", "a.png"),
		filepath.Join(dir, "attachments", "a.txt"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", path)
		}
	}
	for _, path := range []string{
		filepath.Join(dir, "Default", "Cookies"),
		filepath.Join(dir, "Default", "Local Storage", "leveldb", "data"),
		filepath.Join(dir, "Default", "Network", "Cookies"),
		filepath.Join(dir, "Default", "Preferences"),
		filepath.Join(dir, "Local State"),
		filepath.Join(dir, "env.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s should be preserved: %v", path, err)
		}
	}
}

func TestContainerDryRun(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "default-test")
	meta, err := envdir.CreateAt(dir, "default", "test", fingerprint.Generate("test"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Default", "Cache", "data"), 100)
	res, err := Container(meta, Options{Cache: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheBytes != 100 {
		t.Fatalf("cache bytes = %d", res.CacheBytes)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cache", "data")); err != nil {
		t.Fatalf("dry run removed file: %v", err)
	}
}
