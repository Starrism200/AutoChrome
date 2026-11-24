// Package fontconfig builds a per-container font set and fontconfig file.
// Chrome is started with FONTCONFIG_FILE pointing at the generated file, so
// containers can expose different font families while sharing one Chrome
// binary and installation.
package fontconfig

import (
	"bytes"
	"crypto/sha1"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"autochrome/internal/atomicfile"
)

// Result describes the generated font configuration and the environment
// variables Chrome must receive.
type Result struct {
	ConfigFile string   `json:"config_file,omitempty"`
	FontsDir   string   `json:"fonts_dir,omitempty"`
	CacheDir   string   `json:"cache_dir,omitempty"`
	Families   []string `json:"families,omitempty"`
	Env        []string `json:"-"`
}

var presets = map[string][]string{
	// minimal is a stable Linux set; it avoids leaking the host's unusual
	// fonts while keeping common Latin/CJK/emoji coverage.
	"minimal": {
		"DejaVu Sans", "DejaVu Serif", "DejaVu Sans Mono",
		"Liberation Sans", "Liberation Serif", "Liberation Mono",
		"Noto Sans", "Noto Serif", "Noto Sans Mono",
		"Noto Sans CJK SC", "Noto Serif CJK SC", "Noto Color Emoji",
	},
	"linux": {
		"DejaVu Sans", "DejaVu Serif", "DejaVu Sans Mono",
		"Liberation Sans", "Liberation Serif", "Liberation Mono",
		"Noto Sans", "Noto Serif", "Noto Sans Mono",
		"Noto Sans CJK SC", "Noto Serif CJK SC", "Noto Color Emoji",
	},
	"windows": {
		"Arial", "Times New Roman", "Courier New", "Segoe UI", "Tahoma",
		"Verdana", "Calibri", "Cambria", "Georgia", "Trebuchet MS",
		"Microsoft YaHei", "SimSun",
	},
	"macos": {
		"Helvetica", "Helvetica Neue", "Arial", "Times New Roman",
		"Courier New", "Menlo", "Monaco", "Georgia", "Verdana",
		"PingFang SC", "Songti SC",
	},
}

// aliases maps common Windows/macOS family names to the Linux fonts that are
// actually installed. The generated fontconfig file keeps the alias so CSS
// requests still resolve naturally.
var aliases = map[string]string{
	"Arial":           "Liberation Sans",
	"Times New Roman": "Liberation Serif",
	"Courier New":     "Liberation Mono",
	"Segoe UI":        "DejaVu Sans",
	"Tahoma":          "DejaVu Sans",
	"Verdana":         "DejaVu Sans",
	"Calibri":         "Carlito",
	"Cambria":         "Caladea",
	"Georgia":         "DejaVu Serif",
	"Trebuchet MS":    "DejaVu Sans",
	"Helvetica":       "DejaVu Sans",
	"Helvetica Neue":  "DejaVu Sans",
	"Menlo":           "DejaVu Sans Mono",
	"Monaco":          "DejaVu Sans Mono",
	"Microsoft YaHei": "Noto Sans CJK SC",
	"SimSun":          "Noto Serif CJK SC",
	"PingFang SC":     "Noto Sans CJK SC",
	"Songti SC":       "Noto Serif CJK SC",
}

type fontFile struct {
	Path     string
	Families []string
}

func normalizeSet(set string) string {
	set = strings.ToLower(strings.TrimSpace(set))
	if set == "" {
		return "system"
	}
	return set
}

// Validate checks the requested font set and whitelist without touching the
// filesystem. It is used by the HTTP API before persisting a fingerprint.
func Validate(set string, whitelist []string) error {
	set = normalizeSet(set)
	switch set {
	case "system", "minimal", "linux", "windows", "macos", "custom":
	default:
		return fmt.Errorf("font_set must be system, minimal, linux, windows, macos, or custom")
	}
	if set == "custom" && len(cleanFamilies(whitelist)) == 0 {
		return fmt.Errorf("font_set custom requires a non-empty font_whitelist")
	}
	if len(whitelist) > 64 {
		return fmt.Errorf("font_whitelist supports at most 64 families")
	}
	for _, family := range whitelist {
		family = strings.TrimSpace(family)
		if family == "" || len(family) > 128 {
			return fmt.Errorf("font_whitelist families must be 1-128 characters")
		}
		for _, r := range family {
			if unicode.IsControl(r) {
				return fmt.Errorf("font_whitelist contains a control character")
			}
		}
	}
	return nil
}

