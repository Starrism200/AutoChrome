package jsruntime

import (
	"fmt"
	"io"
	"testing"

	"autochrome/internal/session"
	"autochrome/internal/users"
)

func TestRunAndEnv(t *testing.T) {
	t.Setenv("AUTOCHROME_HOME", t.TempDir())
	u, err := users.Add("js")
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(u, "/bin/true")
	r := New(u, mgr, io.Discard)

	out, err := r.Run(`return {n: input.x + 1};`, map[string]any{"x": 41})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if fmt.Sprint(m["n"]) != "42" {
		t.Fatalf("result = %#v", out)
	}

	if _, err := r.Run(`ac.env.create({id: "e1"}); return ac.env.list().length;`, nil); err != nil {
		t.Fatal(err)
	}
	out, err = r.Run(`return ac.env.list().length;`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out) != "1" {
		t.Fatalf("env list length = %#v", out)
	}
	if _, err := r.Run(`ac.env.create({id: "e1"}); return 1;`, nil); err == nil {
		t.Fatal("expected duplicate env error")
	}
}
