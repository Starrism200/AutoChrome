package fingerprint

import (
	"reflect"
	"testing"
)

func TestGeneratePrivacyDefaults(t *testing.T) {
	fp := Generate("container-a")
	if fp.CanvasNoise <= 0 || fp.AudioNoise <= 0 {
		t.Fatalf("expected noise defaults: %+v", fp)
	}
	if fp.FontSet != "minimal" {
		t.Fatalf("font_set = %q", fp.FontSet)
	}
	if fp.Language == "" || fp.Timezone == "" || fp.UserAgent == "" {
		t.Fatalf("incomplete fingerprint: %+v", fp)
	}
}

func TestGenerateStable(t *testing.T) {
	a := Generate("same-seed")
	b := Generate("same-seed")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("fingerprint is not stable")
	}
}