// Prepare generates a per-container font directory and fonts.conf. It returns
// an empty Result when font isolation is disabled.
func Prepare(containerDir, set string, whitelist []string) (Result, error) {
	if err := Validate(set, whitelist); err != nil {
		return Result{}, err
	}
	set = normalizeSet(set)
	families := cleanFamilies(whitelist)
	if len(families) == 0 && set != "system" && set != "custom" {
		families = cleanFamilies(presets[set])
	}
	if len(families) == 0 {
		return Result{}, nil
	}
	containerDir = filepath.Clean(containerDir)
	if containerDir == "" || containerDir == "." || containerDir == string(os.PathSeparator) {
		return Result{}, fmt.Errorf("fontconfig requires a container directory")
	}
	absDir, err := filepath.Abs(containerDir)
	if err != nil {
		return Result{}, err
	}
	containerDir = absDir

	fonts, err := listFonts()
	if err != nil {
		return Result{}, err
	}
	byFamily := make(map[string][]string)
	fileFamilies := make(map[string][]string)
	for _, f := range fonts {
		fileFamilies[f.Path] = append(fileFamilies[f.Path], f.Families...)
		for _, family := range f.Families {
			key := strings.ToLower(strings.TrimSpace(family))
			if key == "" {
				continue
			}
			byFamily[key] = append(byFamily[key], f.Path)
		}
	}

	fontconfigDir := filepath.Join(containerDir, "fontconfig")
	fontsDir := filepath.Join(fontconfigDir, "fonts")
	cacheDir := filepath.Join(fontconfigDir, "cache")
	if err := os.RemoveAll(fontsDir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(fontsDir, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return Result{}, err
	}

	linked := make(map[string]bool)
	allowed := map[string]bool{
		"dejavu sans":      true,
		"dejavu serif":     true,
		"dejavu sans mono": true,
	}
	resolved := make([]string, 0, len(families))
	links := 0
	for _, family := range families {
		targets := []string{family}
		allowed[strings.ToLower(family)] = true
		if alias, ok := aliasTarget(family); ok && !strings.EqualFold(alias, family) {
			targets = append(targets, alias)
			allowed[strings.ToLower(alias)] = true
		}
		found := false
		for _, target := range targets {
			files := byFamily[strings.ToLower(target)]
			fallback := false
			if len(files) == 0 {
				if file := fallbackFile(target); file != "" {
					files = []string{file}
					fallback = true
				}
			}
			for _, file := range files {
				if fallback {
					for _, name := range fileFamilies[file] {
						allowed[strings.ToLower(strings.TrimSpace(name))] = true
					}
				}
				if linked[file] {
					found = true
					continue
				}
				if err := linkFont(fontsDir, file); err != nil {
					return Result{}, err
				}
				linked[file] = true
				found = true
				links++
			}
		}
		if found {
			resolved = append(resolved, family)
		}
	}
	if links == 0 {
		return Result{}, fmt.Errorf("font configuration %q resolved no installed fonts", set)
	}
	rejectedSet := make(map[string]bool)
	for file := range linked {
		for _, name := range fileFamilies[file] {
			name = strings.TrimSpace(name)
			if name != "" && !allowed[strings.ToLower(name)] {
				rejectedSet[name] = true
			}
		}
	}
	rejected := make([]string, 0, len(rejectedSet))
	for name := range rejectedSet {
		rejected = append(rejected, name)
	}
	sort.Strings(rejected)

	configFile := filepath.Join(fontconfigDir, "fonts.conf")
	config := buildConfig(fontsDir, cacheDir, families, rejected)
	if err := atomicfile.Write(configFile, []byte(config), 0o644); err != nil {
		return Result{}, err
	}
	result := Result{
		ConfigFile: configFile,
		FontsDir:   fontsDir,
		CacheDir:   cacheDir,
		Families:   resolved,
		Env: []string{
			"FONTCONFIG_FILE=" + configFile,
			"FONTCONFIG_PATH=" + fontconfigDir,
		},
	}
	return result, nil
}

// Predefined returns a copy of the families in a preset. It is useful for
// diagnostics and tests.
func Predefined(set string) []string {
	set = normalizeSet(set)
	return append([]string(nil), presets[set]...)
}

func cleanFamilies(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(in))
	for _, family := range in {
		family = strings.TrimSpace(family)
		if family == "" {
			continue
		}
		key := strings.ToLower(family)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, family)
	}
	return out
}

