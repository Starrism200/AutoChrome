// Package cleaner removes caches, downloads and other non-login files from a
// container while explicitly preserving cookies and login state.
package cleaner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"autochrome/internal/envdir"
)

// Options selects which non-login data to remove.
type Options struct {
	Cache       bool
	Screenshots bool
	Attachments bool
	Downloads   bool
	DryRun      bool
}

// Result reports what was removed.
type Result struct {
	DryRun           bool     `json:"dry_run"`
	CacheBytes       int64    `json:"cache_bytes"`
	ScreenshotsBytes int64    `json:"screenshots_bytes"`
	AttachmentsBytes int64    `json:"attachments_bytes"`
	DownloadsBytes   int64    `json:"downloads_bytes"`
	FreedBytes       int64    `json:"freed_bytes"`
	Removed          []string `json:"removed,omitempty"`
	Skipped          []string `json:"skipped,omitempty"`
}

// protectedDefault lists files that are part of the login state or Chrome's
// ability to decrypt/restore it. Everything else under Default is treated as
// cache/history and removed.
var protectedDefault = map[string]bool{
	"Cookies":                        true,
	"Cookies-journal":                true,
	"Network":                        true,
	"ClientCertificates":             true,
	"Login Data":                     true,
	"Login Data-journal":             true,
	"Login Data For Account":         true,
	"Login Data For Account-journal": true,
	"Local Storage":                  true,
	"Session Storage":                true,
	"IndexedDB":                      true,
	"WebStorage":                     true,
	"Preferences":                    true,
	"Secure Preferences":             true,
	"Extensions":                     true,
	"Extension State":                true,
	"Extension Rules":                true,
	"Extension Scripts":              true,
	"Extension Activity":             true,
	"Extension Cookies":              true,
	"Local Extension Settings":       true,
	"Managed Extension Settings":     true,
	"Sync Extension Settings":        true,
	"Service Worker":                 true,
	"Sync Data":                      true,
	"Trust Tokens":                   true,
	"shared_proto_db":                true,
}

var topCacheNames = []string{
	"component_crx_cache",
	"extensions_crx_cache",
	"GPUPersistentCache",
	"OptGuidePredictionModels",
	"OptimizationGuideModelsManifest",
	"Safe Browsing",
	"segmentation_platform",
	"BrowserMetrics",
	"Crashpad",
	"DawnCache",
	"DawnGraphiteCache",
	"DawnWebGPUCache",
	"GrShaderCache",
	"ShaderCache",
	"GraphiteDawnCache",
}

// Container removes selected data from one container directory.
func Container(meta envdir.Meta, opts Options) (Result, error) {
	meta.FillPaths()
	res := Result{DryRun: opts.DryRun}
	if meta.Dir == "" {
		return res, fmt.Errorf("container has no directory")
	}
	if _, err := os.Stat(meta.Dir); err != nil {
		return res, err
	}

	release := func() {}
	if !opts.DryRun {
		var err error
		release, err = lockProfile(meta.Dir)
		if err != nil {
			return res, err
		}
	}
	defer release()

	if opts.Cache {
		remove := func(path string) {
			bytes, err := removePath(path, opts.DryRun)
			if err != nil {
				res.Skipped = append(res.Skipped, path+": "+err.Error())
				return
			}
			res.CacheBytes += bytes
			res.Removed = append(res.Removed, displayPath(meta.Dir, path))
		}
		for _, name := range topCacheNames {
			remove(filepath.Join(meta.Dir, name))
		}
		remove(shmCacheDir(meta.Dir))
		defaultDir := filepath.Join(meta.Dir, "Default")
		entries, err := os.ReadDir(defaultDir)
		if err != nil && !os.IsNotExist(err) {
			res.Skipped = append(res.Skipped, defaultDir+": "+err.Error())
		}
		for _, e := range entries {
			if protectedDefault[e.Name()] {
				continue
			}
			remove(filepath.Join(defaultDir, e.Name()))
		}
	}

	if opts.Screenshots {
		bytes, err := clearDir(dirOr(meta.ScreenshotDir, filepath.Join(meta.Dir, "screenshots")), opts.DryRun)
		if err != nil {
			res.Skipped = append(res.Skipped, "screenshots: "+err.Error())
		} else {
			res.ScreenshotsBytes += bytes
			res.Removed = append(res.Removed, "screenshots/")
		}
	}
	if opts.Attachments {
		bytes, err := clearDir(dirOr(meta.AttachmentDir, filepath.Join(meta.Dir, "attachments")), opts.DryRun)
		if err != nil {
			res.Skipped = append(res.Skipped, "attachments: "+err.Error())
		} else {
			res.AttachmentsBytes += bytes
			res.Removed = append(res.Removed, "attachments/")
		}
	}
	if opts.Downloads {
		bytes, err := clearDir(dirOr(meta.DownloadDir, filepath.Join(meta.Dir, "downloads")), opts.DryRun)
		if err != nil {
			res.Skipped = append(res.Skipped, "downloads: "+err.Error())
		} else {
			res.DownloadsBytes += bytes
			res.Removed = append(res.Removed, "downloads/")
		}
	}
	res.FreedBytes = res.CacheBytes + res.ScreenshotsBytes + res.AttachmentsBytes + res.DownloadsBytes
	return res, nil
}

func lockProfile(dir string) (func(), error) {
	path := filepath.Join(dir, ".autochrome.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("container is in use")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func removePath(path string, dry bool) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	size := pathSize(path, info)
	if dry {
		return size, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return 0, err
	}
	return size, nil
}

func clearDir(dir string, dry bool) (int64, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, e := range entries {
		size, err := removePath(filepath.Join(dir, e.Name()), dry)
		if err != nil {
			return total, err
		}
		total += size
	}
	if !dry {
		_ = os.MkdirAll(dir, 0o755)
	}
	return total, nil
}

func pathSize(path string, info os.FileInfo) int64 {
	if info.Mode().IsRegular() {
		return info.Size()
	}
	if !info.IsDir() {
		return 0
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

func shmCacheDir(profileDir string) string {
	sum := sha256.Sum256([]byte(profileDir))
	return filepath.Join("/dev/shm", "autochrome-cache", hex.EncodeToString(sum[:8]))
}

func dirOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func displayPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
