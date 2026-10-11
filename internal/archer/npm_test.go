package archer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func npmFixtureArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	metadata, _ := json.Marshal(map[string]any{"name": name, "version": version, "main": "index.js"})
	for _, file := range []struct {
		name string
		data []byte
	}{{"package/package.json", metadata}, {"package/index.js", []byte("module.exports = 'frozen package';\n")}} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0644, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func npmEngineFixture(t *testing.T) (*Engine, *atomic.Int32) {
	t.Helper()
	payloads := map[string][]byte{"/alpha.tgz": npmFixtureArchive(t, "alpha", "1.0.2"), "/beta.tgz": npmFixtureArchive(t, "beta", "2.0.3"), "/nested.tgz": npmFixtureArchive(t, "nested", "3.0.0"), "/foreign.tgz": npmFixtureArchive(t, "foreign", "1.0.0")}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		payload, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	manifest := map[string]any{"dependencies": map[string]any{"alpha": "^1.0.0"}, "devDependencies": map[string]any{"beta": "~2.0.0"}, "optionalDependencies": map[string]any{"foreign": "1.0.0"}}
	entry := func(name, version string) map[string]any {
		data := payloads["/"+name+".tgz"]
		digest := sha512.Sum512(data)
		return map[string]any{"version": version, "resolved": server.URL + "/" + name + ".tgz", "integrity": "sha512-" + base64.StdEncoding.EncodeToString(digest[:])}
	}
	alpha := entry("alpha", "1.0.2")
	alpha["dependencies"] = map[string]any{"nested": "^3"}
	foreign := entry("foreign", "1.0.0")
	foreign["optional"] = true
	foreign["os"] = []string{"never"}
	lock := map[string]any{"lockfileVersion": 3, "packages": map[string]any{"": manifest, "node_modules/alpha": alpha, "node_modules/beta": entry("beta", "2.0.3"), "node_modules/alpha/node_modules/nested": entry("nested", "3.0.0"), "node_modules/foreign": foreign}}
	p := Project{Schema: "archer.project/v1", Scope: "npm-integration", Requirements: []Requirement{{Capability: "npm:alpha", Delivery: "package", Constraint: "^1.0.0"}}}
	e := engine(t, p)
	for name, value := range map[string]any{"package.json": manifest, "package-lock.json": lock} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.ProjectDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return e, calls
}

func TestNPMSelectionPreservesConstraintsAndReplaysFullTree(t *testing.T) {
	e, calls := npmEngineFixture(t)
	matching := Requirement{Capability: "npm:alpha", Delivery: "package", Constraint: "^1.0.0"}
	different := Requirement{Capability: "npm:alpha", Delivery: "package", Constraint: ">=1"}
	transitive := Requirement{Capability: "npm:nested", Delivery: "package"}
	optional := Requirement{Capability: "npm:foreign", Delivery: "package"}
	plan, decisions, err := e.npmPlan([]Requirement{matching, different, transitive, optional})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Packages) != 3 || len(plan.Skipped) != 1 {
		t.Fatalf("full frozen graph not retained: %v", plan)
	}
	if decisions[npmDecisionKey(matching)].Status != "npm_install" || decisions[npmDecisionKey(different)].Status != "unresolved" || decisions[npmDecisionKey(transitive)].Status != "unresolved" || decisions[npmDecisionKey(optional)].Status != "npm_skipped" {
		t.Fatalf("incorrect selected requirements: %#v", decisions)
	}
	if !strings.Contains(decisions[npmDecisionKey(matching)].Reason, "full frozen npm tree") {
		t.Fatal("full graph effect was hidden")
	}
	if decisions[npmDecisionKey(optional)].Evidence != "" || decisions[npmDecisionKey(optional)].AIEligible {
		t.Fatal("skipped optional package falsely satisfied")
	}
	if calls.Load() != 0 {
		t.Fatal("planning downloaded package bytes")
	}
	if _, err := os.Stat(filepath.Join(e.ProjectDir, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("planning materialized packages")
	}
}

