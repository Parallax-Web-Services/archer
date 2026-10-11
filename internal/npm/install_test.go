package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fixtureArchiveEntry struct {
	name, data, link string
	kind             byte
	size             int64
}

func registryArchive(t *testing.T, name, version string, extra ...fixtureArchiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	writer := tar.NewWriter(gz)
	manifest, _ := json.Marshal(map[string]any{"name": name, "version": version, "scripts": map[string]string{"test": "this must never execute"}})
	if len(extra) > 0 && extra[0].name == "fixture-package-json" {
		manifest = []byte(extra[0].data)
		extra = extra[1:]
	}
	entries := append([]fixtureArchiveEntry{{name: "package/package.json", data: string(manifest)}, {name: "package/index.js", data: "module.exports = 'frozen';\n"}}, extra...)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(entry.data))
		if entry.size != 0 {
			size = entry.size
		}
		if kind != tar.TypeReg {
			size = 0
		}
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0644, Typeflag: kind, Size: size, Linkname: entry.link}); err != nil {
			t.Fatal(err)
		}
		if entry.data != "" {
			if _, err := writer.Write([]byte(entry.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = writer.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
func sri(bytes []byte) string {
	sum := sha512.Sum512(bytes)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}
func installFixture(t *testing.T, source string, archives map[string][]byte, bins map[string]map[string]string) (string, *Plan) {
	t.Helper()
	root := t.TempDir()
	dependencies := map[string]any{}
	entries := map[string]any{}
	for placement, data := range archives {
		name, _ := placementName(placement)
		if strings.Count(placement, "node_modules/") == 1 {
			dependencies[name] = "1.0.0"
		}
		entry := map[string]any{"version": "1.0.0", "resolved": source + "/" + placement, "integrity": sri(data)}
		if len(bins[placement]) > 0 {
			entry["bin"] = bins[placement]
		}
		entries[placement] = entry
	}
	manifest := map[string]any{"dependencies": dependencies}
	entries[""] = manifest
	lock := map[string]any{"lockfileVersion": 3, "packages": entries}
	for name, value := range map[string]any{"package.json": manifest, "package-lock.json": lock} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Load(root, Platform{OS: runtime.GOOS, Arch: runtime.GOARCH, Libc: libcFixture()})
	if err != nil {
		t.Fatal(err)
	}
	return root, plan
}
func libcFixture() string {
	if runtime.GOOS == "linux" {
		return "glibc"
	}
	return ""
}
func placementName(value string) (string, error) {
	parts := strings.Split(value, "node_modules/")
	return strings.TrimSuffix(parts[len(parts)-1], "/"), nil
}
func registryFixture(t *testing.T, archives map[string][]byte, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		data, ok := archives[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(data)
	}))
}
func assertNoPublish(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("failed install published tree: %v", err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, ".archer-npm-stage-*"))
	if len(paths) > 0 {
		t.Fatal("failed install retained staging directories", paths)
	}
	if _, err := os.Stat(filepath.Join(root, ".archer-npm-install.lock")); !os.IsNotExist(err) {
		t.Fatal("project lock retained")
	}
}

func TestInstallPinnedTreeNestedPlacementAndOfflineReplay(t *testing.T) {
	archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0"), "node_modules/alpha/node_modules/nested": registryArchive(t, "nested", "1.0.0"), "node_modules/@scope/tool": registryArchive(t, "@scope/tool", "1.0.0")}
	var calls atomic.Int32
	server := registryFixture(t, archives, &calls)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, nil)
	home := t.TempDir()
	report, err := Install(context.Background(), root, plan, Options{Home: home, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if report.Installed != 3 || report.Downloaded != 3 || report.Cached != 0 || calls.Load() != 3 {
		t.Fatalf("wrong install counts: %+v calls=%d", report, calls.Load())
	}
	if ok, err := Verify(root, plan); !ok || err != nil {
		t.Fatal("installed tree unverified", ok, err)
	}
	for placement := range archives {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(placement), "index.js")); err != nil {
			t.Fatal("placement lost", placement, err)
		}
	}
	second, err := Install(context.Background(), root, plan, Options{Home: home, Offline: true})
	if err != nil || second.Downloaded != 0 || calls.Load() != 3 {
		t.Fatalf("warm repeat downloaded: %+v %v", second, err)
	}
	owned := filepath.Join(root, "node_modules")
	if filepath.Dir(owned) != root {
		t.Fatal("unsafe fixture cleanup")
	}
	if err := os.RemoveAll(owned); err != nil {
		t.Fatal(err)
	}
	third, err := Install(context.Background(), root, plan, Options{Home: home, Offline: true})
	if err != nil || third.Downloaded != 0 || third.Cached != 3 || calls.Load() != 3 {
		t.Fatalf("offline CAS replay failed: %+v %v", third, err)
	}
}

