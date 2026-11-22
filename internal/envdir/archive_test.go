package envdir

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"autochrome/internal/fingerprint"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "envs.json"))
	meta := Meta{ID: "a", Platform: "default", Dir: "/tmp/a"}
	if err := r.Put(meta); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("a")
	if !ok || got.Dir != "/tmp/a" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	list, err := r.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if err := r.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("expected deleted")
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "default-a")
	meta, err := CreateAt(dir, "default", "a", fingerprint.Generate("a"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "a" {
		t.Fatalf("meta = %+v", meta)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Export(&buf, dir); err != nil {
		t.Fatal(err)
	}

	root2 := t.TempDir()
	imported, target, err := Import(root2, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if imported.ID != "a" || target != filepath.Join(root2, "default-a") {
		t.Fatalf("imported = %+v, target = %s", imported, target)
	}
	if b, err := os.ReadFile(filepath.Join(target, "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("note = %q, %v", b, err)
	}
	if _, ok := ReadMeta(target); !ok {
		t.Fatal("missing env.json")
	}
}

func TestImportRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../evil", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Import(t.TempDir(), &buf); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestNameTaken(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "envs.json"))
	dir := t.TempDir()
	if err := r.Put(Meta{ID: "a", Name: "alpha", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if !r.NameTaken("alpha", "") {
		t.Fatal("expected alpha to be taken")
	}
	if r.NameTaken("alpha", "a") {
		t.Fatal("same id should be ignored")
	}
	if r.NameTaken("a", "") != true {
		t.Fatal("id should count as a name")
	}
}
