package scripts

import (
	"path/filepath"
	"testing"
)

func TestScriptLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scripts")
	manifest := map[string]any{"description": "demo", "timeout": "5s"}
	if err := Write(root, "demo", "return {ok: true};", manifest); err != nil {
		t.Fatal(err)
	}
	script, src, err := Load(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if script.Description != "demo" || src == "" {
		t.Fatalf("unexpected script: %+v %q", script, src)
	}
	list, err := List(root)
	if err != nil || len(list) != 1 || list[0].Name != "demo" {
		t.Fatalf("List = %v, %v", list, err)
	}
	if err := Delete(root, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root, "demo"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestInvalidNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../x", "a/b", "", ".hidden"} {
		if err := Write(root, name, "return 1;", nil); err == nil {
			t.Errorf("Write accepted invalid name %q", name)
		}
	}
}

func TestScriptTimeout(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, "slow", "return 1;", map[string]any{"timeout": "2s"}); err != nil {
		t.Fatal(err)
	}
	script, _, err := Load(root, "slow")
	if err != nil {
		t.Fatal(err)
	}
	if script.TimeoutDuration().Seconds() != 2 {
		t.Fatalf("timeout = %v", script.TimeoutDuration())
	}
}
