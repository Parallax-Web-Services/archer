package runtimes

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Parallax-Web-Services/archer/internal/store"
)

// A copied native test executable is a standalone fixture. No shell, compiler,
// PHP or Node installation is required to run these acceptance checks.
func init() {
	name := filepath.Base(os.Args[0])
	if strings.HasPrefix(name, "runtime-fixture-v") {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "runtime-fixture-v"), ".exe")
		if len(os.Args) > 1 && os.Args[1] == "--version" {
			if os.Getenv("NODE_OPTIONS") != "" {
				os.Exit(7)
			}
			fmt.Print("v" + version)
		} else if len(os.Args) == 4 && os.Args[1] == "-n" && os.Args[2] == "-r" {
			fmt.Print(version)
		} else {
			os.Exit(8)
		}
		mutation := os.Getenv("ARCHER_RUNTIME_FIXTURE_MUTATE")
		root := os.Getenv("ARCHER_PHP_ROOT")
		if mutation == "ini" || (mutation == "published_ini" && root != "" && !strings.HasPrefix(filepath.Base(root), ".runtime-stage-")) {
			if err := os.WriteFile(os.Getenv("PHPRC"), []byte("probe mutation"), 0644); err != nil {
				os.Exit(9)
			}
		}
		os.Exit(0)
	}
}
func fixture(t *testing.T, version string) (string, []byte) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	name := "runtime-fixture-v" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, data, 0755); err != nil {
		t.Fatal(err)
	}
	return file, data
}

