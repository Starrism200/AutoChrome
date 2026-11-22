package browser

import (
	"os"
	"testing"
	"time"
)

func TestLaunchAndSnapshot(t *testing.T) {
	if os.Getenv("AUTOCHROME_TEST_BROWSER") != "1" {
		t.Skip("set AUTOCHROME_TEST_BROWSER=1 to run browser integration test")
	}
	ctx, cleanup, err := Launch(Options{
		ChromePath: "/usr/bin/google-chrome",
		ProfileDir: t.TempDir(),
		Headless:   true,
		Timeout:    45 * time.Second,
		SlotDir:    t.TempDir(),
		SlotMax:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := Navigate(ctx, "data:text/html,<button>Hello</button>"); err != nil {
		t.Fatal(err)
	}
	if err := WaitFor(ctx, "button", "", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	snap, err := TakeSnapshot(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) == 0 {
		t.Fatal("empty snapshot")
	}
}