func TestRedundantDotArchivePathsAndIdenticalDuplicatesAreSafe(t *testing.T) {
	archive := registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/./index.js", data: "module.exports = 'frozen';\n"}, fixtureArchiveEntry{name: "package/./dist/./extra.js", data: "identical publisher layout"}, fixtureArchiveEntry{name: "package/dist/extra.js", data: "identical publisher layout"})
	archives := map[string][]byte{"node_modules/alpha": archive}
	server := registryFixture(t, archives, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, nil)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(root, plan); !ok || err != nil {
		t.Fatal("normalized identical duplicates failed verification", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "node_modules", "alpha", "dist", "extra.js"))
	if err != nil || string(data) != "identical publisher layout" {
		t.Fatal("normalized file placement changed", string(data), err)
	}
}

func TestVerificationPermitsOnlyEmptyViteTemporaryCache(t *testing.T) {
	archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0")}
	server := registryFixture(t, archives, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, nil)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "node_modules", ".vite-temp")
	if err := os.Mkdir(cache, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0755); err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(root, plan); !ok || err != nil {
		t.Fatal("empty Vite cache rejected", ok, err)
	}
	if err := os.WriteFile(filepath.Join(cache, "config.js"), []byte("active bundle"), 0644); err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(root, plan); ok || err == nil {
		t.Fatal("nonempty Vite cache accepted", ok, err)
	}
	if err := os.Remove(filepath.Join(cache, "config.js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("unmanaged file"), 0644); err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(root, plan); ok || err == nil {
		t.Fatal("Vite cache file accepted", ok, err)
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), cache); err != nil {
		t.Skip("symlink fixture unavailable", err)
	}
	if ok, err := Verify(root, plan); ok || err == nil {
		t.Fatal("Vite cache symlink accepted", ok, err)
	}
}

func TestInstallRejectsIntegrityIdentityAndArchiveTricks(t *testing.T) {
	tests := map[string][]byte{
		"traversal":      registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/../../outside", data: "escape"}),
		"absolute":       registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "/package/escape", data: "escape"}),
		"wrong-prefix":   registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "other/file", data: "escape"}),
		"symlink":        registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/link", kind: tar.TypeSymlink, link: "../../outside"}),
		"hardlink":       registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/link", kind: tar.TypeLink, link: "package/index.js"}),
		"device":         registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/device", kind: tar.TypeChar}),
		"duplicate":      registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/index.js", data: "overwrite"}),
		"declared-bomb":  registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/bomb", size: maxExpandedBytes + 1}),
		"bundled-tree":   registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/node_modules/hidden/index.js", data: "undeclared"}),
		"wrong-identity": registryArchive(t, "other", "1.0.0"),
		"wrong-version":  registryArchive(t, "alpha", "9.0.0"),
		"hidden-hook":    registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "fixture-package-json", data: `{"name":"alpha","version":"1.0.0","scripts":{"postinstall":"node postinstall.js"}}`}),
		"native-gyp":     registryArchive(t, "alpha", "1.0.0", fixtureArchiveEntry{name: "package/binding.gyp", data: `{"targets":[]}`}),
	}
	for name, archive := range tests {
		t.Run(name, func(t *testing.T) {
			archives := map[string][]byte{"node_modules/alpha": archive}
			server := registryFixture(t, archives, nil)
			defer server.Close()
			root, plan := installFixture(t, server.URL, archives, nil)
			if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			assertNoPublish(t, root)
		})
	}
	t.Run("SRI", func(t *testing.T) {
		good := registryArchive(t, "alpha", "1.0.0")
		archives := map[string][]byte{"node_modules/alpha": good}
		server := registryFixture(t, map[string][]byte{"node_modules/alpha": []byte("different bytes")}, nil)
		defer server.Close()
		root, plan := installFixture(t, server.URL, archives, nil)
		if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "integrity") {
			t.Fatal("SRI mismatch accepted", err)
		}
		assertNoPublish(t, root)
	})
}

