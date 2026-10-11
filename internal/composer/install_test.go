package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const fixtureReference = "0123456789abcdef0123456789abcdef01234567"

type installZIPEntry struct {
	Name, Data string
	Mode       os.FileMode
}

func installZIP(t *testing.T, name string, extras ...installZIPEntry) []byte {
	t.Helper()
	var result bytes.Buffer
	writer := zip.NewWriter(&result)
	manifest, _ := json.Marshal(map[string]any{"name": name, "autoload": map[string]any{"psr-4": map[string]string{"Fixture\\": "src/"}}, "scripts": map[string]string{"post-install-cmd": "this must never execute"}})
	entries := append([]installZIPEntry{{Name: "snapshot/composer.json", Data: string(manifest)}, {Name: "snapshot/src/Package.php", Data: "<?php\nnamespace Fixture;\nclass Package {}\n"}}, extras...)
	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0644
		}
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Deflate}
		header.SetMode(mode)
		out, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte(entry.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func installServer(t *testing.T, data []byte, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		_, _ = w.Write(data)
	}))
}

func installFixture(t *testing.T, endpoint, sha1 string) (string, *Plan) {
	t.Helper()
	root := t.TempDir()
	manifest := map[string]any{"name": "fixture/application", "require": map[string]string{"fixture/package": "1.0.0"}}
	manifestData, _ := json.Marshal(manifest)
	contentHash, err := ContentHash(manifestData)
	if err != nil {
		t.Fatal(err)
	}
	lock := map[string]any{"content-hash": contentHash, "packages": []any{map[string]any{
		"name": "fixture/package", "version": "1.0.0", "type": "library",
		"dist":     map[string]string{"type": "zip", "url": endpoint + "/repos/fixture/package/zipball/" + fixtureReference, "reference": fixtureReference, "shasum": sha1},
		"autoload": map[string]any{"psr-4": map[string]string{"Fixture\\": "src/"}},
	}}, "packages-dev": []any{}, "aliases": []any{}, "stability-flags": map[string]any{}, "platform": map[string]any{}, "platform-dev": map[string]any{}}
	lockData, _ := json.Marshal(lock)
	for name, data := range map[string][]byte{"composer.json": manifestData, "composer.lock": lockData} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(endpoint)
	policy := SourcePolicy{Mode: "source-reference", TrustedHosts: []string{u.Hostname()}}
	plan, err := Load(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	return root, plan
}

func assertComposerUnpublished(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, "vendor")); !os.IsNotExist(err) {
		t.Fatalf("vendor published: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".archer-composer-stage-") || entry.Name() == ".archer-composer-install.lock" {
			t.Fatalf("temporary install state remains: %s", entry.Name())
		}
	}
}

