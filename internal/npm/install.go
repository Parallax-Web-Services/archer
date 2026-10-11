package npm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxArchiveBytes int64 = 256 << 20
const maxExpandedBytes int64 = 512 << 20
const maxArchiveEntries = 100000
const maxTreeEntries = 200000
const maxTreeBytes int64 = 4 << 30
const maxMarkerBytes = 32 << 20
const installMarker = ".archer-install.json"

type Options struct {
	Home        string
	Offline     bool
	Concurrency int
}

type Report struct {
	Installed  int    `json:"installed"`
	Skipped    int    `json:"skipped"`
	Cached     int    `json:"cached"`
	Downloaded int    `json:"downloaded"`
	Path       string `json:"path"`
	PlanID     string `json:"plan_id"`
}

type treeEntry struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
}

type treeMarker struct {
	Schema    string               `json:"schema"`
	PlanID    string               `json:"plan_id"`
	CacheRoot string               `json:"cache_root"`
	Files     map[string]treeEntry `json:"files"`
}

// Install materializes only pinned registry archives. It never invokes package
// code, lifecycle hooks, npm, a shell, or a dependency solver.
func Install(ctx context.Context, projectDir string, plan *Plan, opts Options) (Report, error) {
	root, err := projectRoot(projectDir)
	if err != nil {
		return Report{}, err
	}
	report := Report{Path: filepath.Join(root, "node_modules")}
	if err := checkPlan(root, plan); err != nil {
		return report, err
	}
	report.PlanID, report.Skipped = plan.ID, len(plan.Skipped)
	if ok, err := Verify(root, plan); err != nil {
		return report, err
	} else if ok {
		report.Installed, report.Cached = len(plan.Packages), len(plan.Packages)
		return report, nil
	}
	unlock, err := lockDirectory(ctx, filepath.Join(root, ".archer-npm-install.lock"))
	if err != nil {
		return report, err
	}
	defer unlock()
	if ok, err := Verify(root, plan); err != nil {
		return report, err
	} else if ok {
		report.Installed, report.Cached = len(plan.Packages), len(plan.Packages)
		return report, nil
	}
	if opts.Home == "" {
		return report, errors.New("npm artifact cache home is required")
	}
	home, err := filepath.Abs(opts.Home)
	if err != nil {
		return report, err
	}
	cacheRoot := filepath.Join(home, "cache", "sha512")
	if err := safeDirectories(cacheRoot, true); err != nil {
		return report, err
	}
	stageParent, err := os.MkdirTemp(root, ".archer-npm-stage-")
	if err != nil {
		return report, err
	}
	defer cleanupStage(root, stageParent)
	stage := filepath.Join(stageParent, "node_modules")
	if err := os.Mkdir(stage, 0755); err != nil {
		return report, err
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return report, err
	}
	blobs, cached, downloaded, err := obtainAll(ctx, cacheRoot, plan.Packages, opts)
	report.Cached, report.Downloaded = cached, downloaded
	if err != nil {
		return report, err
	}
	entries := map[string]treeEntry{".": {Kind: "directory", Mode: directoryMode()}}
	packages := append([]Package{}, plan.Packages...)
	sort.Slice(packages, func(i, j int) bool {
		if len(packages[i].Path) != len(packages[j].Path) {
			return len(packages[i].Path) < len(packages[j].Path)
		}
		return packages[i].Path < packages[j].Path
	})
	for _, pkg := range packages {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		digest, _ := integrityDigest(pkg.Integrity)
		if err := unpack(ctx, blobs[digest], pkg, stage, entries); err != nil {
			return report, fmt.Errorf("extract %s: %w", pkg.Path, err)
		}
		if err := checkTreeBudget(entries); err != nil {
			return report, err
		}
	}
	if err := addBins(packages, stage, entries); err != nil {
		return report, err
	}
	marker := treeMarker{Schema: "archer.npm-install/v1", PlanID: plan.ID, CacheRoot: cacheRoot, Files: entries}
	data, err := json.Marshal(marker)
	if err != nil {
		return report, err
	}
	if len(data) > maxMarkerBytes {
		return report, errors.New("npm tree inventory exceeds 32 MiB")
	}
	if err := os.WriteFile(filepath.Join(stage, installMarker), data, 0600); err != nil {
		return report, err
	}
	if err := verifyTree(stage, entries); err != nil {
		return report, fmt.Errorf("verify staging tree: %w", err)
	}
	if err := checkPlan(root, plan); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if _, err := os.Lstat(report.Path); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("node_modules appeared during installation; refusing replacement")
	}
	if err := os.Rename(stage, report.Path); err != nil {
		return report, fmt.Errorf("publish node_modules: %w", err)
	}
	report.Installed = len(plan.Packages)
	return report, nil
}