func TestVerifyRejectsTamperUnmanagedAndForgedMarker(t *testing.T) {
	for _, kind := range []string{"content", "extra", "missing", "marker-forged", "marker-removed", "mode"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "mode" && runtime.GOOS == "windows" {
				t.Skip("POSIX mode verification")
			}
			archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0")}
			server := registryFixture(t, archives, nil)
			defer server.Close()
			root, plan := installFixture(t, server.URL, archives, nil)
			if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "node_modules", "alpha", "index.js")
			markerPath := filepath.Join(root, "node_modules", installMarker)
			switch kind {
			case "content":
				if err := os.WriteFile(file, []byte("corrupt"), 0644); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(root, "node_modules", "unexpected"), []byte("unmanaged"), 0644); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "marker-removed":
				if err := os.Remove(markerPath); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(file, 0755); err != nil {
					t.Fatal(err)
				}
			case "marker-forged":
				data := []byte("forged")
				if err := os.WriteFile(file, data, 0644); err != nil {
					t.Fatal(err)
				}
				encoded, _ := os.ReadFile(markerPath)
				var marker treeMarker
				if err := json.Unmarshal(encoded, &marker); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(data)
				entry := marker.Files["alpha/index.js"]
				entry.SHA256 = hex.EncodeToString(hash[:])
				entry.Size = int64(len(data))
				marker.Files["alpha/index.js"] = entry
				encoded, _ = json.Marshal(marker)
				if err := os.WriteFile(markerPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := Verify(root, plan); ok || err == nil {
				t.Fatal("mutated tree or marker was accepted", ok, err)
			}
			if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
				t.Fatal("tampered tree was overwritten")
			}
		})
	}
	t.Run("unmanaged", func(t *testing.T) {
		archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0")}
		server := registryFixture(t, archives, nil)
		defer server.Close()
		root, plan := installFixture(t, server.URL, archives, nil)
		if err := os.Mkdir(filepath.Join(root, "node_modules"), 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
			t.Fatal("unmanaged empty tree replaced")
		}
	})
}

