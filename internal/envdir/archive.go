package envdir

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Export writes a gzip-compressed tar archive of an environment directory.
func Export(w io.Writer, dir string) (err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		if e := tw.Close(); err == nil {
			err = e
		}
		if e := gz.Close(); err == nil {
			err = e
		}
	}()

	return filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = rel + "/"
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() || skipRuntimeFile(info.Name()) {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// Import extracts an environment archive into root and returns its metadata.
func Import(root string, r io.Reader) (Meta, string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Meta{}, "", err
	}
	tmp, err := os.MkdirTemp(root, ".import-*")
	if err != nil {
		return Meta{}, "", err
	}
	defer os.RemoveAll(tmp)

	gz, err := gzip.NewReader(r)
	if err != nil {
		return Meta{}, "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	const maxTotal = int64(4 << 30)
	var total int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Meta{}, "", err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return Meta{}, "", fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(tmp, name)
		if !within(tmp, target) {
			return Meta{}, "", fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return Meta{}, "", err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxTotal || total > maxTotal-hdr.Size {
				return Meta{}, "", fmt.Errorf("archive too large")
			}
			total += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return Meta{}, "", err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return Meta{}, "", err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return Meta{}, "", copyErr
			}
			if closeErr != nil {
				return Meta{}, "", closeErr
			}
		}
	}

	base := tmp
	meta, ok := ReadMeta(base)
	if !ok {
		if entries, err := os.ReadDir(tmp); err == nil && len(entries) == 1 && entries[0].IsDir() {
			base = filepath.Join(tmp, entries[0].Name())
			meta, ok = ReadMeta(base)
		}
	}
	if !ok {
		return Meta{}, "", fmt.Errorf("archive does not contain %s", MetaFile)
	}
	if strings.TrimSpace(meta.ID) == "" {
		return Meta{}, "", fmt.Errorf("archive has no environment id")
	}
	if meta.Platform == "" {
		meta.Platform = "default"
	}
	target := filepath.Join(root, Name(meta.Platform, meta.ID))
	if _, err := os.Stat(target); err == nil {
		return Meta{}, "", fmt.Errorf("environment %q already exists", meta.ID)
	}
	if err := os.Rename(base, target); err != nil {
		return Meta{}, "", err
	}
	meta.Dir = target
	if err := writeMeta(target, meta); err != nil {
		_ = os.RemoveAll(target)
		return Meta{}, "", err
	}
	return meta, target, nil
}

func within(root, target string) bool {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	return cleanTarget == cleanRoot || strings.HasPrefix(cleanTarget+string(os.PathSeparator), cleanRoot+string(os.PathSeparator))
}

func skipRuntimeFile(name string) bool {
	switch name {
	case "SingletonLock", "SingletonSocket", "SingletonCookie", "DevToolsActivePort", ".autochrome.lock":
		return true
	}
	return false
}
