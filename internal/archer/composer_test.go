package archer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/composer"
)

func composerEngineFixture(t *testing.T) (*Engine, *atomic.Int32) {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, data := range map[string]string{"snapshot/composer.json": `{"name":"fixture/library"}`, "snapshot/src/Example.php": "<?php namespace Fixture; class Example {}"} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(archive.Bytes()) }))
	t.Cleanup(server.Close)
	ref := strings.Repeat("a", 40)
	manifest := []byte(`{"name":"fixture/app","require":{"php":">=8.0","fixture/library":"^1.0"}}`)
	hash, err := composer.ContentHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := json.Marshal(map[string]any{"content-hash": hash, "packages": []any{map[string]any{"name": "fixture/library", "version": "1.2.3", "type": "library", "dist": map[string]any{"type": "zip", "url": server.URL + "/repos/fixture/library/zipball/" + ref, "reference": ref, "shasum": ""}, "autoload": map[string]any{"psr-4": map[string]string{"Fixture\\": "src/"}}}}, "packages-dev": []any{}, "platform": map[string]string{"php": ">=8.0"}, "platform-dev": map[string]any{}, "aliases": []any{}})
	php := os.Getenv("ARCHER_TEST_PHP")
	if php == "" {
		php, _ = exec.LookPath("php")
	}
	p := Project{Schema: "archer.project/v1", Scope: "composer-integration", Requirements: []Requirement{{Capability: "composer:php", Delivery: "package", Constraint: ">=8.0"}, {Capability: "composer:fixture/library", Delivery: "package", Constraint: "^1.0"}}, Composer: &ComposerConfig{PHP: php, SourcePolicy: composer.SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"127.0.0.1"}}}}
	e := engine(t, p)
	for name, data := range map[string][]byte{"composer.json": manifest, "composer.lock": lock} {
		if err := os.WriteFile(filepath.Join(e.ProjectDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return e, calls
}

func TestComposerSourcePolicyAndRuntimeBlockBeforeDownloads(t *testing.T) {
	e, calls := composerEngineFixture(t)
	e.Project.Composer = nil
	plan, err := e.Plan(context.Background(), nil)
	if err != nil || len(plan.Remaining) != 2 || plan.Composer != nil || !strings.Contains(plan.Decisions[0].Reason, "source-reference") {
		t.Fatal(plan, err)
	}
	e.Project.Composer = &ComposerConfig{PHP: "archer-php-that-does-not-exist", SourcePolicy: composer.SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"127.0.0.1"}}}
	plan, err = e.Plan(context.Background(), nil)
	if err != nil || len(plan.Remaining) != 2 || !strings.Contains(plan.Decisions[0].Reason, "runtime readiness") {
		t.Fatal(plan, err)
	}
	if calls.Load() != 0 {
		t.Fatal("blocked plan downloaded")
	}
	entries, err := os.ReadDir(e.Home)
	if err != nil || len(entries) != 0 {
		t.Fatal("blocked planning wrote state", entries, err)
	}
}

func TestComposerEngineReplayOfflineAndTamperedVendor(t *testing.T) {
	e, calls := composerEngineFixture(t)
	if e.Project.Composer.PHP == "" {
		t.Skip("PHP unavailable")
	}
	plan, err := e.Plan(context.Background(), nil)
	if err != nil || plan.Composer == nil || plan.Decisions[0].Status != "composer_platform" || plan.Decisions[1].Status != "composer_install" {
		t.Fatal(plan, err)
	}
	installed, err := e.Install(context.Background(), nil, plan.ID)
	if err != nil || len(installed.Remaining) != 0 || installed.ComposerReport.Installed != 1 || calls.Load() != 1 {
		t.Fatal(installed, err, calls.Load())
	}
	e.Offline = true
	reused, err := e.Install(context.Background(), nil, "")
	if err != nil || reused.ComposerReport.Downloaded != 0 || reused.Decisions[1].Status != "composer_installed" || calls.Load() != 1 {
		t.Fatal(reused, err)
	}
	if err := os.WriteFile(filepath.Join(e.ProjectDir, "vendor/fixture/library/src/Example.php"), []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Plan(context.Background(), nil); err == nil {
		t.Fatal("tampered vendor accepted")
	}
}

func TestComposerSelectionsPreserveDeclaredPolicy(t *testing.T) {
	e, calls := composerEngineFixture(t)
	if e.Project.Composer.PHP == "" {
		t.Skip("PHP unavailable")
	}
	selected := []Requirement{{Capability: "composer:fixture/library", Constraint: "^2"}, {Capability: "composer:fixture/library", Delivery: "service"}, {Capability: "composer:unknown/library"}}
	plan, err := e.Plan(context.Background(), selected)
	if err != nil || len(plan.Remaining) != 3 || plan.Composer != nil || calls.Load() != 0 {
		t.Fatal(plan, err)
	}
	for _, d := range plan.Decisions {
		if d.Status != "unresolved" || d.AIEligible {
			t.Fatal(d)
		}
	}
	e.SkipLocalVerification = true
	plan, err = e.Plan(context.Background(), nil)
	if err != nil || len(plan.Remaining) != 2 || plan.ComposerRuntime != nil || calls.Load() != 0 {
		t.Fatal(plan, err)
	}
}
