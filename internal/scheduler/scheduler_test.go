package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	j, err := store.Add(Job{Type: "close", IntervalSeconds: 5, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == "" {
		t.Fatal("empty id")
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	due, err := store.Due(time.Now())
	if err != nil || len(due) != 1 {
		t.Fatalf("Due = %v, %v", due, err)
	}
	if _, err := store.MarkRun(j.ID, "ok", "", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	due, err = store.Due(time.Now())
	if err != nil || len(due) != 0 {
		t.Fatalf("Due after run = %v, %v", due, err)
	}
	if _, err := store.Update(j.ID, func(j *Job) error { j.IntervalSeconds = 10; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(j.ID); err == nil {
		t.Fatal("expected deleted")
	}
}
