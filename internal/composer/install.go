package composer

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
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
	"strings"
	"sync"
	"time"

	"github.com/Parallax-Web-Services/archer/internal/parallax"
)

const installArchiveLimit int64 = 256 << 20
const installExpandedLimit int64 = 512 << 20
const installTreeLimit int64 = 4 << 30
const installEntryLimit = 200000
const installMarkerLimit int64 = 32 << 20
const composerMarker = ".archer-install.json"

type Options struct {
	Home        string
	Offline     bool
	Concurrency int
}

type Report struct {
	Installed  int    `json:"installed"`
	Cached     int    `json:"cached"`
	Downloaded int    `json:"downloaded"`
	Path       string `json:"path"`
	PlanID     string `json:"plan_id"`
}

type installEntry struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Mode   uint32 `json:"mode"`
}

type installInventory struct {
	Schema    string                  `json:"schema"`
	PlanID    string                  `json:"plan_id"`
	CacheRoot string                  `json:"cache_root"`
	Files     map[string]installEntry `json:"files"`
}

// A source receipt records the first observed bytes under the explicitly chosen
// source policy. Its SHA-256 detects later changes; it is not a publisher signature.
type installReceipt struct {
	Schema    string `json:"schema"`
	URL       string `json:"url"`
	SourceURL string `json:"source_url,omitempty"`
	FetchURL  string `json:"fetch_url"`
	Reference string `json:"reference"`
	SHA1      string `json:"lock_sha1,omitempty"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// Install replays immutable ZIP distributions and generates autoload metadata.
// It does not run Composer, package scripts, plugins, or application code.
func Install(ctx context.Context, projectDir string, plan *Plan, opts Options) (Report, error) {
	root, err := installProjectRoot(projectDir)
	if err != nil {
		return Report{}, err
	}
	report := Report{Path: filepath.Join(root, "vendor")}
	if err := installCheckPlan(root, plan); err != nil {
		return report, err
	}
	report.PlanID = plan.ID
	if ok, err := Verify(root, plan); err != nil {
		return report, err
	} else if ok {
		report.Installed, report.Cached = len(plan.Packages), len(plan.Packages)
		return report, nil
	}
	unlock, err := installLock(ctx, filepath.Join(root, ".archer-composer-install.lock"))
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
		return report, errors.New("Composer archive cache home is required")
	}
	home, err := filepath.Abs(opts.Home)
	if err != nil {
		return report, err
	}
	cache := filepath.Join(home, "cache", "composer", "sources")
	if err := installDirectories(cache, true); err != nil {
		return report, err
	}
	stageParent, err := os.MkdirTemp(root, ".archer-composer-stage-")
	if err != nil {
		return report, err
	}
	defer installCleanup(root, stageParent, ".archer-composer-stage-")
	stage := filepath.Join(stageParent, "vendor")
	if err := os.Mkdir(stage, 0755); err != nil {
		return report, err
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return report, err
	}
	blobs, cached, downloaded, err := installObtainAll(ctx, cache, plan, opts)
	report.Cached, report.Downloaded = cached, downloaded
	if err != nil {
		return report, err
	}
	entries := map[string]installEntry{".": {Kind: "directory", Mode: installDirectoryMode()}}
	for _, pkg := range plan.Packages {
		if err := installUnpack(ctx, blobs[installSourceKey(pkg)], pkg, stage, entries); err != nil {
			return report, fmt.Errorf("extract %s: %w", pkg.Name, err)
		}
		if err := installBudget(entries); err != nil {
			return report, err
		}
	}
	generated, err := AutoloadFiles(root, stage, plan)
	if err != nil {
		return report, err
	}
	if err := installGenerated(stage, generated, entries); err != nil {
		return report, err
	}
	if err := installBudget(entries); err != nil {
		return report, err
	}
	marker := installInventory{Schema: "archer.composer-install/v1", PlanID: plan.ID, CacheRoot: cache, Files: entries}
	data, err := json.Marshal(marker)
	if err != nil {
		return report, err
	}
	if int64(len(data)) > installMarkerLimit {
		return report, errors.New("Composer tree inventory exceeds 32 MiB")
	}
	if err := os.WriteFile(filepath.Join(stage, composerMarker), data, 0600); err != nil {
		return report, err
	}
	if err := installVerifyTree(stage, entries, true); err != nil {
		return report, fmt.Errorf("verify vendor staging tree: %w", err)
	}
	if err := installCheckPlan(root, plan); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if _, err := os.Lstat(report.Path); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("vendor appeared during installation; refusing replacement")
	}
	if err := os.Rename(stage, report.Path); err != nil {
		return report, fmt.Errorf("publish vendor: %w", err)
	}
	report.Installed = len(plan.Packages)
	return report, nil
}

// Verify derives file expectations from the immutable source receipts and
// rehashed ZIP archives, then regenerates autoload files from verified sources.
// The mutable vendor marker alone never establishes an installed package.
func Verify(projectDir string, plan *Plan) (bool, error) {
	root, err := installProjectRoot(projectDir)
	if err != nil {
		return false, err
	}
	if err := installCheckPlan(root, plan); err != nil {
		return false, err
	}
	target := filepath.Join(root, "vendor")
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("vendor is not a managed directory")
	}
	var marker installInventory
	if err := installReadJSON(filepath.Join(target, composerMarker), installMarkerLimit, &marker); err != nil {
		return false, errors.New("vendor has no valid Archer marker; refusing unmanaged tree")
	}
	if marker.Schema != "archer.composer-install/v1" || marker.Files == nil {
		return false, errors.New("invalid Archer Composer marker")
	}
	if marker.PlanID != plan.ID {
		return false, errors.New("vendor belongs to another frozen plan; lockfile updates are not yet supported")
	}
	if !filepath.IsAbs(marker.CacheRoot) {
		return false, errors.New("invalid Composer cache identity")
	}
	if err := installDirectories(marker.CacheRoot, false); err != nil {
		return false, err
	}
	expected := map[string]installEntry{".": {Kind: "directory", Mode: installDirectoryMode()}}
	for _, pkg := range plan.Packages {
		blob, err := installVerifySource(context.Background(), marker.CacheRoot, pkg, plan.Policy)
		if err != nil {
			return false, fmt.Errorf("verify cached source for %s: %w", pkg.Name, err)
		}
		if err := installUnpack(context.Background(), blob, pkg, "", expected); err != nil {
			return false, fmt.Errorf("verify ZIP for %s: %w", pkg.Name, err)
		}
		if err := installBudget(expected); err != nil {
			return false, err
		}
	}
	// Verify package bytes before reading them to derive generated metadata.
	if err := installVerifyTree(target, expected, false); err != nil {
		return false, err
	}
	generated, err := AutoloadFiles(root, target, plan)
	if err != nil {
		return false, err
	}
	if err := installGenerated("", generated, expected); err != nil {
		return false, err
	}
	if err := installBudget(expected); err != nil {
		return false, err
	}
	if len(marker.Files) != len(expected) {
		return false, errors.New("vendor marker inventory differs from frozen sources")
	}
	for name, entry := range expected {
		if marker.Files[name] != entry {
			return false, fmt.Errorf("vendor marker differs from frozen source at %s", name)
		}
	}
	if err := installVerifyTree(target, expected, true); err != nil {
		return false, err
	}
	return true, nil
}

func installProjectRoot(value string) (string, error) {
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
		return "", errors.New("Composer project root must be an existing directory")
	}
	return abs, nil
}

func installCheckPlan(root string, plan *Plan) error {
	if err := ValidatePlan(plan); err != nil {
		return err
	}
	for _, source := range []struct{ name, digest string }{{"composer.json", plan.ManifestSHA256}, {"composer.lock", plan.LockSHA256}} {
		limit := int64(16 << 20)
		if source.name == "composer.json" {
			limit = 2 << 20
		}
		sum, _, err := installHashFile(context.Background(), filepath.Join(root, source.name), limit)
		if err != nil {
			return fmt.Errorf("check %s: %w", source.name, err)
		}
		if sum != source.digest {
			return fmt.Errorf("%s changed since Composer planning", source.name)
		}
	}
	for _, pkg := range plan.Packages {
		if _, err := installPackageRelative(pkg.Path, pkg.Name); err != nil {
			return err
		}
		if _, err := installArchiveIdentity(pkg.URL, pkg.Reference, plan.Policy); err != nil {
			return err
		}
	}
	return nil
}

func installSafeRelative(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func installPackageRelative(value, name string) (string, error) {
	if value != "vendor/"+name || !installSafeRelative(value) {
		return "", errors.New("unsafe Composer package placement")
	}
	return strings.TrimPrefix(value, "vendor/"), nil
}

func installDirectoryMode() uint32 {
	if runtime.GOOS == "windows" {
		return 0777
	}
	return 0755
}
func installFileMode(executable bool) uint32 {
	if runtime.GOOS == "windows" {
		return 0666
	}
	if executable {
		return 0755
	}
	return 0644
}

func installDirectories(value string, create bool) error {
	for current := filepath.Clean(value); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("Composer path contains a symlink or non-directory")
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

func installLock(ctx context.Context, value string) (func(), error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.Mkdir(value, 0700); err == nil {
			return func() { _ = os.Remove(value) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err := os.Lstat(value)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe Composer lock path")
		}
		if time.Now().After(deadline) {
			return nil, errors.New("Composer cache or project is locked; inspect abandoned lock")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func installCleanup(parent, value, prefix string) {
	abs, err := filepath.Abs(value)
	if err != nil || filepath.Dir(abs) != parent || !strings.HasPrefix(filepath.Base(abs), prefix) {
		return
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(abs)
}

func installSourceKey(pkg Package) string {
	h := sha256.Sum256([]byte(pkg.URL + "\x00" + pkg.SourceURL + "\x00" + pkg.Reference + "\x00" + pkg.SHA1))
	return hex.EncodeToString(h[:])
}

func installSourceDirectory(root string, pkg Package) string {
	key := installSourceKey(pkg)
	return filepath.Join(root, key[:2], key)
}

func installObtainAll(ctx context.Context, root string, plan *Plan, opts Options) (map[string]string, int, int, error) {
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
		key, file string
		cached    bool
		err       error
	}
	jobs := make(chan Package)
	results := make(chan result, len(plan.Packages))
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for pkg := range jobs {
				file, cached, err := installObtain(ctx, root, pkg, plan.Policy, opts.Offline)
				results <- result{installSourceKey(pkg), file, cached, err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, pkg := range plan.Packages {
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
		blobs[result.key] = result.file
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

func installObtain(ctx context.Context, root string, pkg Package, policy SourcePolicy, offline bool) (string, bool, error) {
	if file, err := installVerifySource(ctx, root, pkg, policy); err == nil {
		return file, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if offline {
		return "", false, fmt.Errorf("archive for %s is unavailable in offline cache", pkg.Name)
	}
	directory := installSourceDirectory(root, pkg)
	if err := installDirectories(filepath.Dir(directory), true); err != nil {
		return "", false, err
	}
	unlock, err := installLock(ctx, directory+".lock")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	if file, err := installVerifySource(ctx, root, pkg, policy); err == nil {
		return file, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	identity, err := installArchiveIdentity(pkg.URL, pkg.Reference, policy)
	if err != nil {
		return "", false, err
	}
	fetch := identity.url
	// This deterministic GitHub archive route preserves the frozen repository and
	// commit while avoiding an API request for every package. Both hosts must be trusted.
	if strings.EqualFold(fetch.Hostname(), "api.github.com") && installHostTrusted(policy, "codeload.github.com") {
		fetch, _ = url.Parse("https://codeload.github.com/" + identity.owner + "/" + identity.repository + "/legacy.zip/" + pkg.Reference)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetch.String(), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "Archer/0.1 frozen Composer replay")
	client := installArchiveClient(identity, policy)
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, errors.New("Composer archive request failed or redirect policy rejected")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("Composer archive returned HTTP %d for %s", response.StatusCode, pkg.Name)
	}
	if response.ContentLength > installArchiveLimit {
		return "", false, errors.New("Composer archive exceeds 256 MiB")
	}
	stage, err := os.MkdirTemp(filepath.Dir(directory), ".composer-source-")
	if err != nil {
		return "", false, err
	}
	defer installCleanup(filepath.Dir(directory), stage, ".composer-source-")
	archive, err := os.OpenFile(filepath.Join(stage, "archive.zip"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", false, err
	}
	h256, h1 := sha256.New(), sha1.New()
	n, copyErr := io.Copy(io.MultiWriter(archive, h256, h1), &installCancelReader{ctx, io.LimitReader(response.Body, installArchiveLimit+1)})
	if copyErr == nil && n > installArchiveLimit {
		copyErr = errors.New("Composer archive exceeds 256 MiB")
	}
	if copyErr == nil && pkg.SHA1 != "" && hex.EncodeToString(h1.Sum(nil)) != pkg.SHA1 {
		copyErr = errors.New("Composer archive lock SHA-1 mismatch")
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = archive.Sync()
	}
	if closeErr := archive.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", false, copyErr
	}
	receipt := installReceipt{Schema: "archer.composer-source/v1", URL: pkg.URL, SourceURL: pkg.SourceURL, FetchURL: response.Request.URL.String(), Reference: pkg.Reference, SHA1: pkg.SHA1, SHA256: hex.EncodeToString(h256.Sum(nil)), Size: n}
	data, err := json.Marshal(receipt)
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(stage, "receipt.json"), data, 0444); err != nil {
		return "", false, err
	}
	if err := os.Chmod(filepath.Join(stage, "archive.zip"), 0444); err != nil {
		return "", false, err
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		return "", false, errors.New("Composer source cache appeared during download; refusing replacement")
	}
	if err := os.Rename(stage, directory); err != nil {
		return "", false, err
	}
	return filepath.Join(directory, "archive.zip"), false, nil
}

func installVerifySource(ctx context.Context, root string, pkg Package, policy SourcePolicy) (string, error) {
	directory := installSourceDirectory(root, pkg)
	if err := installDirectories(directory, false); err != nil {
		return "", err
	}
	var receipt installReceipt
	if err := installReadJSON(filepath.Join(directory, "receipt.json"), 16<<10, &receipt); err != nil {
		return "", errors.New("Composer source cache has no valid first-seen receipt; refusing replacement")
	}
	if receipt.Schema != "archer.composer-source/v1" || receipt.URL != pkg.URL || receipt.SourceURL != pkg.SourceURL || receipt.Reference != pkg.Reference || receipt.SHA1 != pkg.SHA1 || !installHex(receipt.SHA256, 64) || receipt.Size < 0 || receipt.Size > installArchiveLimit {
		return "", errors.New("Composer first-seen source receipt differs from frozen identity")
	}
	identity, err := installArchiveIdentity(receipt.URL, receipt.Reference, policy)
	if err != nil {
		return "", err
	}
	actual, err := installArchiveIdentity(receipt.FetchURL, receipt.Reference, policy)
	if err != nil || !installSameArchive(identity, actual) {
		return "", errors.New("Composer source receipt fetch route violates frozen identity")
	}
	file := filepath.Join(directory, "archive.zip")
	digest, size, err := installHashFile(ctx, file, installArchiveLimit)
	if err != nil {
		return "", errors.New("Composer source cache archive is missing or unsafe; refusing replacement")
	}
	if digest != receipt.SHA256 || size != receipt.Size {
		return "", errors.New("Composer cached archive differs from first-seen SHA-256 receipt")
	}
	if pkg.SHA1 != "" {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		h := sha1.New()
		_, err = io.Copy(h, &installCancelReader{ctx, io.LimitReader(f, installArchiveLimit+1)})
		f.Close()
		if err != nil {
			return "", err
		}
		if hex.EncodeToString(h.Sum(nil)) != pkg.SHA1 {
			return "", errors.New("Composer cached archive lock SHA-1 mismatch")
		}
	}
	return file, nil
}

func installReadJSON(filename string, limit int64, value any) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("expected bounded regular JSON file")
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("JSON file changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(f, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func installHashFile(ctx context.Context, filename string, limit int64) (string, int64, error) {
	if err := installDirectories(filepath.Dir(filename), false); err != nil {
		return "", 0, err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return "", 0, errors.New("expected bounded regular file without symlinks")
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", 0, errors.New("file changed while opening")
	}
	h := sha256.New()
	n, err := io.Copy(h, &installCancelReader{ctx, io.LimitReader(f, limit+1)})
	if err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil || n > limit || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("file changed while checking frozen identity")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type installCancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *installCancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func installHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type installArchive struct {
	url                          *url.URL
	owner, repository, reference string
}

func installHostTrusted(policy SourcePolicy, host string) bool {
	if policy.Mode == "checksum" && (strings.EqualFold(host, "api.github.com") || strings.EqualFold(host, "codeload.github.com")) {
		return true
	}
	for _, allowed := range policy.TrustedHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}
func installArchiveIdentity(raw, reference string, policy SourcePolicy) (installArchive, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || !installHostTrusted(policy, u.Hostname()) || !installHex(reference, 40) {
		return installArchive{}, errors.New("Composer archive URL violates explicit source policy")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return installArchive{}, errors.New("Composer archive requires HTTPS outside direct loopback fixtures")
	}
	if !loopback && u.Port() != "" && u.Port() != "443" {
		return installArchive{}, errors.New("Composer archive requires standard HTTPS port")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	identity := installArchive{url: u, reference: reference}
	switch {
	case (strings.EqualFold(u.Hostname(), "api.github.com") || loopback) && len(parts) == 5 && parts[0] == "repos" && parts[3] == "zipball" && parts[4] == reference:
		identity.owner, identity.repository = parts[1], parts[2]
	case (strings.EqualFold(u.Hostname(), "codeload.github.com") || loopback) && len(parts) == 4 && (parts[2] == "zip" || parts[2] == "legacy.zip") && parts[3] == reference:
		identity.owner, identity.repository = parts[0], parts[1]
	default:
		return installArchive{}, errors.New("Composer archive must identify one GitHub repository and immutable commit")
	}
	if identity.owner == "" || identity.repository == "" || !installSafeRelative(identity.owner) || !installSafeRelative(identity.repository) || strings.Contains(identity.owner, "/") || strings.Contains(identity.repository, "/") {
		return installArchive{}, errors.New("invalid GitHub archive repository")
	}
	return identity, nil
}
func installSameArchive(a, b installArchive) bool {
	return strings.EqualFold(a.owner, b.owner) && strings.EqualFold(a.repository, b.repository) && a.reference == b.reference
}
func installArchiveClient(identity installArchive, policy SourcePolicy) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if identity.url.Scheme == "http" {
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
		actual, err := installArchiveIdentity(req.URL.String(), identity.reference, policy)
		if err != nil || len(via) >= 5 || !installSameArchive(identity, actual) {
			return errors.New("Composer archive redirect violates frozen repository and commit")
		}
		if identity.url.Scheme == "http" && (actual.url.Scheme != "http" || actual.url.Host != identity.url.Host) {
			return errors.New("fixture redirect violates direct origin")
		}
		return nil
	}}
}

func installUnpack(ctx context.Context, blob string, pkg Package, stage string, entries map[string]installEntry) error {
	rel, err := installPackageRelative(pkg.Path, pkg.Name)
	if err != nil {
		return err
	}
	if err := installAddDirectory(rel, stage, entries); err != nil {
		return err
	}
	reader, err := zip.OpenReader(blob)
	if err != nil {
		return errors.New("Composer distribution is not a ZIP archive")
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > 100000 {
		return errors.New("Composer ZIP entry count is invalid")
	}
	archiveRoot := ""
	var expanded int64
	var manifest []byte
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(file.Name, "/")
		if len(name) > 4096 || !installSafeRelative(name) {
			return errors.New("unsafe Composer ZIP path")
		}
		parts := strings.SplitN(name, "/", 2)
		if archiveRoot == "" {
			archiveRoot = parts[0]
		}
		if parts[0] != archiveRoot {
			return errors.New("Composer ZIP must contain one root directory")
		}
		if len(parts) == 1 {
			if !file.FileInfo().IsDir() {
				return errors.New("Composer ZIP root must be a directory")
			}
			continue
		}
		inner := parts[1]
		destination := path.Join(rel, inner)
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return errors.New("Composer ZIP cannot contain symlinks, devices or special files")
		}
		if file.FileInfo().IsDir() {
			if err := installAddDirectory(destination, stage, entries); err != nil {
				return err
			}
			continue
		}
		if file.UncompressedSize64 > uint64(installExpandedLimit) || uint64(expanded)+file.UncompressedSize64 > uint64(installExpandedLimit) {
			return errors.New("Composer ZIP exceeds 512 MiB expanded size")
		}
		expanded += int64(file.UncompressedSize64)
		if inner == "composer.json" && file.UncompressedSize64 > 2<<20 {
			return errors.New("downloaded composer.json exceeds 2 MiB")
		}
		if err := installAddDirectory(path.Dir(destination), stage, entries); err != nil {
			return err
		}
		existing, duplicate := entries[destination]
		if duplicate && existing.Kind != "file" {
			return errors.New("Composer ZIP file collides with a directory")
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		writer := io.Writer(h)
		var output *os.File
		fileMode := installFileMode(mode.Perm()&0111 != 0)
		if stage != "" && !duplicate {
			output, err = os.OpenFile(filepath.Join(stage, filepath.FromSlash(destination)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(fileMode))
			if err != nil {
				source.Close()
				return err
			}
			if err := output.Chmod(os.FileMode(fileMode)); err != nil {
				output.Close()
				source.Close()
				return err
			}
			writer = io.MultiWriter(output, h)
		}
		var capture strings.Builder
		if inner == "composer.json" {
			writer = io.MultiWriter(writer, &capture)
		}
		n, copyErr := io.Copy(writer, &installCancelReader{ctx, io.LimitReader(source, int64(file.UncompressedSize64)+1)})
		if closeErr := source.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if output != nil {
			if closeErr := output.Close(); copyErr == nil {
				copyErr = closeErr
			}
		}
		if copyErr != nil {
			return copyErr
		}
		if n != int64(file.UncompressedSize64) {
			return errors.New("Composer ZIP expanded size differs from declared entry")
		}
		candidate := installEntry{Kind: "file", SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Mode: fileMode}
		if duplicate && existing != candidate {
			return errors.New("duplicate Composer ZIP file has conflicting bytes or mode")
		}
		entries[destination] = candidate
		if inner == "composer.json" {
			manifest = []byte(capture.String())
		}
	}
	if len(manifest) > 0 {
		object, err := parallax.Decode(strings.NewReader(string(manifest)))
		if err != nil {
			return errors.New("downloaded composer.json is invalid")
		}
		if name, present := object["name"]; present && name != pkg.Name {
			return errors.New("downloaded package name differs from frozen Composer lock")
		}
		if kind, present := object["type"]; present && kind == "composer-plugin" {
			return errors.New("Composer plugins are unsupported")
		}
	}
	return nil
}

func installAddDirectory(value, stage string, entries map[string]installEntry) error {
	if value == "." {
		return nil
	}
	if !installSafeRelative(value) {
		return errors.New("unsafe Composer directory path")
	}
	if existing, ok := entries[value]; ok {
		if existing.Kind != "directory" {
			return errors.New("Composer directory collides with a file")
		}
		return nil
	}
	if err := installAddDirectory(path.Dir(value), stage, entries); err != nil {
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
	entries[value] = installEntry{Kind: "directory", Mode: installDirectoryMode()}
	return nil
}

func installGenerated(stage string, files map[string]GeneratedFile, entries map[string]installEntry) error {
	for name, file := range files {
		if !installSafeRelative(name) || name == composerMarker {
			return errors.New("unsafe generated Composer metadata path")
		}
		if _, present := entries[name]; present {
			return fmt.Errorf("generated Composer metadata collides with package at %s", name)
		}
		if err := installAddDirectory(path.Dir(name), stage, entries); err != nil {
			return err
		}
		mode := installFileMode(file.Executable)
		if stage != "" {
			output, err := os.OpenFile(filepath.Join(stage, filepath.FromSlash(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(mode))
			if err != nil {
				return err
			}
			if err := output.Chmod(os.FileMode(mode)); err != nil {
				output.Close()
				return err
			}
			_, err = output.Write(file.Data)
			if closeErr := output.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		}
		h := sha256.Sum256(file.Data)
		entries[name] = installEntry{Kind: "file", SHA256: hex.EncodeToString(h[:]), Size: int64(len(file.Data)), Mode: mode}
	}
	return nil
}

func installBudget(entries map[string]installEntry) error {
	if len(entries) > installEntryLimit {
		return errors.New("vendor tree exceeds 200000 entries")
	}
	var total int64
	for _, entry := range entries {
		total += entry.Size
		if total > installTreeLimit {
			return errors.New("vendor tree exceeds 4 GiB expanded file content")
		}
	}
	return nil
}

func installVerifyTree(root string, expected map[string]installEntry, complete bool) error {
	verify := func(relative string, want installEntry) error {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("vendor entry missing at %s", relative)
		}
		if uint32(info.Mode().Perm()) != want.Mode {
			return fmt.Errorf("vendor entry mode changed at %s", relative)
		}
		if want.Kind == "directory" {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("vendor directory changed at %s", relative)
			}
			return nil
		}
		if want.Kind != "file" || !info.Mode().IsRegular() || info.Size() != want.Size {
			return fmt.Errorf("vendor file changed at %s", relative)
		}
		h, size, err := installHashFile(context.Background(), filename, want.Size)
		if err != nil || size != want.Size || h != want.SHA256 {
			return fmt.Errorf("vendor checksum changed at %s", relative)
		}
		return nil
	}
	if !complete {
		for relative, want := range expected {
			if err := verify(relative, want); err != nil {
				return err
			}
		}
		return nil
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == composerMarker {
			return nil
		}
		want, present := expected[relative]
		if !present {
			return fmt.Errorf("unmanaged vendor tree entry %s", relative)
		}
		seen[relative] = true
		return verify(relative, want)
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("vendor tree is missing frozen source files")
	}
	return nil
}
