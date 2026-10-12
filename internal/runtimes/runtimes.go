// Package runtimes installs explicitly pinned runtimes without system installers.
// Content cache hits never establish runtime readiness.
package runtimes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Parallax-Web-Services/archer/internal/store"
)

type Spec struct {
	Version   string   `json:"version"`
	Binary    string   `json:"binary,omitempty"`
	Artifacts []Bundle `json:"artifacts,omitempty"`
	PHPIni    string   `json:"php_ini,omitempty"`
}
type Bundle struct {
	Platform    string         `json:"platform"`
	Format      string         `json:"format"`
	Artifact    store.Artifact `json:"artifact"`
	Executable  string         `json:"executable"`
	StripPrefix string         `json:"strip_prefix,omitempty"`
}
type Options struct {
	Home, ScopeKey, ProjectDir     string
	Offline, SkipLocalVerification bool
	ExcludeLocal                   bool
}
type Plan struct {
	Schema      string            `json:"schema"`
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Version     string            `json:"version"`
	Platform    string            `json:"platform"`
	Status      string            `json:"status"`
	Path        string            `json:"path,omitempty"`
	BinDir      string            `json:"bin_dir,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Cached      bool              `json:"cached"`
	Spec        Spec              `json:"spec"`
	Environment map[string]string `json:"environment,omitempty"`
	Evidence    string            `json:"evidence,omitempty"`
}

var exactVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var scopeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func Validate(kind string, spec Spec) error {
	if kind != "node" && kind != "php" {
		return errors.New("runtime kind must be node or php")
	}
	if !exactVersion.MatchString(spec.Version) {
		return errors.New("runtime version must be an exact stable x.y.z version")
	}
	if strings.ContainsAny(spec.Binary, "\x00\r\n") {
		return errors.New("invalid runtime binary")
	}
	if kind != "php" && spec.PHPIni != "" {
		return errors.New("php_ini is only valid for PHP")
	}
	seen := map[string]bool{}
	for _, b := range spec.Artifacts {
		parts := strings.Split(b.Platform, "/")
		if len(parts) != 2 || (parts[0] != "linux" && parts[0] != "darwin" && parts[0] != "windows") || (parts[1] != "amd64" && parts[1] != "arm64") {
			return fmt.Errorf("unsupported runtime platform %q", b.Platform)
		}
		if seen[b.Platform] {
			return fmt.Errorf("duplicate runtime platform %s", b.Platform)
		}
		seen[b.Platform] = true
		if b.Format != "raw" && b.Format != "tar.gz" && b.Format != "zip" {
			return fmt.Errorf("unsupported runtime bundle format %q", b.Format)
		}
		if _, err := safeRelative(b.Executable); err != nil {
			return fmt.Errorf("runtime executable: %w", err)
		}
		if b.StripPrefix != "" {
			if _, err := safeRelative(b.StripPrefix); err != nil {
				return fmt.Errorf("runtime strip prefix: %w", err)
			}
		}
		if b.Format == "raw" && b.StripPrefix != "" {
			return errors.New("raw runtime bundles cannot strip a prefix")
		}
		if len(b.Artifact.SHA256) != 64 {
			return errors.New("runtime artifact requires a SHA256 digest")
		}
		if _, err := hex.DecodeString(b.Artifact.SHA256); err != nil {
			return errors.New("runtime artifact requires a SHA256 digest")
		}
		if strings.TrimSpace(b.Artifact.URL) == "" {
			return errors.New("runtime artifact requires a source URL or absolute file")
		}
	}
	return nil
}
func identity(kind string, spec Spec, platform string) string {
	data, _ := json.Marshal(struct {
		Schema, Kind, Platform, Policy string
		Spec                           Spec
	}{"archer.runtime.v1", kind, platform, "bounded-tree-v1", spec})
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func basePlan(kind string, spec Spec) Plan {
	p := Plan{Schema: "archer.runtime.v1", Kind: kind, Version: spec.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Status: "unresolved", Spec: spec}
	p.ID = identity(kind, spec, p.Platform)
	return p
}
func selected(p Plan) (Bundle, bool) {
	for _, b := range p.Spec.Artifacts {
		if b.Platform == p.Platform {
			return b, true
		}
	}
	return Bundle{}, false
}
func managedRoot(p Plan, opts Options) (string, error) {
	if opts.Home == "" || !filepath.IsAbs(opts.Home) {
		return "", errors.New("runtime home must be absolute")
	}
	if !scopeName.MatchString(opts.ScopeKey) {
		return "", errors.New("runtime scope key must be a safe nonempty segment")
	}
	return filepath.Join(opts.Home, "projects", opts.ScopeKey, "runtimes", p.Kind, p.ID), nil
}

// Inspect performs no writes or downloads and freshly verifies readiness.
func Inspect(ctx context.Context, kind string, spec Spec, opts Options) (Plan, error) {
	if err := Validate(kind, spec); err != nil {
		return Plan{}, err
	}
	p := basePlan(kind, spec)
	if err := ctx.Err(); err != nil {
		return p, err
	}
	if !opts.SkipLocalVerification && !opts.ExcludeLocal {
		if local, err := localPlan(ctx, p, opts); err == nil {
			return local, nil
		} else {
			p.Reason = err.Error()
		}
	} else if opts.SkipLocalVerification {
		p.Reason = "local runtime verification was skipped"
	} else {
		p.Reason = "local runtime candidate was excluded"
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	b, ok := selected(p)
	if !ok {
		p.Reason += "; no pinned artifact for " + p.Platform
		return p, nil
	}
	root, err := managedRoot(p, opts)
	if err != nil {
		return p, err
	}
	if err := checkDirectories(filepath.Dir(root), false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, err
	}
	object, cached, err := store.New(filepath.Join(opts.Home, "cache")).Has(b.Artifact.SHA256)
	if err != nil {
		return p, err
	}
	p.Cached = cached
	if info, statErr := os.Lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return p, fmt.Errorf("refusing unmanaged runtime path %s", root)
		}
		if !cached {
			return p, errors.New("managed runtime provenance archive is missing from the verified cache")
		}
		expected, err := bundleInventory(ctx, object, b, "")
		if err != nil {
			return p, err
		}
		expected, err = withPHPIni(p, expected)
		if err != nil {
			return p, err
		}
		if err := verifyManaged(root, p, b, expected); err != nil {
			return p, err
		}
		if opts.SkipLocalVerification {
			p.Status = "install"
			p.Path = filepath.Join(root, filepath.FromSlash(b.Executable))
			p.BinDir = filepath.Dir(p.Path)
			p.Reason = "managed bytes verified; runtime readiness probe was skipped"
			return p, nil
		}
		return readyManaged(ctx, p, b, root, expected), nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return p, statErr
	}
	p.Status = "install"
	p.Path = filepath.Join(root, filepath.FromSlash(b.Executable))
	p.BinDir = filepath.Dir(p.Path)
	p.Reason = "pinned runtime bundle must be installed and verified"
	if opts.Offline && !cached {
		p.Status = "unresolved"
		p.Reason = "pinned runtime bundle is absent from the offline cache"
	}
	return p, nil
}

// VerifySelection freshly rechecks a frozen ready selection without downloads
// or writes. Managed selections remain managed even when a local candidate is
// now present. It is also suitable after a caller's configuration/extension
// probe, because managed inventory and local executable/config evidence are
// verified anew before the caller uses the selection.
func VerifySelection(ctx context.Context, plan Plan, opts Options) error {
	if plan.Status != "local" && plan.Status != "managed" {
		return errors.New("runtime selection is not ready")
	}
	if plan.Status == "managed" {
		opts.ExcludeLocal = true
	}
	fresh, err := Inspect(ctx, plan.Kind, plan.Spec, opts)
	if err != nil {
		return err
	}
	if fresh.ID != plan.ID || fresh.Status != plan.Status || fresh.Path != plan.Path || fresh.BinDir != plan.BinDir || fresh.Evidence != plan.Evidence || fresh.Version != plan.Version || fresh.Platform != plan.Platform || !maps.Equal(fresh.Environment, plan.Environment) {
		return errors.New("runtime selection changed after planning")
	}
	return nil
}

// Install revalidates the frozen identity and publishes an immutable tree.
func Install(ctx context.Context, plan Plan, opts Options) (Plan, error) {
	if opts.SkipLocalVerification {
		return plan, errors.New("runtime installation requires readiness verification")
	}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if err := Validate(plan.Kind, plan.Spec); err != nil {
		return plan, err
	}
	base := basePlan(plan.Kind, plan.Spec)
	if plan.Schema != base.Schema || plan.ID != base.ID || plan.Version != base.Version || plan.Platform != base.Platform {
		return plan, errors.New("runtime plan identity changed")
	}
	if plan.Status == "local" {
		if opts.SkipLocalVerification || opts.ExcludeLocal {
			return plan, errors.New("local runtime verification is disabled")
		}
		fresh, err := localPlan(ctx, base, opts)
		if err != nil {
			return plan, err
		}
		if fresh.Path != plan.Path || fresh.Evidence != plan.Evidence {
			return plan, errors.New("local runtime changed after planning")
		}
		return fresh, nil
	}
	if plan.Status != "install" && plan.Status != "managed" {
		return plan, errors.New("runtime plan is unresolved")
	}
	b, ok := selected(base)
	if !ok {
		return plan, errors.New("runtime plan has no current-platform artifact")
	}
	root, err := managedRoot(base, opts)
	if err != nil {
		return plan, err
	}
	executable := filepath.Join(root, filepath.FromSlash(b.Executable))
	if plan.Path != executable || plan.BinDir != filepath.Dir(executable) {
		return plan, errors.New("runtime plan path changed")
	}
	if err := checkDirectories(filepath.Dir(root), true); err != nil {
		return plan, err
	}
	unlock, err := acquire(ctx, root+".lock")
	if err != nil {
		return plan, err
	}
	defer unlock()
	cache := store.New(filepath.Join(opts.Home, "cache"))
	var object string
	var cached bool
	if opts.Offline {
		object, cached, err = cache.Has(b.Artifact.SHA256)
		if err == nil && !cached {
			err = errors.New("runtime bundle is absent from offline cache")
		}
	} else {
		object, cached, err = cache.Ensure(ctx, b.Artifact)
	}
	if err != nil {
		return plan, err
	}
	expected, err := bundleInventory(ctx, object, b, "")
	if err != nil {
		return plan, err
	}
	expected, err = withPHPIni(base, expected)
	if err != nil {
		return plan, err
	}
	if _, err := os.Lstat(root); err == nil {
		if err := verifyManaged(root, base, b, expected); err != nil {
			return plan, err
		}
		ready := readyManaged(ctx, base, b, root, expected)
		ready.Cached = cached
		if ready.Status != "managed" {
			return ready, errors.New(ready.Reason)
		}
		return ready, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".runtime-stage-*")
	if err != nil {
		return plan, err
	}
	defer os.RemoveAll(stage)
	actual, err := bundleInventory(ctx, object, b, stage)
	if err != nil {
		return plan, err
	}
	actual, err = withPHPIni(base, actual)
	if err != nil {
		return plan, err
	}
	if base.Kind == "php" && base.Spec.PHPIni == "" {
		if err := os.WriteFile(filepath.Join(stage, emptyIni), nil, 0644); err != nil {
			return plan, err
		}
	}
	if !sameInventory(actual, expected) {
		return plan, errors.New("runtime archive changed during extraction")
	}
	if err := writeReceipt(stage, base, b, actual); err != nil {
		return plan, err
	}
	if err := verifyManaged(stage, base, b, expected); err != nil {
		return plan, err
	}
	probe := readyManaged(ctx, base, b, stage, expected)
	if probe.Status != "managed" {
		return probe, errors.New(probe.Reason)
	}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return plan, errors.New("runtime target appeared during publication; target preserved")
	}
	if err := os.Rename(stage, root); err != nil {
		return plan, fmt.Errorf("publish runtime: %w", err)
	}
	ready := readyManaged(ctx, base, b, root, expected)
	ready.Cached = cached
	if ready.Status != "managed" {
		return ready, errors.New(ready.Reason)
	}
	return ready, nil
}

func readyManaged(ctx context.Context, p Plan, b Bundle, root string, expected []entry) Plan {
	p.Path = filepath.Join(root, filepath.FromSlash(b.Executable))
	p.BinDir = filepath.Dir(p.Path)
	p.Environment = runtimeEnvironment(p, root)
	probeErr := probe(ctx, p.Kind, p.Path, p.Version, p.Environment)
	// A runtime probe executes downloaded code. Its successful output cannot
	// authenticate any bytes or configuration it may have changed while running.
	if err := verifyManaged(root, p, b, expected); err != nil {
		p.Status = "unresolved"
		p.Reason = "runtime tree changed during readiness probe: " + err.Error()
		return p
	}
	if probeErr != nil {
		p.Status = "unresolved"
		p.Reason = probeErr.Error()
		return p
	}
	p.Status = "managed"
	p.Reason = "exact runtime version and pinned managed tree verified"
	p.Cached = true
	p.Evidence = "sha256:" + strings.ToLower(b.Artifact.SHA256)
	return p
}
func runtimeEnvironment(p Plan, root string) map[string]string {
	if p.Kind == "node" {
		return map[string]string{"NODE_OPTIONS": ""}
	}
	ini := p.Spec.PHPIni
	if ini == "" {
		ini = emptyIni
	}
	return map[string]string{"PHPRC": filepath.Join(root, filepath.FromSlash(ini)), "PHP_INI_SCAN_DIR": "", "ARCHER_PHP_ROOT": root}
}
func localPlan(ctx context.Context, p Plan, opts Options) (Plan, error) {
	candidate := p.Spec.Binary
	if candidate == "" {
		candidate = p.Kind
	}
	if !filepath.IsAbs(candidate) && strings.ContainsAny(candidate, "/\\") {
		if _, err := safeRelative(filepath.ToSlash(candidate)); err != nil {
			return p, err
		}
		if !filepath.IsAbs(opts.ProjectDir) {
			return p, errors.New("relative runtime binary requires an absolute project directory")
		}
		candidate = filepath.Join(opts.ProjectDir, candidate)
	}
	resolved, err := exec.LookPath(candidate)
	if err != nil {
		return p, fmt.Errorf("local %s runtime unavailable", p.Kind)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return p, err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return p, err
	}
	hash, err := hashRegular(resolved, store.MaxArtifactBytes)
	if err != nil {
		return p, err
	}
	env := map[string]string{}
	if p.Kind == "node" {
		env["NODE_OPTIONS"] = ""
	}
	iniEvidence := ""
	if p.Kind == "php" && p.Spec.PHPIni != "" {
		ini := p.Spec.PHPIni
		if !filepath.IsAbs(ini) {
			if _, err := safeRelative(filepath.ToSlash(ini)); err != nil {
				return p, err
			}
			if !filepath.IsAbs(opts.ProjectDir) {
				return p, errors.New("relative PHP ini requires an absolute project directory")
			}
			ini = filepath.Join(opts.ProjectDir, ini)
		}
		if err := checkDirectories(filepath.Dir(ini), false); err != nil {
			return p, err
		}
		iniHash, err := hashRegular(ini, 1<<20)
		if err != nil {
			return p, err
		}
		iniEvidence = ini + ":" + iniHash
		env["PHPRC"] = ini
		env["PHP_INI_SCAN_DIR"] = ""
		env["ARCHER_PHP_ROOT"] = filepath.Dir(resolved)
	}
	if err := probe(ctx, p.Kind, resolved, p.Version, env); err != nil {
		return p, err
	}
	after, err := hashRegular(resolved, store.MaxArtifactBytes)
	if err != nil || after != hash {
		return p, errors.New("local runtime changed during verification")
	}
	if iniEvidence != "" {
		current, err := hashRegular(env["PHPRC"], 1<<20)
		if err != nil || env["PHPRC"]+":"+current != iniEvidence {
			return p, errors.New("local PHP ini changed during verification")
		}
	}
	p.Status = "local"
	p.Path = resolved
	p.BinDir = filepath.Dir(resolved)
	p.Environment = env
	p.Reason = "exact local runtime version freshly verified"
	h := sha256.Sum256([]byte(resolved + "\n" + hash + "\n" + iniEvidence))
	p.Evidence = "sha256:" + hex.EncodeToString(h[:])
	return p, nil
}
func probe(ctx context.Context, kind, binary, version string, environment map[string]string) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"--version"}
	if kind == "php" {
		args = []string{"-n", "-r", "echo PHP_VERSION;"}
	}
	cmd := exec.CommandContext(bounded, binary, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = os.Environ()
	for k, v := range environment {
		cmd.Env = replaceEnv(cmd.Env, k, v)
	}
	if kind == "node" {
		cmd.Env = replaceEnv(cmd.Env, "NODE_OPTIONS", "")
	}
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s runtime version probe failed: %w", kind, err)
	}
	found := strings.TrimSpace(out.String())
	if kind == "node" {
		found = strings.TrimPrefix(found, "v")
	}
	if found != version {
		return fmt.Errorf("%s runtime requires %s, found %q", kind, version, found)
	}
	return nil
}
func replaceEnv(env []string, key, value string) []string {
	filtered := env[:0]
	for _, item := range env {
		k, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(k, key) {
			filtered = append(filtered, item)
		}
	}
	return append(filtered, key+"="+value)
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("runtime probe output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func hashRegular(path string, limit int64) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return "", fmt.Errorf("runtime file is not a bounded regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.New("runtime file changed while opening")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", errors.New("runtime file exceeds size limit")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("runtime file changed during hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func checkDirectories(dir string, create bool) error {
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := checkDirectories(parent, create); err != nil {
			return err
		}
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.Mkdir(dir, 0755); err == nil || errors.Is(err, os.ErrExist) {
			info, err = os.Lstat(dir)
		}
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime directory is not a real directory: %s", dir)
	}
	return nil
}
func acquire(ctx context.Context, path string) (func(), error) {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := os.Mkdir(path, 0700)
		if err == nil {
			owned, _ := os.Lstat(path)
			return func() {
				now, e := os.Lstat(path)
				if e == nil && owned != nil && now.IsDir() && now.Mode()&os.ModeSymlink == 0 && os.SameFile(owned, now) {
					_ = os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, e := os.Lstat(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe runtime lock")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, errors.New("runtime lock timed out; existing lock preserved")
		case <-tick.C:
		}
	}
}