func listFonts() ([]fontFile, error) {
	out, err := exec.Command("fc-list", "--format=%{file}\t%{family}\n").Output()
	if err != nil {
		return nil, fmt.Errorf("list system fonts: %w", err)
	}
	var fonts []fontFile
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		path := strings.TrimSpace(parts[0])
		if path == "" {
			continue
		}
		families := strings.Split(parts[1], ",")
		for i := range families {
			families[i] = strings.TrimSpace(families[i])
		}
		fonts = append(fonts, fontFile{Path: path, Families: families})
	}
	return fonts, nil
}

func fallbackFile(family string) string {
	out, err := exec.Command("fc-match", "-f", "%{file}\n", family).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		path := strings.TrimSpace(line)
		if path != "" {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

func linkFont(dir, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat font %s: %w", src, err)
	}
	if info.IsDir() {
		return fmt.Errorf("font %s is a directory", src)
	}
	name := sanitizeName(filepath.Base(src))
	if name == "" {
		name = "font"
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		sum := sha1.Sum([]byte(src))
		dst = filepath.Join(dir, fmt.Sprintf("%x-%s", sum[:4], name))
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("link font %s: %w", src, err)
	}
	return nil
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "._-")
}

func aliasTarget(family string) (string, bool) {
	for name, target := range aliases {
		if strings.EqualFold(name, family) {
			return target, true
		}
	}
	return "", false
}

func buildConfig(fontsDir, cacheDir string, families, rejected []string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\"?>\n")
	b.WriteString("<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n")
	b.WriteString("<fontconfig>\n")
	b.WriteString("  <dir>" + xmlEscape(fontsDir) + "</dir>\n")
	b.WriteString("  <cachedir>" + xmlEscape(cacheDir) + "</cachedir>\n")
	b.WriteString("  <alias binding=\"same\"><family>sans-serif</family><prefer><family>DejaVu Sans</family></prefer></alias>\n")
	b.WriteString("  <alias binding=\"same\"><family>serif</family><prefer><family>DejaVu Serif</family></prefer></alias>\n")
	b.WriteString("  <alias binding=\"same\"><family>monospace</family><prefer><family>DejaVu Sans Mono</family></prefer></alias>\n")
	b.WriteString("  <alias binding=\"same\"><family>system-ui</family><prefer><family>DejaVu Sans</family></prefer></alias>\n")
	if len(rejected) > 0 {
		b.WriteString("  <selectfont>\n")
		b.WriteString("    <rejectfont>\n")
		for _, family := range rejected {
			b.WriteString("      <pattern><patelt name=\"family\"><string>" + xmlEscape(family) + "</string></patelt></pattern>\n")
		}
		b.WriteString("    </rejectfont>\n")
		b.WriteString("  </selectfont>\n")
	}
	aliasNames := make([]string, 0)
	for _, family := range families {
		if target, ok := aliasTarget(family); ok && !strings.EqualFold(target, family) {
			aliasNames = append(aliasNames, family)
		}
	}
	sort.Strings(aliasNames)
	for _, family := range aliasNames {
		target, _ := aliasTarget(family)
		b.WriteString("  <alias binding=\"same\">\n")
		b.WriteString("    <family>" + xmlEscape(family) + "</family>\n")
		b.WriteString("    <prefer><family>" + xmlEscape(target) + "</family></prefer>\n")
		b.WriteString("  </alias>\n")
	}
	b.WriteString("</fontconfig>\n")
	return b.String()
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
