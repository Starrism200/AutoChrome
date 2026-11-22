// Package fingerprint generates a stable, per-environment browser identity.
// The same seed always produces the same profile so logins stay consistent.
package fingerprint

import (
	"encoding/json"
	"hash/fnv"

	"autochrome/internal/ua"
)

type locale struct {
	Language       string
	AcceptLanguage string
	Timezone       string
}

type osProfile struct {
	UserAgent     string
	Platform      string
	WebGLVendor   string
	WebGLRenderer string
}

// Profile is the browser identity applied to one environment.
type Profile struct {
	UserAgent           string  `json:"user_agent"`
	Platform            string  `json:"platform"`
	Language            string  `json:"language"`
	AcceptLanguage      string  `json:"accept_language"`
	Timezone            string  `json:"timezone"`
	Width               int     `json:"width"`
	Height              int     `json:"height"`
	ColorDepth          int     `json:"color_depth"`
	PixelRatio          float64 `json:"pixel_ratio"`
	HardwareConcurrency int     `json:"hardware_concurrency"`
	DeviceMemory        int     `json:"device_memory"`
	WebGLVendor         string  `json:"webgl_vendor"`
	WebGLRenderer       string  `json:"webgl_renderer"`
}

var osProfiles = []osProfile{
	{
		UserAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ua.ChromeMajor + ".0.0.0 Safari/537.36",
		Platform:      "Win32",
		WebGLVendor:   "Google Inc. (NVIDIA)",
		WebGLRenderer: "ANGLE (NVIDIA, NVIDIA GeForce GTX 1660 SUPER Direct3D11 vs_5_0 ps_5_0, D3D11)",
	},
	{
		UserAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ua.ChromeMajor + ".0.0.0 Safari/537.36",
		Platform:      "Win32",
		WebGLVendor:   "Google Inc. (Intel)",
		WebGLRenderer: "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)",
	},
	{
		UserAgent:     "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ua.ChromeMajor + ".0.0.0 Safari/537.36",
		Platform:      "MacIntel",
		WebGLVendor:   "Google Inc. (Apple)",
		WebGLRenderer: "ANGLE (Apple, Apple M1, OpenGL 4.1)",
	},
	{
		UserAgent:     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ua.ChromeMajor + ".0.0.0 Safari/537.36",
		Platform:      "Linux x86_64",
		WebGLVendor:   "Google Inc. (Intel)",
		WebGLRenderer: "ANGLE (Intel, Mesa Intel(R) UHD Graphics 620 (KBL GT2), OpenGL 4.6)",
	},
}

var locales = []locale{
	{"en-US", "en-US,en", "America/New_York"},
	{"en-US", "en-US,en", "America/Chicago"},
	{"en-US", "en-US,en", "America/Los_Angeles"},
	{"en-GB", "en-GB,en", "Europe/London"},
	{"fr-FR", "fr-FR,fr,en", "Europe/Paris"},
	{"de-DE", "de-DE,de,en", "Europe/Berlin"},
	{"es-ES", "es-ES,es,en", "Europe/Madrid"},
	{"pt-BR", "pt-BR,pt,en", "America/Sao_Paulo"},
	{"it-IT", "it-IT,it,en", "Europe/Rome"},
	{"nl-NL", "nl-NL,nl,en", "Europe/Amsterdam"},
}

var screens = []struct {
	w, h  int
	ratio float64
}{
	{1366, 900, 1},
	{1440, 900, 1},
	{1536, 864, 1.25},
	{1600, 900, 1},
	{1920, 1080, 1},
	{1280, 800, 1},
	{1512, 982, 2},
}

var cores = []int{4, 8, 12, 16}
var memory = []int{4, 8, 16}

// Generate returns the deterministic profile for seed.
func Generate(seed string) Profile {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	n := h.Sum64()
	next := func(mod int) int {
		n = n*6364136223846793005 + 1442695040888963407
		if mod <= 0 {
			return 0
		}
		return int((n >> 33) % uint64(mod))
	}
	o := osProfiles[next(len(osProfiles))]
	l := locales[next(len(locales))]
	s := screens[0]
	return Profile{
		UserAgent:           o.UserAgent,
		Platform:            o.Platform,
		Language:            l.Language,
		AcceptLanguage:      l.AcceptLanguage,
		Timezone:            l.Timezone,
		Width:               s.w,
		Height:              s.h,
		ColorDepth:          24,
		PixelRatio:          s.ratio,
		HardwareConcurrency: cores[next(len(cores))],
		DeviceMemory:        memory[next(len(memory))],
		WebGLVendor:         o.WebGLVendor,
		WebGLRenderer:       o.WebGLRenderer,
	}
}

func Encode(p Profile) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func Decode(raw string) (Profile, bool) {
	if raw == "" {
		return Profile{}, false
	}
	var p Profile
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.UserAgent == "" {
		return Profile{}, false
	}
	if p.Width <= 0 || p.Height <= 0 {
		return Profile{}, false
	}
	if p.ColorDepth <= 0 {
		p.ColorDepth = 24
	}
	if p.PixelRatio <= 0 {
		p.PixelRatio = 1
	}
	return p, true
}
