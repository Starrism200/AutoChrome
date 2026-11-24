package fontconfig

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	if err := Validate("minimal", nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate("custom", []string{"Arial"}); err != nil {
		t.Fatal(err)
	}
	if err := Validate("custom", nil); err == nil {
		t.Fatal("custom without whitelist should fail")
	}
	if err := Validate("bogus", nil); err == nil {
		t.Fatal("unknown preset should fail")
	}
	if err := Validate("system", []string{"Arial", "Arial"}); err != nil {
		t.Fatal(err)
	}
	if err := Validate("system", []string{"bad\x00font"}); err == nil {
		t.Fatal("control character should fail")
	}
}

func TestPrepareSystemIsNoop(t *testing.T) {
	res, err := Prepare(t.TempDir(), "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfigFile != "" || len(res.Env) != 0 {
		t.Fatalf("system should not create isolation: %+v", res)
	}
}

func TestPrepareMinimal(t *testing.T) {
	if _, err := exec.LookPath("fc-list"); err != nil {
		t.Skip("fontconfig tools are not installed")
	}
	dir := t.TempDir()
	res, err := Prepare(dir, "minimal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfigFile == "" || res.FontsDir == "" || res.CacheDir == "" {
		t.Fatalf("incomplete result: %+v", res)
	}
	config, err := os.ReadFile(res.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), res.FontsDir) || !strings.Contains(string(config), res.CacheDir) {
		t.Fatalf("config paths missing: %s", config)
	}
	entries, err := os.ReadDir(res.FontsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no fonts linked")
	}
	found := false
	for _, env := range res.Env {
		if strings.HasPrefix(env, "FONTCONFIG_FILE="+res.ConfigFile) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing FONTCONFIG_FILE: %v", res.Env)
	}
}

func TestBuildConfigRejectsExtraFamilies(t *testing.T) {
	config := buildConfig("/tmp/fonts", "/tmp/cache", []string{"Arial"}, []string{"Noto Sans CJK JP"})
	if !strings.Contains(config, "<rejectfont>") ||
		!strings.Contains(config, "<string>Noto Sans CJK JP</string>") {
		t.Fatalf("missing reject pattern: %s", config)
	}
	if !strings.Contains(config, "<family>Arial</family>") {
		t.Fatalf("missing alias: %s", config)
	}
}

func TestPrepareWindowsAliases(t *testing.T) {
	if _, err := exec.LookPath("fc-list"); err != nil {
		t.Skip("fontconfig tools are not installed")
	}
	res, err := Prepare(t.TempDir(), "windows", nil)
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(res.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "<family>Arial</family>") ||
		!strings.Contains(string(config), "<family>Liberation Sans</family>") {
		t.Fatalf("missing Arial alias: %s", config)
	}
}
