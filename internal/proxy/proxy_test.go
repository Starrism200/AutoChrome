package proxy

import (
	"net"
	"path/filepath"
	"testing"
)

func TestValidateURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8080",
		"https://example.com:443",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"127.0.0.1:1080",
	}
	for _, raw := range valid {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want nil", raw, err)
		}
	}
	invalid := []string{"", "ftp://example.com", "http://", "nohost"}
	for _, raw := range invalid {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", raw)
		}
	}
}

func TestStoreLifecycle(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "proxies.json"))
	p, err := store.Add(Proxy{Name: "one", URL: "socks5://127.0.0.1:1080", Enabled: true, Tags: []string{"fr"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" {
		t.Fatal("empty id")
	}
	if _, err := store.Get(p.ID); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	updated, err := store.Update(p.ID, func(x *Proxy) error {
		x.Name = "two"
		x.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "two" || updated.Enabled {
		t.Fatalf("unexpected update: %+v", updated)
	}
	if err := store.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(p.ID); err == nil {
		t.Fatal("expected not found")
	}
}

func TestCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	store := NewStore(filepath.Join(t.TempDir(), "proxies.json"))
	p, err := store.Add(Proxy{Name: "local", URL: "socks5://" + ln.Addr().String(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	checked, err := store.Check(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !checked.LastCheckOK {
		t.Fatalf("check failed: %+v", checked)
	}
}
