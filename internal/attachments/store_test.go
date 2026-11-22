package attachments

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "attachments.json")
	store := NewStore(root, index)

	f, err := store.Save("hello.txt", strings.NewReader("hello"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != 5 || f.Name != "hello.txt" || f.SHA256 == "" {
		t.Fatalf("unexpected file: %+v", f)
	}
	if b, err := os.ReadFile(f.Path); err != nil || string(b) != "hello" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	if _, err := store.Get(f.ID); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	renamed, err := store.Rename(f.ID, "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "renamed.txt" {
		t.Fatalf("unexpected name: %q", renamed.Name)
	}
	if err := store.Delete(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Fatalf("file still exists: %v", err)
	}
}

func TestSaveTooLarge(t *testing.T) {
	store := NewStore(t.TempDir(), filepath.Join(t.TempDir(), "attachments.json"))
	if _, err := store.Save("big.bin", strings.NewReader("hello"), 3); err == nil {
		t.Fatal("expected size error")
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"a b.txt":          "a_b.txt",
		".":                "",
		"":                 "",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