func TestNPMOfflineReplayAndFreshInstalledVerification(t *testing.T) {
	e, calls := npmEngineFixture(t)
	requirements := e.Project.Requirements
	plan, _, err := e.npmPlan(requirements)
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.installNPM(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Installed != 3 || report.Downloaded != 3 || report.Skipped != 1 || calls.Load() != 3 {
		t.Fatalf("wrong full-tree report: %#v requests=%d", report, calls.Load())
	}
	_, decisions, err := e.npmPlan(requirements)
	if err != nil {
		t.Fatal(err)
	}
	if decision := decisions[npmDecisionKey(requirements[0])]; decision.Status != "npm_installed" || decision.Evidence == "" {
		t.Fatalf("verified tree not reused: %#v", decision)
	}
	if err := os.RemoveAll(filepath.Join(e.ProjectDir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	e.Offline = true
	plan, _, err = e.npmPlan(requirements)
	if err != nil {
		t.Fatal(err)
	}
	report, err = e.installNPM(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Downloaded != 0 || report.Cached != 3 || calls.Load() != 3 {
		t.Fatalf("offline replay used network: %#v requests=%d", report, calls.Load())
	}
}

func TestNPMFailsClosedOnSourceInstalledAndUnmanagedMutation(t *testing.T) {
	for _, mutation := range []string{"source", "installed", "unmanaged", "plan"} {
		t.Run(mutation, func(t *testing.T) {
			e, calls := npmEngineFixture(t)
			plan, _, err := e.npmPlan(e.Project.Requirements)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "installed" {
				if _, err := e.installNPM(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(e.ProjectDir, "node_modules", "alpha", "index.js"), []byte("tampered"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "source":
				file := filepath.Join(e.ProjectDir, "package-lock.json")
				data, _ := os.ReadFile(file)
				if err := os.WriteFile(file, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "unmanaged":
				if err := os.Mkdir(filepath.Join(e.ProjectDir, "node_modules"), 0755); err != nil {
					t.Fatal(err)
				}
			case "plan":
				plan.Packages[0].URL = "https://registry.npmjs.org/changed.tgz"
			}
			before := calls.Load()
			if _, err := e.installNPM(context.Background(), plan); err == nil {
				t.Fatal("unsafe or changed tree accepted")
			}
			if calls.Load() != before {
				t.Fatal("unsafe tree triggered a download")
			}
			if mutation == "installed" || mutation == "unmanaged" {
				if _, _, err := e.npmPlan(e.Project.Requirements); err == nil {
					t.Fatal("planning accepted damaged/unmanaged tree")
				}
			}
		})
	}
}

func TestNPMColdOfflineAndUnsupportedSelectionsDoNotInstall(t *testing.T) {
	e, calls := npmEngineFixture(t)
	for _, request := range []Requirement{{Capability: "npm:unknown"}, {Capability: "npm:alpha", Delivery: "service"}, {Capability: "npm:foreign"}} {
		plan, decisions, err := e.npmPlan([]Requirement{request})
		if err != nil {
			t.Fatal(err)
		}
		if plan != nil || decisions[npmDecisionKey(request)].Status == "npm_installed" {
			t.Fatal("unsupported-only selection installed a graph")
		}
	}
	plan, _, err := e.npmPlan(e.Project.Requirements)
	if err != nil {
		t.Fatal(err)
	}
	e.Offline = true
	if _, err := e.installNPM(context.Background(), plan); err == nil {
		t.Fatal("cold offline tree reported installed")
	}
	if calls.Load() != 0 {
		t.Fatal("cold offline selection used network")
	}
	if _, err := os.Stat(filepath.Join(e.ProjectDir, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("failed offline install published a partial tree")
	}
}

func TestNPMRemotePlanningNeverClaimsInstalledProof(t *testing.T) {
	e, _ := npmEngineFixture(t)
	plan, _, err := e.npmPlan(e.Project.Requirements)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.installNPM(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	e.SkipLocalVerification = true
	_, decisions, err := e.npmPlan(e.Project.Requirements)
	if err != nil {
		t.Fatal(err)
	}
	if decision := decisions[npmDecisionKey(e.Project.Requirements[0])]; decision.Status != "npm_install" || decision.Evidence != "" {
		t.Fatal("remote planning claimed worker's installed state")
	}
}
