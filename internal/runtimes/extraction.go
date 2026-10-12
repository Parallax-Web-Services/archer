package runtimes

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/store"
)

const maxTreeBytes int64 = 512 << 20
const maxEntries = 50000
const receiptName = ".archer-runtime.json"
const emptyIni = ".archer-empty.ini"

type entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
}

func safeRelative(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return "", fmt.Errorf("unsafe relative runtime path %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." || part == "" || strings.TrimRight(part, " .") != part {
			return "", fmt.Errorf("unsafe relative runtime path %q", value)
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return "", errors.New("runtime path uses a reserved device name")
		}
		for _, r := range part {
			if r < 32 || strings.ContainsRune(`<>"|?*`, r) {
				return "", errors.New("runtime path contains a nonportable character")
			}
		}
	}
	return value, nil
}
func archivePath(name string, b Bundle) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if _, err := safeRelative(name); err != nil {
		return "", err
	}
	if b.StripPrefix == "" {
		return name, nil
	}
	if name == b.StripPrefix {
		return "", nil
	}
	if !strings.HasPrefix(name, b.StripPrefix+"/") {
		return "", errors.New("runtime archive entry is outside the declared strip prefix")
	}
	return strings.TrimPrefix(name, b.StripPrefix+"/"), nil
}

type treeBuilder struct {
	ctx      context.Context
	root     string
	entries  map[string]entry
	explicit map[string]bool
	folded   map[string]string
	bytes    int64
	count    int
}