// Verify rehashes installed bytes and re-derives their expected metadata from
// SHA-512 verified cache archives. A mutable marker alone never proves integrity.
func Verify(projectDir string, plan *Plan) (bool, error) {
	root, err := projectRoot(projectDir)
	if err != nil {
		return false, err
	}
	if err := checkPlan(root, plan); err != nil {
		return false, err
	}
	target := filepath.Join(root, "node_modules")
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("node_modules is not a managed directory")
	}
	markerPath := filepath.Join(target, installMarker)
	info, err = os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMarkerBytes {
		return false, errors.New("node_modules has no valid Archer marker; refusing unmanaged tree")
	}
	f, err := os.Open(markerPath)
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(io.LimitReader(f, maxMarkerBytes))
	decoder.DisallowUnknownFields()
	var marker treeMarker
	err = decoder.Decode(&marker)
	var extra any
	if err == nil && decoder.Decode(&extra) != io.EOF {
		err = errors.New("trailing marker data")
	}
	f.Close()
	if err != nil || marker.Schema != "archer.npm-install/v1" || marker.Files == nil {
		return false, errors.New("invalid Archer npm tree marker")
	}
	if marker.PlanID != plan.ID {
		return false, errors.New("node_modules belongs to another frozen plan; lockfile updates are not yet supported")
	}
	if !filepath.IsAbs(marker.CacheRoot) {
		return false, errors.New("invalid npm archive cache identity")
	}
	if err := safeDirectories(marker.CacheRoot, false); err != nil {
		return false, fmt.Errorf("verify npm archive cache: %w", err)
	}
	expected := map[string]treeEntry{".": {Kind: "directory", Mode: directoryMode()}}
	for _, pkg := range plan.Packages {
		digest, _ := integrityDigest(pkg.Integrity)
		blob := cachePath(marker.CacheRoot, digest)
		if err := verifyBlob(context.Background(), blob, digest); err != nil {
			return false, fmt.Errorf("verify cached source for %s: %w", pkg.Path, err)
		}
		if err := unpack(context.Background(), blob, pkg, "", expected); err != nil {
			return false, fmt.Errorf("verify archive metadata for %s: %w", pkg.Path, err)
		}
		if err := checkTreeBudget(expected); err != nil {
			return false, err
		}
	}
	if err := addBins(plan.Packages, "", expected); err != nil {
		return false, err
	}
	if len(marker.Files) != len(expected) {
		return false, errors.New("npm marker inventory differs from pinned archives")
	}
	for name, entry := range expected {
		if marker.Files[name] != entry {
			return false, fmt.Errorf("npm marker differs from pinned archive at %s", name)
		}
	}
	if err := verifyTree(target, expected); err != nil {
		return false, err
	}
	return true, nil
}

func projectRoot(value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("npm project root must be an existing directory")
	}
	return abs, nil
}