func TestFailedExtractionPublishesNoPartialTreeAndSourceDriftStopsDownloads(t *testing.T) {
	archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0"), "node_modules/beta": registryArchive(t, "wrong-name", "1.0.0")}
	server := registryFixture(t, archives, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, nil)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
		t.Fatal("bad second package accepted")
	}
	assertNoPublish(t, root)
	var calls atomic.Int32
	one := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0")}
	source := registryFixture(t, one, &calls)
	defer source.Close()
	other, frozen := installFixture(t, source.URL, one, nil)
	if err := os.WriteFile(filepath.Join(other, "package.json"), []byte(`{"changed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), other, frozen, Options{Home: t.TempDir()}); err == nil || calls.Load() != 0 {
		t.Fatal("source drift downloaded packages", err, calls.Load())
	}
	assertNoPublish(t, other)
}

func TestConcurrentArtifactRequestsDownloadOnceAndCacheAlwaysRehashes(t *testing.T) {
	archive := registryArchive(t, "alpha", "1.0.0")
	var calls atomic.Int32
	server := registryFixture(t, map[string][]byte{"node_modules/alpha": archive}, &calls)
	defer server.Close()
	root := filepath.Join(t.TempDir(), "cache")
	if err := safeDirectories(root, true); err != nil {
		t.Fatal(err)
	}
	pkg := Package{Name: "alpha", Version: "1.0.0", URL: server.URL + "/node_modules/alpha", Integrity: sri(archive)}
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, _, err := obtain(context.Background(), root, pkg, false); errs <- err }()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent downloads duplicated", calls.Load())
	}
	digest, _ := integrityDigest(pkg.Integrity)
	if err := os.WriteFile(cachePath(root, digest), []byte("corrupt archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := obtain(context.Background(), root, pkg, false); err == nil || calls.Load() != 1 {
		t.Fatal("corrupt cache trusted or silently redownloaded", err, calls.Load())
	}
}

func TestRedirectBoundaryAndCancelledInstallLeaveNoTree(t *testing.T) {
	archive := registryArchive(t, "alpha", "1.0.0")
	var destinationCalls atomic.Int32
	destination := registryFixture(t, map[string][]byte{"node_modules/alpha": archive}, &destinationCalls)
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL+r.URL.Path, 302) }))
	defer source.Close()
	root, plan := installFixture(t, source.URL, map[string][]byte{"node_modules/alpha": archive}, nil)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || destinationCalls.Load() != 0 {
		t.Fatal("cross-origin redirect followed", err, destinationCalls.Load())
	}
	assertNoPublish(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Install(ctx, root, plan, Options{Home: t.TempDir()}); err == nil {
		t.Fatal("cancelled install succeeded")
	}
	assertNoPublish(t, root)
}

func TestBinLinksAreRecordedAndVerifiedWithoutExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("first installer supports POSIX executable links")
	}
	archives := map[string][]byte{"node_modules/@scope/tool": registryArchive(t, "@scope/tool", "1.0.0", fixtureArchiveEntry{name: "package/bin/tool.js", data: "#!/usr/bin/env node\nthrow new Error('must never execute');\n"})}
	server := registryFixture(t, archives, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, map[string]map[string]string{"node_modules/@scope/tool": {"tool": "bin/tool.js"}})
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "node_modules", ".bin", "tool")
	target, err := os.Readlink(link)
	if err != nil || filepath.ToSlash(target) != "../@scope/tool/bin/tool.js" {
		t.Fatal("wrong scoped bin link", target, err)
	}
	if ok, err := Verify(root, plan); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", link); err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(root, plan); ok || err == nil {
		t.Fatal("bin target tamper accepted", ok, err)
	}
}

func TestStandaloneInstallerRejectsForeignHostAndNewShrinkwrap(t *testing.T) {
	archives := map[string][]byte{"node_modules/alpha": registryArchive(t, "alpha", "1.0.0")}
	var calls atomic.Int32
	server := registryFixture(t, archives, &calls)
	defer server.Close()
	root, frozen := installFixture(t, server.URL, archives, nil)
	foreign := Platform{OS: "darwin", Arch: "arm64"}
	if runtime.GOOS == "darwin" {
		foreign = Platform{OS: "linux", Arch: "amd64", Libc: "glibc"}
	}
	plan, err := Load(root, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "execution host") {
		t.Fatal("foreign host accepted", err)
	}
	if err := os.WriteFile(filepath.Join(root, "npm-shrinkwrap.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), root, frozen, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "shrinkwrap") {
		t.Fatal("new override lock accepted", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid host or override lock downloaded archives")
	}
	assertNoPublish(t, root)
}

type zeroFixtureReader struct{}

func (zeroFixtureReader) Read(data []byte) (int, error) { clear(data); return len(data), nil }

func TestExpandedGzipBombIsBoundedAndUnpublished(t *testing.T) {
	// A second concatenated gzip member compresses over 512 MiB into a small
	// response. The tar EOF must not bypass expanded-stream accounting.
	var compressed bytes.Buffer
	compressed.Write(registryArchive(t, "alpha", "1.0.0"))
	writer := gzip.NewWriter(&compressed)
	if _, err := io.CopyN(writer, zeroFixtureReader{}, maxExpandedBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archives := map[string][]byte{"node_modules/alpha": compressed.Bytes()}
	server := registryFixture(t, archives, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, archives, nil)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "expanded size") {
		t.Fatal("compressed expansion bomb accepted", err)
	}
	assertNoPublish(t, root)
}