func (t *treeBuilder) register(e entry, explicit bool) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if _, err := safeRelative(e.Path); err != nil {
		return err
	}
	if e.Path == receiptName || e.Path == emptyIni {
		return errors.New("runtime archive collides with reserved metadata")
	}
	if previous, ok := t.folded[strings.ToLower(e.Path)]; ok && previous != e.Path {
		return errors.New("runtime archive has case-colliding paths")
	}
	t.folded[strings.ToLower(e.Path)] = e.Path
	if old, ok := t.entries[e.Path]; ok {
		if old.Kind != "dir" || e.Kind != "dir" || (explicit && t.explicit[e.Path]) {
			return fmt.Errorf("duplicate or conflicting runtime archive entry %s", e.Path)
		}
	} else {
		t.entries[e.Path] = e
		if len(t.entries) > maxEntries {
			return errors.New("runtime tree exceeds entry limit")
		}
	}
	if explicit {
		t.explicit[e.Path] = true
		t.count++
		if t.count > maxEntries {
			return errors.New("runtime archive exceeds entry limit")
		}
	}
	for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
		if old, ok := t.entries[parent]; ok && old.Kind != "dir" {
			return errors.New("runtime archive entry has a non-directory parent")
		}
		if err := t.registerDir(parent); err != nil {
			return err
		}
	}
	return nil
}
func (t *treeBuilder) registerDir(name string) error {
	if previous, ok := t.folded[strings.ToLower(name)]; ok && previous != name {
		return errors.New("runtime archive has case-colliding parent paths")
	}
	t.folded[strings.ToLower(name)] = name
	if old, ok := t.entries[name]; ok {
		if old.Kind != "dir" {
			return errors.New("runtime archive has a non-directory parent")
		}
		return nil
	}
	t.entries[name] = entry{Path: name, Kind: "dir", Mode: 0755}
	if len(t.entries) > maxEntries {
		return errors.New("runtime tree exceeds entry limit")
	}
	return nil
}
func (t *treeBuilder) add(name, kind, target string, mode os.FileMode, size int64, r io.Reader) error {
	if name == "" {
		if kind != "dir" {
			return errors.New("strip prefix root is not a directory")
		}
		return nil
	}
	e := entry{Path: name, Kind: kind, Mode: 0644, Size: size, Target: target}
	if kind == "dir" {
		e.Mode = 0755
		e.Size = 0
	} else if kind == "link" {
		e.Mode = 0777
		e.Size = 0
		if target == "" || strings.ContainsAny(target, "\\:\x00\r\n") || strings.HasPrefix(target, "/") {
			return errors.New("unsafe runtime symlink target")
		}
		if path.Clean(target) != target || target == "." {
			return errors.New("runtime symlink target must be canonical")
		}
		resolved := path.Clean(path.Join(path.Dir(name), target))
		if resolved == "." || strings.HasPrefix(resolved, "../") || resolved == ".." {
			return errors.New("runtime symlink escapes tree")
		}
		if _, err := safeRelative(resolved); err != nil {
			return err
		}
	} else if kind == "file" {
		if mode.Perm()&0111 != 0 {
			e.Mode = 0755
		}
		if size < 0 || size > maxTreeBytes-t.bytes {
			return errors.New("runtime archive exceeds expanded size limit")
		}
	} else {
		return errors.New("unsupported runtime archive entry type")
	}
	if err := t.register(e, true); err != nil {
		return err
	}
	if kind != "file" {
		return nil
	}
	var out *os.File
	if t.root != "" {
		dest := filepath.Join(t.root, filepath.FromSlash(name))
		if err := checkDirectories(filepath.Dir(dest), true); err != nil {
			return err
		}
		var err error
		out, err = os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(e.Mode))
		if err != nil {
			return err
		}
		defer out.Close()
	}
	hash := sha256.New()
	writer := io.Writer(hash)
	if out != nil {
		writer = io.MultiWriter(hash, out)
	}
	n, err := io.Copy(writer, io.LimitReader(&ctxReader{t.ctx, r}, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return errors.New("runtime archive entry size changed")
	}
	if out != nil {
		if err := out.Chmod(os.FileMode(e.Mode)); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	t.bytes += n
	e.SHA256 = hex.EncodeToString(hash.Sum(nil))
	t.entries[name] = e
	return nil
}
func (t *treeBuilder) finish() ([]entry, error) {
	all := make([]entry, 0, len(t.entries))
	for _, e := range t.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	for _, e := range all {
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if t.entries[parent].Kind != "dir" {
				return nil, errors.New("runtime archive has a symlink or file parent")
			}
		}
		if e.Kind == "link" {
			if _, err := resolveLink(t.entries, e.Path, map[string]bool{}); err != nil {
				return nil, err
			}
		}
	}
	if t.root != "" {
		for _, e := range all {
			if e.Kind == "dir" {
				dest := filepath.Join(t.root, filepath.FromSlash(e.Path))
				if err := checkDirectories(dest, true); err != nil {
					return nil, err
				}
				if err := os.Chmod(dest, 0755); err != nil {
					return nil, err
				}
			}
		}
		for _, e := range all {
			if e.Kind == "link" {
				if err := os.Symlink(filepath.FromSlash(e.Target), filepath.Join(t.root, filepath.FromSlash(e.Path))); err != nil {
					return nil, fmt.Errorf("runtime bundle requires symlink support: %w", err)
				}
			}
		}
	}
	return all, nil
}
func resolveLink(entries map[string]entry, name string, seen map[string]bool) (string, error) {
	if len(seen) > 64 || seen[name] {
		return "", errors.New("runtime archive contains a symlink cycle")
	}
	seen[name] = true
	parts := strings.Split(name, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		e, ok := entries[prefix]
		if !ok {
			return "", errors.New("runtime symlink target is absent")
		}
		if e.Kind == "link" {
			dest := path.Clean(path.Join(path.Dir(prefix), e.Target))
			if i+1 < len(parts) {
				dest = path.Join(dest, strings.Join(parts[i+1:], "/"))
			}
			return resolveLink(entries, dest, seen)
		}
		if i+1 < len(parts) && e.Kind != "dir" {
			return "", errors.New("runtime symlink target has a file parent")
		}
	}
	return name, nil
}
func bundleInventory(ctx context.Context, object string, b Bundle, root string) ([]entry, error) {
	before, err := hashRegular(object, store.MaxArtifactBytes)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(before, b.Artifact.SHA256) {
		return nil, errors.New("runtime artifact checksum mismatch")
	}
	f, err := os.Open(object)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &treeBuilder{ctx: ctx, root: root, entries: map[string]entry{}, explicit: map[string]bool{}, folded: map[string]string{}}
	switch b.Format {
	case "raw":
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if err := t.add(b.Executable, "file", "", 0755, info.Size(), f); err != nil {
			return nil, err
		}
	case "tar.gz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		expanded := &io.LimitedReader{R: &ctxReader{ctx, gz}, N: maxTreeBytes + (64 << 20) + 1}
		tr := tar.NewReader(expanded)
		headers := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			headers++
			if headers > maxEntries {
				return nil, errors.New("runtime tar exceeds entry limit")
			}
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Size != 0 {
				return nil, errors.New("non-file runtime tar entry carries a payload")
			}
			name, err := archivePath(h.Name, b)
			if err != nil {
				return nil, err
			}
			kind := ""
			switch h.Typeflag {
			case tar.TypeReg, tar.TypeRegA:
				kind = "file"
			case tar.TypeDir:
				kind = "dir"
			case tar.TypeSymlink:
				kind = "link"
			default:
				return nil, errors.New("runtime tar contains a hardlink, device, or unsupported entry")
			}
			if err := t.add(name, kind, h.Linkname, os.FileMode(h.Mode), h.Size, tr); err != nil {
				return nil, err
			}
		}
		if _, err := io.Copy(io.Discard, expanded); err != nil {
			return nil, err
		}
		if expanded.N == 0 {
			return nil, errors.New("runtime tar exceeds expanded stream limit")
		}
	case "zip":
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return nil, err
		}
		if len(zr.File) > maxEntries {
			return nil, errors.New("runtime zip exceeds entry limit")
		}
		for _, z := range zr.File {
			name, err := archivePath(z.Name, b)
			if err != nil {
				return nil, err
			}
			mode := z.Mode()
			kind := "file"
			if mode.IsDir() {
				kind = "dir"
			} else if mode&os.ModeSymlink != 0 {
				kind = "link"
			} else if !mode.IsRegular() {
				return nil, errors.New("runtime zip contains an unsupported entry")
			}
			if z.UncompressedSize64 > uint64(maxTreeBytes) {
				return nil, errors.New("runtime zip entry exceeds expanded size limit")
			}
			if kind == "dir" && z.UncompressedSize64 != 0 {
				return nil, errors.New("runtime zip directory carries a payload")
			}
			if kind == "file" && name == b.Executable && strings.HasPrefix(b.Platform, "windows/") && strings.HasSuffix(strings.ToLower(name), ".exe") {
				mode |= 0111
			}
			r, err := z.Open()
			if err != nil {
				return nil, err
			}
			target := ""
			if kind == "link" {
				data, readErr := io.ReadAll(io.LimitReader(r, 4097))
				if readErr != nil || len(data) > 4096 {
					r.Close()
					return nil, errors.New("runtime symlink target exceeds limit")
				}
				target = string(data)
			}
			err = t.add(name, kind, target, mode, int64(z.UncompressedSize64), r)
			closeErr := r.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	default:
		return nil, errors.New("unsupported runtime bundle format")
	}
	result, err := t.finish()
	if err != nil {
		return nil, err
	}
	executable, ok := t.entries[b.Executable]
	if !ok || executable.Kind != "file" || executable.Mode&0111 == 0 {
		return nil, errors.New("runtime bundle executable must be an executable regular file")
	}
	after, err := hashRegular(object, store.MaxArtifactBytes)
	if err != nil || after != before {
		return nil, errors.New("runtime artifact changed during extraction")
	}
	return result, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