// appendExecutable changes an executable that a probe may have just run. It
// replaces the file rather than writing in place: Linux can briefly refuse
// writes to a recently executed image, and Windows scanners can hold it open.
func appendExecutable(t *testing.T, path string, trailer []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".changed"
	if err := os.WriteFile(replacement, append(data, trailer...), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = os.Rename(replacement, path)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
}
func rawSpec(t *testing.T, kind string) Spec {
	t.Helper()
	file, _ := fixture(t, "1.2.3")
	digest, err := hashRegular(file, store.MaxArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	return Spec{Version: "1.2.3", Binary: filepath.Join(t.TempDir(), "absent"), Artifacts: []Bundle{{Platform: runtime.GOOS + "/" + runtime.GOARCH, Format: "raw", Executable: "bin/" + filepath.Base(file), Artifact: store.Artifact{URL: file, SHA256: digest}}}}
}
func options(t *testing.T) Options {
	return Options{Home: filepath.Join(t.TempDir(), "archer"), ScopeKey: "project-a", ProjectDir: t.TempDir()}
}
func installFixture(t *testing.T, kind string) (Plan, Spec, Options) {
	t.Helper()
	spec := rawSpec(t, kind)
	opts := options(t)
	p, err := Inspect(context.Background(), kind, spec, opts)
	if err != nil || p.Status != "install" {
		t.Fatalf("inspect = %#v, %v", p, err)
	}
	p, err = Install(context.Background(), p, opts)
	if err != nil || p.Status != "managed" {
		t.Fatalf("install = %#v, %v", p, err)
	}
	return p, spec, opts
}
func TestLocalRuntimeExactVersionFreshEvidenceAndExclusion(t *testing.T) {
	spec := rawSpec(t, "node")
	file, _ := fixture(t, "1.2.3")
	spec.Binary = file
	opts := options(t)
	t.Setenv("NODE_OPTIONS", "--invalid-host-options")
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || p.Status != "local" {
		t.Fatalf("local = %#v, %v", p, err)
	}
	if _, err := os.Stat(opts.Home); !os.IsNotExist(err) {
		t.Fatal("inspection wrote runtime home")
	}
	opts.ExcludeLocal = true
	excluded, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || excluded.Status != "install" || excluded.ID != p.ID {
		t.Fatalf("excluded = %#v, %v", excluded, err)
	}
	opts.ExcludeLocal = false
	appendExecutable(t, file, []byte("changed"))
	if _, err := Install(context.Background(), p, opts); err == nil {
		t.Fatal("accepted changed frozen local evidence")
	}
}
func TestIncompatibleLocalFallsBackAndSkippedInspectionCannotClaimReady(t *testing.T) {
	spec := rawSpec(t, "node")
	file, _ := fixture(t, "2.0.0")
	spec.Binary = file
	opts := options(t)
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || p.Status != "install" {
		t.Fatalf("fallback = %#v, %v", p, err)
	}
	p, err = Install(context.Background(), p, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.SkipLocalVerification = true
	skipped, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || skipped.Status != "install" {
		t.Fatalf("skip claimed readiness: %#v %v", skipped, err)
	}
}
func TestManagedOfflineReuseAndScopeIsolation(t *testing.T) {
	p, spec, opts := installFixture(t, "node")
	if err := os.Remove(spec.Artifacts[0].Artifact.URL); err != nil {
		t.Fatal(err)
	}
	opts.Offline = true
	reused, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || reused.Status != "managed" || !reused.Cached || reused.Path != p.Path {
		t.Fatalf("reuse = %#v %v", reused, err)
	}
	opts.ScopeKey = "project-b"
	other, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil || other.Status != "install" || other.ID != p.ID || other.Path == p.Path {
		t.Fatalf("scope = %#v %v", other, err)
	}
	other, err = Install(context.Background(), other, opts)
	if err != nil || !other.Cached || other.Path == p.Path {
		t.Fatalf("offline scoped install = %#v %v", other, err)
	}
	fresh := options(t)
	fresh.Offline = true
	absent, err := Inspect(context.Background(), "node", spec, fresh)
	if err != nil || absent.Status != "unresolved" {
		t.Fatalf("offline absent = %#v %v", absent, err)
	}
}
func TestModifiedManagedTreeAndEditedReceiptArePreserved(t *testing.T) {
	p, spec, opts := installFixture(t, "node")
	appendExecutable(t, p.Path, []byte("modified"))
	root := filepath.Dir(filepath.Dir(p.Path))
	receiptFile := filepath.Join(root, receiptName)
	data, err := os.ReadFile(receiptFile)
	if err != nil {
		t.Fatal(err)
	}
	var r receipt
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	for i := range r.Inventory {
		if r.Inventory[i].Path == spec.Artifacts[0].Executable {
			r.Inventory[i].SHA256, _ = hashRegular(p.Path, store.MaxArtifactBytes)
			info, _ := os.Stat(p.Path)
			r.Inventory[i].Size = info.Size()
		}
	}
	data, _ = json.Marshal(r)
	if err := os.WriteFile(receiptFile, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), "node", spec, opts); err == nil {
		t.Fatal("accepted edited receipt as provenance")
	}
	if _, err := Install(context.Background(), p, opts); err == nil {
		t.Fatal("overwrote modified runtime")
	}
	after, err := os.ReadFile(receiptFile)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("modified receipt was not preserved")
	}
}
func TestManagedPHPUsesImmutableDeclaredConfiguration(t *testing.T) {
	p, spec, opts := installFixture(t, "php")
	if p.Environment["PHP_INI_SCAN_DIR"] != "" || p.Environment["PHPRC"] == "" {
		t.Fatalf("PHP environment %#v", p.Environment)
	}
	if p.Environment["ARCHER_PHP_ROOT"] != filepath.Dir(filepath.Dir(p.Path)) {
		t.Fatalf("PHP root environment %#v", p.Environment)
	}
	data, err := os.ReadFile(p.Environment["PHPRC"])
	if err != nil || len(data) != 0 {
		t.Fatalf("empty managed ini: %q %v", data, err)
	}
	if err := os.WriteFile(p.Environment["PHPRC"], []byte("hostile=1"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), "php", spec, opts); err == nil {
		t.Fatal("accepted modified immutable ini")
	}
	local, _ := fixture(t, "1.2.3")
	ini := filepath.Join(opts.ProjectDir, "php.ini")
	if err := os.WriteFile(ini, []byte("extension=example"), 0644); err != nil {
		t.Fatal(err)
	}
	spec.Binary = local
	spec.PHPIni = "php.ini"
	found, err := Inspect(context.Background(), "php", spec, opts)
	if err != nil || found.Status != "local" || found.Environment["PHPRC"] != ini {
		t.Fatalf("local PHP ini = %#v %v", found, err)
	}
	localRoot, err := os.Stat(filepath.Dir(local))
	if err != nil {
		t.Fatal(err)
	}
	selectedRoot, err := os.Stat(found.Environment["ARCHER_PHP_ROOT"])
	if err != nil || !selectedRoot.IsDir() || !os.SameFile(localRoot, selectedRoot) {
		t.Fatalf("local PHP root environment %#v", found.Environment)
	}
}

func TestManagedProbeCannotPublishMutatedStaging(t *testing.T) {
	t.Setenv("ARCHER_RUNTIME_FIXTURE_MUTATE", "ini")
	spec := rawSpec(t, "php")
	opts := options(t)
	p, err := Inspect(context.Background(), "php", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Install(context.Background(), p, opts)
	if err == nil || failed.Status == "managed" || !strings.Contains(err.Error(), "readiness probe") {
		t.Fatalf("mutating install = %#v %v", failed, err)
	}
	root, _ := managedRoot(p, opts)
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("mutated staged tree was published: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".runtime-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("mutated stages left behind %v %v", stages, err)
	}
}

func TestManagedProbeCannotClaimMutatedReuseReady(t *testing.T) {
	for _, operation := range []string{"inspect", "install"} {
		t.Run(operation, func(t *testing.T) {
			p, spec, opts := installFixture(t, "php")
			receiptFile := filepath.Join(p.Environment["ARCHER_PHP_ROOT"], receiptName)
			original, err := os.ReadFile(receiptFile)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARCHER_RUNTIME_FIXTURE_MUTATE", "ini")
			var failed Plan
			if operation == "inspect" {
				failed, err = Inspect(context.Background(), "php", spec, opts)
			} else {
				failed, err = Install(context.Background(), p, opts)
				if err == nil {
					t.Fatal("mutating existing Install returned success")
				}
			}
			if failed.Status == "managed" || !strings.Contains(failed.Reason, "readiness probe") {
				t.Fatalf("mutating reuse = %#v %v", failed, err)
			}
			mutated, readErr := os.ReadFile(p.Environment["PHPRC"])
			if readErr != nil || string(mutated) != "probe mutation" {
				t.Fatalf("modified existing tree was repaired or removed: %q %v", mutated, readErr)
			}
			preserved, readErr := os.ReadFile(receiptFile)
			if readErr != nil || !bytes.Equal(preserved, original) {
				t.Fatal("existing receipt replaced after mutating probe")
			}
		})
	}
}

func TestPublishedProbeCannotClaimMutatedTreeReady(t *testing.T) {
	t.Setenv("ARCHER_RUNTIME_FIXTURE_MUTATE", "published_ini")
	spec := rawSpec(t, "php")
	opts := options(t)
	p, err := Inspect(context.Background(), "php", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Install(context.Background(), p, opts)
	if err == nil || failed.Status == "managed" || !strings.Contains(err.Error(), "readiness probe") {
		t.Fatalf("postpublication mutation = %#v %v", failed, err)
	}
	root, _ := managedRoot(p, opts)
	data, readErr := os.ReadFile(filepath.Join(root, emptyIni))
	if readErr != nil || string(data) != "probe mutation" {
		t.Fatalf("postpublication failure should preserve changed published tree: %q %v", data, readErr)
	}
}

func TestVerifySelectionRechecksLocalAndManagedEvidence(t *testing.T) {
	local, _ := fixture(t, "1.2.3")
	localOpts := options(t)
	localSpec := Spec{Version: "1.2.3", Binary: local}
	p, err := Inspect(context.Background(), "node", localSpec, localOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySelection(context.Background(), p, localOpts); err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Environment = map[string]string{"NODE_OPTIONS": "--eval=unsafe"}
	if err := VerifySelection(context.Background(), changed, localOpts); err == nil {
		t.Fatal("accepted changed frozen runtime environment")
	}
	appendExecutable(t, local, []byte("changed"))
	if err := VerifySelection(context.Background(), p, localOpts); err == nil {
		t.Fatal("accepted changed local executable evidence")
	}
	managed, spec, opts := installFixture(t, "php")
	if err := VerifySelection(context.Background(), managed, opts); err != nil {
		t.Fatal(err)
	}
	// Full PHP configuration inspection runs separately in the engine. Its
	// changes must invalidate this frozen readiness evidence before package use.
	if err := os.WriteFile(managed.Environment["PHPRC"], []byte("extension=changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySelection(context.Background(), managed, opts); err == nil {
		t.Fatal("accepted managed mutation after an external PHP probe")
	}
	spec.Binary = local
	unready := basePlan("php", spec)
	if err := VerifySelection(context.Background(), unready, opts); err == nil {
		t.Fatal("accepted unresolved selection")
	}
}

func TestVerifyManagedSelectionDoesNotSwitchToNewLocalCandidate(t *testing.T) {
	local, _ := fixture(t, "1.2.3")
	spec := rawSpec(t, "node")
	spec.Binary = local
	opts := options(t)
	opts.ExcludeLocal = true
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err = Install(context.Background(), p, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.ExcludeLocal = false
	if err := VerifySelection(context.Background(), p, opts); err != nil {
		t.Fatalf("managed selection switched to local candidate: %v", err)
	}
}

func TestManagedReceiptRequiresArchiveAndCompleteInventory(t *testing.T) {
	p, spec, opts := installFixture(t, "node")
	root, _ := managedRoot(p, opts)
	unexpected := filepath.Join(root, "extra")
	if err := os.WriteFile(unexpected, []byte("not-in-archive"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), "node", spec, opts); err == nil {
		t.Fatal("accepted extra managed path")
	}
	if err := os.Remove(unexpected); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p.Path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), "node", spec, opts); err == nil {
			t.Fatal("accepted altered executable mode")
		}
		if err := os.Chmod(p.Path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	object, ok, err := store.New(filepath.Join(opts.Home, "cache")).Has(spec.Artifacts[0].Artifact.SHA256)
	if err != nil || !ok {
		t.Fatalf("cache %v %v", ok, err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), "node", spec, opts); err == nil {
		t.Fatal("accepted receipt without verified provenance archive")
	}
}

func TestZIPWindowsExecutableAndBundledPHPIni(t *testing.T) {
	_, native := fixture(t, "1.2.3")
	name := "runtime-fixture-v1.2.3"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, item := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"runtime/bin/" + name, native, 0755}, {"runtime/config/php.ini", []byte(`extension_dir="${ARCHER_PHP_ROOT}/ext"`), 0644}} {
		mode := item.mode
		if runtime.GOOS == "windows" {
			mode = 0644
		}
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(mode)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "runtime.zip")
	if err := os.WriteFile(source, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(buf.Bytes())
	b := Bundle{Platform: runtime.GOOS + "/" + runtime.GOARCH, Format: "zip", Executable: "bin/" + name, StripPrefix: "runtime", Artifact: store.Artifact{URL: source, SHA256: hex.EncodeToString(digest[:])}}
	spec := Spec{Version: "1.2.3", Binary: "archer-absent-native-runtime", PHPIni: "config/php.ini", Artifacts: []Bundle{b}}
	opts := options(t)
	p, err := Inspect(context.Background(), "php", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err = Install(context.Background(), p, opts)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := managedRoot(p, opts)
	if p.Environment["PHPRC"] != filepath.Join(root, "config", "php.ini") || p.Environment["ARCHER_PHP_ROOT"] != root {
		t.Fatalf("bundled PHP environment %#v", p.Environment)
	}
	if _, err := Inspect(context.Background(), "php", spec, opts); err != nil {
		t.Fatal(err)
	}
}
func TestFrozenPlanAndUnmanagedTargetRefused(t *testing.T) {
	spec := rawSpec(t, "node")
	opts := options(t)
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Spec.Version = "9.9.9"
	if _, err := Install(context.Background(), changed, opts); err == nil {
		t.Fatal("accepted changed frozen specification")
	}
	root, _ := managedRoot(p, opts)
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "user-file")
	if err := os.WriteFile(marker, []byte("preserve"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), p, opts); err == nil {
		t.Fatal("accepted unmanaged runtime directory")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "preserve" {
		t.Fatal("unmanaged path changed")
	}
}
func TestConcurrentInstallPublishesOneCompleteTree(t *testing.T) {
	spec := rawSpec(t, "node")
	opts := options(t)
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready, err := Install(context.Background(), p, opts)
			if err == nil && ready.Status != "managed" {
				err = fmt.Errorf("status %s", ready.Status)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	root, _ := managedRoot(p, opts)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".runtime-stage-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("left incomplete stages %v %v", matches, err)
	}
}
func TestCancelledInspectAndInstall(t *testing.T) {
	spec := rawSpec(t, "node")
	opts := options(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, "node", spec, opts); err == nil {
		t.Fatal("ignored cancelled inspect")
	}
	p := basePlan("node", spec)
	p.Status = "install"
	root, _ := managedRoot(p, opts)
	p.Path = filepath.Join(root, filepath.FromSlash(spec.Artifacts[0].Executable))
	p.BinDir = filepath.Dir(p.Path)
	if _, err := Install(ctx, p, opts); err == nil {
		t.Fatal("ignored cancelled install")
	}
}
func TestValidationRejectsUnpinnedAndUnsafeSpecs(t *testing.T) {
	for _, version := range []string{"latest", "^1.2.3", "1.2", "01.2.3", "1.2.3-alpha.1"} {
		if err := Validate("node", Spec{Version: version}); err == nil {
			t.Fatalf("accepted %s", version)
		}
	}
	spec := rawSpec(t, "node")
	for _, name := range []string{"../node", "/node", "a\\node", "a:node", "a/CON", "a//node", "a/./node", "a/node."} {
		spec.Artifacts[0].Executable = name
		if err := Validate("node", spec); err == nil {
			t.Fatalf("accepted path %s", name)
		}
	}
}

type archiveFixture struct {
	name   string
	kind   byte
	target string
	body   []byte
	size   int64
}

func tarBundle(t *testing.T, entries []archiveFixture) (string, Bundle) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		mode := int64(0644)
		if e.kind == tar.TypeReg {
			mode = 0755
		}
		if e.kind != tar.TypeReg {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.kind, Linkname: e.target, Mode: mode, Size: size}); err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
		if e.size > int64(len(e.body)) {
			break
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	file := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(file, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(buf.Bytes())
	return file, Bundle{Platform: runtime.GOOS + "/" + runtime.GOARCH, Format: "tar.gz", Executable: "bin/runtime-fixture-v1.2.3", StripPrefix: "runtime", Artifact: store.Artifact{URL: file, SHA256: hex.EncodeToString(h[:])}}
}
func TestTarRejectsTraversalConflictsLinksAndExpansion(t *testing.T) {
	cases := []struct {
		name    string
		entries []archiveFixture
	}{
		{"traversal", []archiveFixture{{name: "runtime/../escape", kind: tar.TypeReg, body: []byte("x")}}},
		{"outside-prefix", []archiveFixture{{name: "other/file", kind: tar.TypeReg, body: []byte("x")}}},
		{"duplicate", []archiveFixture{{name: "runtime/a", kind: tar.TypeReg}, {name: "runtime/a", kind: tar.TypeReg}}},
		{"symlink-parent", []archiveFixture{{name: "runtime/bin/node", kind: tar.TypeReg}, {name: "runtime/bin", kind: tar.TypeSymlink, target: "lib"}}},
		{"escaping-link", []archiveFixture{{name: "runtime/link", kind: tar.TypeSymlink, target: "../escape"}}},
		{"hardlink", []archiveFixture{{name: "runtime/link", kind: tar.TypeLink, target: "file"}}},
		{"device", []archiveFixture{{name: "runtime/device", kind: tar.TypeChar}}},
		{"case-collision", []archiveFixture{{name: "runtime/A", kind: tar.TypeReg}, {name: "runtime/a", kind: tar.TypeReg}}},
		{"expansion", []archiveFixture{{name: "runtime/huge", kind: tar.TypeReg, size: maxTreeBytes + 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file, b := tarBundle(t, c.entries)
			stage := t.TempDir()
			if _, err := bundleInventory(context.Background(), file, b, stage); err == nil {
				t.Fatal("accepted malformed archive")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(stage), "escape")); err == nil {
				t.Fatal("archive escaped stage")
			}
		})
	}
}
func TestOfficialNodeStyleTarPreservesSafeRelativeSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("official Windows Node ZIPs use regular .cmd files, not symlinks")
	}
	_, data := fixture(t, "1.2.3")
	_, b := tarBundle(t, []archiveFixture{{name: "runtime/bin/runtime-fixture-v1.2.3", kind: tar.TypeReg, body: data}, {name: "runtime/lib/npm/npm-cli.js", kind: tar.TypeReg, body: []byte("console.log('npm')")}, {name: "runtime/bin/npm", kind: tar.TypeSymlink, target: "../lib/npm/npm-cli.js"}})
	spec := Spec{Version: "1.2.3", Binary: "archer-absent-native-runtime", Artifacts: []Bundle{b}}
	opts := options(t)
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err = Install(context.Background(), p, opts)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(p.BinDir, "npm"))
	if err != nil || target != "../lib/npm/npm-cli.js" {
		t.Fatalf("npm link %q %v", target, err)
	}
	if _, err := Inspect(context.Background(), "node", spec, opts); err != nil {
		t.Fatal(err)
	}
}

func TestTarRejectsSymlinkTraversalThroughEarlierSymlink(t *testing.T) {
	_, native := fixture(t, "1.2.3")
	name := "runtime-fixture-v1.2.3"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	file, b := tarBundle(t, []archiveFixture{{name: "runtime/bin/" + name, kind: tar.TypeReg, body: native}, {name: "runtime/safe", kind: tar.TypeDir}, {name: "runtime/escape", kind: tar.TypeReg, body: []byte("inside")}, {name: "runtime/deep/link1", kind: tar.TypeSymlink, target: "../safe"}, {name: "runtime/link2", kind: tar.TypeSymlink, target: "deep/link1/../../escape"}})
	b.Executable = "bin/" + name
	if _, err := bundleInventory(context.Background(), file, b, ""); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("accepted symlink traversal through earlier link: %v", err)
	}
	stage := t.TempDir()
	if _, err := bundleInventory(context.Background(), file, b, stage); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("published unsafe symlink chain: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stage, "link2")); !os.IsNotExist(err) {
		t.Fatal("unsafe link was created")
	}
}
func TestZipRejectsTraversalAndRawCorruptDigest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writer, err := zw.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("bad"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(file, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(buf.Bytes())
	b := Bundle{Format: "zip", Executable: "node", Artifact: store.Artifact{SHA256: hex.EncodeToString(h[:])}}
	if _, err := bundleInventory(context.Background(), file, b, t.TempDir()); err == nil {
		t.Fatal("accepted zip traversal")
	}
	spec := rawSpec(t, "node")
	spec.Artifacts[0].Artifact.SHA256 = strings.Repeat("0", 64)
	opts := options(t)
	p, err := Inspect(context.Background(), "node", spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), p, opts); err == nil {
		t.Fatal("accepted wrong artifact digest")
	}
}
func TestProbeHasBoundedOutputAndTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	file, _ := fixture(t, "1.2.3")
	if err := probe(ctx, "node", file, "1.2.3", nil); err == nil {
		t.Fatal("ignored probe deadline")
	}
	var b limitedBuffer
	if _, err := b.Write(make([]byte, 4097)); err == nil {
		t.Fatal("accepted unbounded probe output")
	}
}
