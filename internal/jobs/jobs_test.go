package jobs

import (
	"strings"
	"testing"
	"time"

	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

func TestJobSuccessAndLogs(t *testing.T) {
	t.Setenv("AUTOCHROME_HOME", t.TempDir())
	u, err := users.Add("jobs")
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(u, "/bin/true")
	jm := New(u, mgr, true, nil)
	defer jm.Close()
	if err := scripts.Write(u.ScriptRoot, "demo", "console.log('hi'); return {ok:true};", nil); err != nil {
		t.Fatal(err)
	}
	job, err := jm.Start("demo", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := jm.Get(job.ID)
		if got.Status == StatusSucceeded {
			break
		}
		if got.Status == StatusFailed || got.Status == StatusCancelled {
			t.Fatalf("status = %s %s", got.Status, got.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs, err := jm.Logs(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "finished: succeeded") || !strings.Contains(logs, "hi") {
		t.Fatalf("logs = %q", logs)
	}
}

func TestJobCancel(t *testing.T) {
	t.Setenv("AUTOCHROME_HOME", t.TempDir())
	u, err := users.Add("jobs")
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(u, "/bin/true")
	jm := New(u, mgr, true, nil)
	defer jm.Close()
	if err := scripts.Write(u.ScriptRoot, "loop", "while (true) {}", nil); err != nil {
		t.Fatal(err)
	}
	job, err := jm.Start("loop", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := jm.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := jm.Get(job.ID)
		if got.Status == StatusCancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