func TestInstallColdWarmAndFreshOfflineReplay(t *testing.T) {
	data := installZIP(t, "fixture/package")
	var calls atomic.Int32
	server := installServer(t, data, &calls)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	home := t.TempDir()
	report, err := Install(context.Background(), root, plan, Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if report.Installed != 1 || report.Downloaded != 1 || report.Cached != 0 || calls.Load() != 1 {
		t.Fatalf("cold report: %+v calls=%d", report, calls.Load())
	}
	for _, name := range []string{"autoload.php", "composer/installed.json", "fixture/package/src/Package.php"} {
		if _, err := os.Stat(filepath.Join(report.Path, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(report.Path, composerMarker))
	report, err = Install(context.Background(), root, plan, Options{Home: home, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(report.Path, composerMarker))
	if !bytes.Equal(before, after) || report.Downloaded != 0 || report.Cached != 1 || calls.Load() != 1 {
		t.Fatal("warm verification changed installed state or requested network")
	}
	fresh, freshPlan := installFixture(t, server.URL, "")
	report, err = Install(context.Background(), fresh, freshPlan, Options{Home: home, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cached != 1 || report.Downloaded != 0 || calls.Load() != 1 {
		t.Fatalf("fresh offline report %+v", report)
	}
	if ok, err := Verify(fresh, freshPlan); err != nil || !ok {
		t.Fatalf("verified=%v error=%v", ok, err)
	}
	receiptPath := filepath.Join(installSourceDirectory(filepath.Join(home, "cache", "composer", "sources"), plan.Packages[0]), "receipt.json")
	var receipt installReceipt
	if err := installReadJSON(receiptPath, 16<<10, &receipt); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	if receipt.SHA256 != hex.EncodeToString(h[:]) || receipt.URL != plan.Packages[0].URL || receipt.Reference != fixtureReference || receipt.SHA1 != "" {
		t.Fatalf("unexpected first-seen provenance receipt: %+v", receipt)
	}
}

func TestInstallRefusesMissingOfflineSourceAndUnmanagedVendor(t *testing.T) {
	server := installServer(t, installZIP(t, "fixture/package"), nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir(), Offline: true}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("missing offline archive: %v", err)
	}
	assertComposerUnpublished(t, root)
	if err := os.Mkdir(filepath.Join(root, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "keep"), []byte("user file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
		t.Fatal("unmanaged vendor accepted")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "vendor", "keep")); string(data) != "user file" {
		t.Fatal("unmanaged vendor changed")
	}
}

func TestInstallRejectsFrozenSourceDriftAndPlanMutation(t *testing.T) {
	var calls atomic.Int32
	server := installServer(t, installZIP(t, "fixture/package"), &calls)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	plan.Packages[0].Reference = strings.Repeat("a", 40)
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
		t.Fatal("mutated plan accepted")
	}
	root, plan = installFixture(t, server.URL, "")
	f, err := os.OpenFile(filepath.Join(root, "composer.json"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n")
	_ = f.Close()
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
		t.Fatal("changed manifest accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("source drift was detected after network access")
	}
	assertComposerUnpublished(t, root)
}

func TestInstallRehashesCacheAndDoesNotReplaceCorruption(t *testing.T) {
	var calls atomic.Int32
	server := installServer(t, installZIP(t, "fixture/package"), &calls)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	home := t.TempDir()
	if _, err := Install(context.Background(), root, plan, Options{Home: home}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(installSourceDirectory(filepath.Join(home, "cache", "composer", "sources"), plan.Packages[0]), "archive.zip")
	if err := os.Chmod(archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("corrupt source"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, plan); err == nil {
		t.Fatal("corrupt source cache accepted")
	}
	fresh, freshPlan := installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), fresh, freshPlan, Options{Home: home}); err == nil {
		t.Fatal("corrupt source cache replaced")
	}
	if calls.Load() != 1 {
		t.Fatal("corrupt cache caused redownload")
	}
	assertComposerUnpublished(t, fresh)
}

func TestInstallRejectsForgedInventoryAndGeneratedAutoload(t *testing.T) {
	server := installServer(t, installZIP(t, "fixture/package"), nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(root, "vendor", composerMarker)
	var marker installInventory
	if err := installReadJSON(markerFile, installMarkerLimit, &marker); err != nil {
		t.Fatal(err)
	}
	relative := "fixture/package/src/Package.php"
	file := filepath.Join(root, "vendor", filepath.FromSlash(relative))
	altered := []byte("<?php malicious();\n")
	if err := os.WriteFile(file, altered, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(altered)
	entry := marker.Files[relative]
	entry.SHA256 = hex.EncodeToString(sum[:])
	entry.Size = int64(len(altered))
	marker.Files[relative] = entry
	encoded, _ := json.Marshal(marker)
	if err := os.WriteFile(markerFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, plan); err == nil {
		t.Fatal("mutable marker authorized changed package bytes")
	}
	root, plan = installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "autoload.php"), []byte("<?php malicious();\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, plan); err == nil {
		t.Fatal("changed generated autoload accepted")
	}
}

func TestInstallRejectsUnsafeZIPAndWrongPackageIdentity(t *testing.T) {
	fixtures := map[string][]byte{
		"traversal":          installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/../escape", Data: "bad"}),
		"absolute":           installZIP(t, "fixture/package", installZIPEntry{Name: "/snapshot/escape", Data: "bad"}),
		"backslash":          installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/a\\escape", Data: "bad"}),
		"symlink":            installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/link", Data: "../../escape", Mode: os.ModeSymlink | 0777}),
		"device":             installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/device", Mode: os.ModeDevice | 0600}),
		"two roots":          installZIP(t, "fixture/package", installZIPEntry{Name: "second-root/file", Data: "bad"}),
		"duplicate conflict": installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/src/Package.php", Data: "bad"}),
		"wrong identity":     installZIP(t, "other/package"),
		"malformed manifest": installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/composer.json", Data: "{"}),
	}
	for name, data := range fixtures {
		t.Run(name, func(t *testing.T) {
			server := installServer(t, data, nil)
			defer server.Close()
			root, plan := installFixture(t, server.URL, "")
			if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
			assertComposerUnpublished(t, root)
		})
	}
}

func TestInstallAllowsIdenticalDuplicateZIPFiles(t *testing.T) {
	data := installZIP(t, "fixture/package", installZIPEntry{Name: "snapshot/src/Package.php", Data: "<?php\nnamespace Fixture;\nclass Package {}\n"})
	server := installServer(t, data, nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallVerifiesPublishedLockSHA1(t *testing.T) {
	data := installZIP(t, "fixture/package")
	server := installServer(t, data, nil)
	defer server.Close()
	checksum := sha1.Sum(data)
	root, plan := installFixture(t, server.URL, hex.EncodeToString(checksum[:]))
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	root, plan = installFixture(t, server.URL, strings.Repeat("a", 40))
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "SHA-1") {
		t.Fatalf("wrong SHA-1 accepted: %v", err)
	}
	assertComposerUnpublished(t, root)
}

func TestInstallRejectsArchiveRedirectToDifferentReferenceOrRepository(t *testing.T) {
	for _, target := range []string{"/repos/other/package/zipball/" + fixtureReference, "/repos/fixture/package/zipball/" + strings.Repeat("a", 40)} {
		t.Run(target, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) }))
			defer server.Close()
			root, plan := installFixture(t, server.URL, "")
			if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
			assertComposerUnpublished(t, root)
		})
	}
}

func TestInstallSerializesSameProjectAndHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	server := installServer(t, installZIP(t, "fixture/package"), &calls)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	home := t.TempDir()
	var group sync.WaitGroup
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			report, err := Install(context.Background(), root, plan, Options{Home: home})
			if err == nil && report.Installed != 1 {
				err = fmt.Errorf("unexpected report %+v", report)
			}
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("requests=%d", calls.Load())
	}
	fresh, freshPlan := installFixture(t, server.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Install(ctx, fresh, freshPlan, Options{Home: home}); err == nil {
		t.Fatal("cancelled installation accepted")
	}
	assertComposerUnpublished(t, fresh)
}

func TestSourceReceiptMustRetainFrozenFetchIdentity(t *testing.T) {
	server := installServer(t, installZIP(t, "fixture/package"), nil)
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	home := t.TempDir()
	if _, err := Install(context.Background(), root, plan, Options{Home: home}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(installSourceDirectory(filepath.Join(home, "cache", "composer", "sources"), plan.Packages[0]), "receipt.json")
	var receipt installReceipt
	if err := installReadJSON(file, 16<<10, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.FetchURL = strings.Replace(receipt.FetchURL, "/fixture/package/", "/other/package/", 1)
	data, _ := json.Marshal(receipt)
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, plan); err == nil {
		t.Fatal("changed source receipt fetch identity accepted")
	}
}

func TestInstallRejectsArchiveAndExpandedSizeLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(installArchiveLimit+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	root, plan := installFixture(t, server.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "256 MiB") {
		t.Fatalf("oversized archive accepted: %v", err)
	}
	assertComposerUnpublished(t, root)
	// The central directory advertises an expansion larger than the limit. The
	// installer rejects it before opening or allocating the entry's contents.
	data := installZIP(t, "fixture/package")
	index := bytes.Index(data, []byte{'P', 'K', 1, 2})
	if index < 0 {
		t.Fatal("ZIP central directory missing")
	}
	binary.LittleEndian.PutUint32(data[index+24:index+28], uint32(installExpandedLimit+1))
	server2 := installServer(t, data, nil)
	defer server2.Close()
	root, plan = installFixture(t, server2.URL, "")
	if _, err := Install(context.Background(), root, plan, Options{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "512 MiB") {
		t.Fatalf("oversized ZIP entry accepted: %v", err)
	}
	assertComposerUnpublished(t, root)
}

func TestChecksumPolicySupportsKnownGitHubWithoutHostList(t *testing.T) {
	policy := SourcePolicy{Mode: "checksum"}
	for _, raw := range []string{"https://api.github.com/repos/fixture/package/zipball/" + fixtureReference, "https://codeload.github.com/fixture/package/legacy.zip/" + fixtureReference} {
		if _, err := installArchiveIdentity(raw, fixtureReference, policy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := installArchiveIdentity("https://untrusted.example/repos/fixture/package/zipball/"+fixtureReference, fixtureReference, policy); err == nil {
		t.Fatal("arbitrary archive host accepted")
	}
}