func checkPlan(root string, plan *Plan) error {
	if plan == nil || plan.Schema != "archer.npm-plan/v1" || !hexDigest(plan.ID, 64) {
		return errors.New("invalid frozen npm plan")
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, "npm-shrinkwrap.json")); err == nil {
		return errors.New("npm-shrinkwrap.json overrides package-lock.json and is unsupported")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	host, err := CurrentPlatform()
	if err != nil {
		return err
	}
	if host != plan.Platform {
		return errors.New("frozen npm target differs from the verified execution host")
	}
	for _, source := range []struct{ name, want string }{{"package.json", plan.ManifestSHA256}, {"package-lock.json", plan.LockSHA256}} {
		if !hexDigest(source.want, 64) {
			return errors.New("invalid npm source identity")
		}
		filename := filepath.Join(root, source.name)
		before, err := os.Lstat(filename)
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() || before.Size() > 16<<20 {
			return errors.New("npm manifest or lockfile must be a bounded regular file without symlinks")
		}
		f, err := os.Open(filename)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 || !os.SameFile(before, info) {
			f.Close()
			return errors.New("npm manifest or lockfile must be a bounded regular file")
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(f, (16<<20)+1))
		after, statErr := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if statErr != nil || n > 16<<20 || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return errors.New("npm source changed while checking its frozen identity")
		}
		if hex.EncodeToString(h.Sum(nil)) != source.want {
			return fmt.Errorf("%s changed since npm planning", source.name)
		}
	}
	seen := map[string]bool{}
	for _, pkg := range plan.Packages {
		if _, err := packageRelative(pkg.Path); err != nil {
			return err
		}
		if seen[pkg.Path] {
			return errors.New("duplicate npm package placement")
		}
		seen[pkg.Path] = true
		if pkg.Name == "" || pkg.Version == "" {
			return errors.New("npm package name and version are required")
		}
		if pkg.HasInstallScript {
			return fmt.Errorf("lifecycle installation is unsupported for %s", pkg.Name)
		}
		if _, err := integrityDigest(pkg.Integrity); err != nil {
			return err
		}
		if _, err := archiveURL(pkg.URL); err != nil {
			return err
		}
		for name, target := range pkg.Bin {
			if !safeBinName(name) || !safeArchiveRelative(target) {
				return errors.New("unsafe npm binary entry")
			}
		}
		if runtime.GOOS == "windows" && len(pkg.Bin) > 0 {
			return errors.New("npm executable links currently require a POSIX host")
		}
	}
	return nil
}
func hexDigest(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func integrityDigest(value string) (string, error) {
	if !strings.HasPrefix(value, "sha512-") || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("npm archive requires one SHA-512 SRI digest")
	}
	bytes, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(value, "sha512-"))
	if err != nil || len(bytes) != sha512.Size {
		return "", errors.New("invalid SHA-512 SRI digest")
	}
	return hex.EncodeToString(bytes), nil
}
func safeArchiveRelative(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func packageRelative(value string) (string, error) {
	if !strings.HasPrefix(value, "node_modules/") || !safeArchiveRelative(value) {
		return "", fmt.Errorf("unsafe npm package placement %q", value)
	}
	rel := strings.TrimPrefix(value, "node_modules/")
	for _, part := range strings.Split(rel, "/") {
		if part == ".bin" || part == installMarker {
			return "", errors.New("reserved npm package placement")
		}
	}
	return rel, nil
}
func safeBinName(value string) bool {
	return safeArchiveRelative(value) && !strings.Contains(value, "/") && value != installMarker
}

// npm publishers sometimes include redundant ./ path components. They may be
// removed only after rejecting traversal, absolute paths and empty components.
func normalizedArchivePath(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") {
		return "", errors.New("unsafe npm archive path")
	}
	parts := []string{}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "" {
			return "", errors.New("unsafe npm archive path")
		}
		if part != "." {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/"), nil
}
func directoryMode() uint32 {
	if runtime.GOOS == "windows" {
		return 0777
	}
	return 0755
}
func fileMode(executable bool) uint32 {
	if runtime.GOOS == "windows" {
		return 0666
	}
	if executable {
		return 0755
	}
	return 0644
}

func cachePath(root, digest string) string { return filepath.Join(root, digest[:2], digest) }
func safeDirectories(value string, create bool) error {
	for current := filepath.Clean(value); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("npm path contains a symlink or non-directory")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if create {
		return os.MkdirAll(value, 0700)
	}
	_, err := os.Stat(value)
	return err
}
func lockDirectory(ctx context.Context, value string) (func(), error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := os.Mkdir(value, 0700)
		if err == nil {
			return func() { _ = os.Remove(value) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err := os.Lstat(value)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe npm lock path")
		}
		if time.Now().After(deadline) {
			return nil, errors.New("npm cache or project is locked; inspect abandoned lock")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func cleanupStage(root, stage string) {
	abs, err := filepath.Abs(stage)
	if err != nil || filepath.Dir(abs) != root || !strings.HasPrefix(filepath.Base(abs), ".archer-npm-stage-") {
		return
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(abs)
}

func obtainAll(ctx context.Context, root string, packages []Package, opts Options) (map[string]string, int, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	count := opts.Concurrency
	if count <= 0 {
		count = 8
	}
	if count > 16 {
		count = 16
	}
	type result struct {
		digest, path string
		cached       bool
		err          error
	}
	jobs := make(chan Package)
	results := make(chan result, len(packages))
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for pkg := range jobs {
				digest, _ := integrityDigest(pkg.Integrity)
				file, cached, err := obtain(ctx, root, pkg, opts.Offline)
				results <- result{digest, file, cached, err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, pkg := range packages {
			select {
			case jobs <- pkg:
			case <-ctx.Done():
				return
			}
		}
	}()
	group.Wait()
	close(results)
	blobs := map[string]string{}
	cached, downloaded := 0, 0
	var failure error
	for result := range results {
		if result.err != nil {
			if failure == nil || errors.Is(failure, context.Canceled) {
				failure = result.err
			}
			continue
		}
		blobs[result.digest] = result.path
		if result.cached {
			cached++
		} else {
			downloaded++
		}
	}
	if failure == nil {
		failure = ctx.Err()
	}
	return blobs, cached, downloaded, failure
}
func obtain(ctx context.Context, root string, pkg Package, offline bool) (string, bool, error) {
	digest, err := integrityDigest(pkg.Integrity)
	if err != nil {
		return "", false, err
	}
	file := cachePath(root, digest)
	if err := verifyBlob(ctx, file, digest); err == nil {
		return file, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if offline {
		return "", false, fmt.Errorf("archive for %s is unavailable in offline cache", pkg.Name)
	}
	if err := safeDirectories(filepath.Dir(file), true); err != nil {
		return "", false, err
	}
	unlock, err := lockDirectory(ctx, file+".lock")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	if err := verifyBlob(ctx, file, digest); err == nil {
		return file, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	u, err := archiveURL(pkg.URL)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", false, err
	}
	client := archiveClient(u)
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, errors.New("npm archive request failed or redirect policy rejected")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("npm archive returned HTTP %d", response.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".npm-download-")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	h := sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(response.Body, maxArchiveBytes+1))
	if err == nil && n > maxArchiveBytes {
		err = errors.New("npm archive exceeds 256 MiB")
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != digest {
		err = errors.New("npm archive SHA-512 integrity mismatch")
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return "", false, err
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	if err := os.Link(tmp.Name(), file); err != nil {
		if check := verifyBlob(ctx, file, digest); check == nil {
			return file, true, nil
		}
		return "", false, fmt.Errorf("publish npm archive: %w", err)
	}
	return file, false, nil
}
func verifyBlob(ctx context.Context, file, want string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := safeDirectories(filepath.Dir(file), false); err != nil {
		return err
	}
	before, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() > maxArchiveBytes {
		return errors.New("npm cache object is not a bounded regular archive")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("npm cache object changed while opening")
	}
	h := sha512.New()
	_, err = io.Copy(h, &cancelReader{ctx, io.LimitReader(f, maxArchiveBytes+1)})
	if err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("npm cache archive integrity mismatch")
	}
	return nil
}

type cancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func archiveURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("npm archive requires an absolute credential-free HTTPS URL")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("npm archive requires HTTPS outside direct loopback fixtures")
	}
	for key := range u.Query() {
		switch strings.ToLower(key) {
		case "token", "access_token", "api_key", "apikey", "password", "secret", "authorization":
			return nil, errors.New("npm archive URL cannot contain credentials")
		}
	}
	return u, nil
}
func archiveOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
func archiveClient(u *url.URL) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "http" {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(host, "localhost") {
				host = "127.0.0.1"
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil, errors.New("fixture must dial direct loopback")
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, port))
		}
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		checked, err := archiveURL(req.URL.String())
		if err != nil || len(via) >= 5 || archiveOrigin(checked) != archiveOrigin(u) {
			return errors.New("npm archive redirect violates origin policy")
		}
		return nil
	}}
}

func unpack(ctx context.Context, blob string, pkg Package, stage string, entries map[string]treeEntry) error {
	rel, err := packageRelative(pkg.Path)
	if err != nil {
		return err
	}
	if err := addDirectory(rel, stage, entries); err != nil {
		return err
	}
	f, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return errors.New("npm archive is not gzip")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: &cancelReader{ctx, gz}, N: maxExpandedBytes + 1}
	reader := tar.NewReader(limited)
	count := 0
	var packageJSON []byte
	bindingGyp := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > maxArchiveEntries {
			return errors.New("npm archive has too many entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "package" && header.Typeflag == tar.TypeDir {
			continue
		}
		if !strings.HasPrefix(name, "package/") {
			return errors.New("npm archive entries must have the package/ prefix")
		}
		inner, err := normalizedArchivePath(strings.TrimPrefix(name, "package/"))
		if err != nil || len(inner) > 4096 {
			return errors.New("unsafe npm archive path")
		}
		if inner == "" {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return errors.New("npm archive file has no package-relative name")
		}
		if inner == "node_modules" || strings.HasPrefix(inner, "node_modules/") {
			return errors.New("bundled dependency trees are unsupported")
		}
		entryPath := path.Join(rel, inner)
		if inner == "binding.gyp" {
			bindingGyp = true
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := addDirectory(entryPath, stage, entries); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxExpandedBytes {
				return errors.New("npm archive file exceeds expanded size limit")
			}
			existing, duplicate := entries[entryPath]
			if duplicate && existing.Kind != "file" {
				return errors.New("duplicate npm archive entry collides with a directory")
			}
			if err := addDirectory(path.Dir(entryPath), stage, entries); err != nil {
				return err
			}
			h := sha256.New()
			destination := io.Writer(h)
			var out *os.File
			mode := fileMode(header.Mode&0111 != 0)
			if stage != "" && !duplicate {
				out, err = os.OpenFile(filepath.Join(stage, filepath.FromSlash(entryPath)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode))
				if err != nil {
					return err
				}
				if err := out.Chmod(os.FileMode(mode)); err != nil {
					out.Close()
					return err
				}
				destination = io.MultiWriter(out, h)
			}
			var capture strings.Builder
			if inner == "package.json" {
				if header.Size > 2<<20 {
					if out != nil {
						out.Close()
					}
					return errors.New("downloaded package.json exceeds 2 MiB")
				}
				destination = io.MultiWriter(destination, &capture)
			}
			n, copyErr := io.Copy(destination, reader)
			if out != nil {
				if closeErr := out.Close(); copyErr == nil {
					copyErr = closeErr
				}
			}
			if copyErr != nil {
				return copyErr
			}
			if n != header.Size {
				return errors.New("truncated npm archive file")
			}
			candidate := treeEntry{Kind: "file", SHA256: hex.EncodeToString(h.Sum(nil)), Mode: mode, Size: n}
			if duplicate && candidate != existing {
				return errors.New("duplicate npm archive file has conflicting contents, size or mode")
			}
			entries[entryPath] = candidate
			if inner == "package.json" {
				packageJSON = []byte(capture.String())
			}
		default:
			return errors.New("npm archives cannot contain symlinks, hard links, devices or special files")
		}
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("npm archive exceeds 512 MiB expanded size limit")
	}
	var manifest struct {
		Name    string                     `json:"name"`
		Version string                     `json:"version"`
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if len(packageJSON) == 0 || json.Unmarshal(packageJSON, &manifest) != nil || manifest.Name != pkg.Name || manifest.Version != pkg.Version {
		return errors.New("downloaded package identity differs from frozen lock")
	}
	for _, hook := range []string{"preinstall", "install", "postinstall"} {
		if raw, exists := manifest.Scripts[hook]; exists {
			var command string
			if json.Unmarshal(raw, &command) != nil || command != "" {
				return fmt.Errorf("downloaded package requires unsupported %s lifecycle hook", hook)
			}
		}
	}
	if bindingGyp {
		return errors.New("downloaded package contains binding.gyp; native build lifecycle is unsupported")
	}
	return nil
}
func addDirectory(value, stage string, entries map[string]treeEntry) error {
	if value == "." {
		return nil
	}
	if !safeArchiveRelative(value) {
		return errors.New("unsafe npm directory path")
	}
	if existing, ok := entries[value]; ok {
		if existing.Kind != "directory" {
			return errors.New("npm archive directory collides with a file")
		}
		return nil
	}
	if err := addDirectory(path.Dir(value), stage, entries); err != nil {
		return err
	}
	if stage != "" {
		if err := os.Mkdir(filepath.Join(stage, filepath.FromSlash(value)), 0755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Chmod(filepath.Join(stage, filepath.FromSlash(value)), 0755); err != nil {
			return err
		}
	}
	entries[value] = treeEntry{Kind: "directory", Mode: directoryMode()}
	return nil
}
func addBins(packages []Package, stage string, entries map[string]treeEntry) error {
	for _, pkg := range packages {
		rel, _ := packageRelative(pkg.Path)
		parent := path.Dir(rel)
		if strings.HasPrefix(path.Base(parent), "@") {
			parent = path.Dir(parent)
		}
		binRoot := path.Join(parent, ".bin")
		for name, target := range pkg.Bin {
			if !safeBinName(name) || !safeArchiveRelative(target) {
				return errors.New("unsafe npm bin link")
			}
			filePath := path.Join(rel, target)
			file, ok := entries[filePath]
			if !ok || file.Kind != "file" {
				return fmt.Errorf("npm bin %s has no regular package target", name)
			}
			if runtime.GOOS == "windows" {
				return errors.New("npm executable links currently require a POSIX host")
			}
			linkPath := path.Join(binRoot, name)
			relative, err := filepath.Rel(filepath.FromSlash(binRoot), filepath.FromSlash(filePath))
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if existing, ok := entries[linkPath]; ok {
				if existing.Kind == "symlink" && existing.Target == relative {
					continue
				}
				return fmt.Errorf("conflicting npm binary name %s", name)
			}
			if err := addDirectory(binRoot, stage, entries); err != nil {
				return err
			}
			file.Mode = fileMode(true)
			entries[filePath] = file
			if stage != "" {
				if err := os.Chmod(filepath.Join(stage, filepath.FromSlash(filePath)), 0755); err != nil {
					return err
				}
				if err := os.Symlink(relative, filepath.Join(stage, filepath.FromSlash(linkPath))); err != nil {
					return err
				}
			}
			entries[linkPath] = treeEntry{Kind: "symlink", Target: relative, Mode: 0777}
		}
	}
	return nil
}
func verifyTree(root string, expected map[string]treeEntry) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == installMarker {
			return nil
		}
		// Vite removes its transient config bundle but can leave this empty
		// directory. Permit only that exact empty cache, never arbitrary output.
		if rel == ".vite-temp" {
			info, err := entry.Info()
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
				return errors.New("Vite temporary cache must be a safe empty directory")
			}
			contents, err := os.ReadDir(file)
			if err != nil || len(contents) != 0 {
				return errors.New("Vite temporary cache is nonempty; finish the build before verification")
			}
			return filepath.SkipDir
		}
		want, ok := expected[rel]
		if !ok {
			return fmt.Errorf("unmanaged npm tree entry %s", rel)
		}
		seen[rel] = true
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch want.Kind {
		case "directory":
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("npm directory changed at %s", rel)
			}
		case "symlink":
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("npm bin link changed at %s", rel)
			}
			target, err := os.Readlink(file)
			if err != nil || filepath.ToSlash(target) != want.Target {
				return fmt.Errorf("npm bin link target changed at %s", rel)
			}
		case "file":
			if !info.Mode().IsRegular() || info.Size() != want.Size {
				return fmt.Errorf("npm file changed at %s", rel)
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err != nil || !os.SameFile(info, opened) {
				f.Close()
				return errors.New("npm file changed while opening")
			}
			h := sha256.New()
			_, err = io.Copy(h, io.LimitReader(f, want.Size+1))
			after, statErr := f.Stat()
			f.Close()
			if err != nil || statErr != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
				return fmt.Errorf("npm file checksum changed at %s", rel)
			}
		default:
			return errors.New("invalid npm tree entry kind")
		}
		if uint32(info.Mode().Perm()) != want.Mode {
			return fmt.Errorf("npm tree mode changed at %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("npm tree is missing pinned files")
	}
	return nil
}

func checkTreeBudget(entries map[string]treeEntry) error {
	if len(entries) > maxTreeEntries {
		return errors.New("npm tree exceeds 200000 entries")
	}
	var total int64
	for _, entry := range entries {
		if entry.Kind == "file" {
			total += entry.Size
			if total > maxTreeBytes {
				return errors.New("npm tree exceeds 4 GiB expanded file content")
			}
		}
	}
	return nil
}
