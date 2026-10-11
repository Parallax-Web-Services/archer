package archer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/catalog"
	"github.com/Parallax-Web-Services/archer/internal/store"
)

func project() Project {
	return Project{Schema: "archer.project/v1", Scope: "test", Requirements: []Requirement{{Capability: "database"}}}
}
func engine(t *testing.T, p Project) *Engine {
	t.Helper()
	e, err := New(p, t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func artifact(t *testing.T) store.Artifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime")
	b := []byte("verified executable fixture")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return store.Artifact{URL: path, SHA256: digest(b)}
}

func TestPlanRejectsUnboundedOrNonFileSourcesBeforeDiscovery(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint("directory=", directory), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer server.Close()
			p := project()
			p.Services = []catalog.Service{{ID: "relay", Provides: []string{"database"}, Endpoint: server.URL, HealthURL: server.URL}}
			e := engine(t, p)
			path := filepath.Join(e.ProjectDir, "package-lock.json")
			if directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(maxProjectSourceBytes + 1); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.Plan(context.Background(), nil); err == nil {
				t.Fatal("unsafe source accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("discovery ran before source validation")
			}
		})
	}
}
func TestServiceFirstNoInstallOrAI(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	p := project()
	p.Services = []catalog.Service{{ID: "relay", Provides: []string{"database"}, Endpoint: server.URL, HealthURL: server.URL + "/health"}}
	p.Recipes = []Recipe{{ID: "database-binary", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "service" || result.Decisions[0].Discovery != "bound" || len(result.Remaining) != 0 || result.AIRequests != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(e.target(p.Recipes[0])); !os.IsNotExist(err) {
		t.Fatal("service caused an installation")
	}
	if calls.Load() != 1 {
		t.Fatal("service was not verified")
	}
	memory, err := e.Context()
	if err != nil || memory.Bindings["database"] != "relay" {
		t.Fatal("successful service binding not remembered")
	}
}
func TestUnavailableKnownServiceBlocksDuplicateInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	p := project()
	p.Services = []catalog.Service{{ID: "relay", Provides: []string{"database"}, Endpoint: server.URL, HealthURL: server.URL}}
	p.Recipes = []Recipe{{ID: "duplicate", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if d := result.Decisions[0]; d.Status != "unresolved" || d.AIEligible || !strings.Contains(d.Reason, "blocked") {
		t.Fatalf("service fallback violated: %+v", d)
	}
}
func TestArtifactWarmCacheAndFreshInstalledVerification(t *testing.T) {
	p := project()
	p.Requirements[0].Delivery = "local"
	p.Recipes = []Recipe{{ID: "binary", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "installed" {
		t.Fatal(result)
	}
	if err := os.Remove(result.Decisions[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.Recipes[0].Artifact.URL); err != nil {
		t.Fatal(err)
	}
	result, err = e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "install" || !result.Decisions[0].Cached {
		t.Fatal("cached proof falsely established current installation")
	}
	e.Offline = true
	result, err = e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "installed" || !result.Decisions[0].Cached {
		t.Fatal("offline warm cache not reused")
	}
}
func TestLocalProbeIsLiveOnEveryPlan(t *testing.T) {
	p := project()
	if runtime.GOOS == "windows" {
		p.Local = []LocalCapability{{ID: "local", Provides: []string{"database"}, Probe: Probe{Argv: []string{"cmd.exe", "/d", "/c", "echo", "proof"}, Contains: "proof"}}}
	} else {
		p.Local = []LocalCapability{{ID: "local", Provides: []string{"database"}, Probe: Probe{Argv: []string{"sh", "-c", "printf proof"}, Contains: "proof"}}}
	}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "local" || result.Decisions[0].Discovery != "local" {
		t.Fatal(result)
	}
	e.Project.Local[0].Probe.Argv = []string{"archer-does-not-exist-12345"}
	result, err = e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "unresolved" {
		t.Fatal("remembered local result bypassed verification")
	}
}
func TestStaleDiscoveryCannotJustifyInstallationOrAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	p := project()
	p.Registries = []string{server.URL}
	p.Recipes = []Recipe{{ID: "duplicate", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if d := result.Decisions[0]; d.Status != "unresolved" || d.AIEligible {
		t.Fatal("unknown discovery incorrectly fell back")
	}
}
func TestRemoteServiceCannotSubstituteForPackage(t *testing.T) {
	p := project()
	p.Requirements = []Requirement{{Capability: "composer:example/library", Delivery: "package"}}
	p.Services = []catalog.Service{{ID: "remote-library", Provides: []string{"composer:example/library"}}}
	result, err := engine(t, p).Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "unresolved" || result.Decisions[0].AIEligible || result.Decisions[0].Discovery != "not_applicable" {
		t.Fatal(result)
	}
}

func TestPackageOnlyPlanDoesNotRefreshServiceCatalogs(t *testing.T) {
	e, downloads := npmEngineFixture(t)
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { catalogCalls.Add(1) }))
	defer server.Close()
	e.Project.Registries = []string{server.URL}
	plan, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NPM == nil || plan.Decisions[0].Status != "npm_install" {
		t.Fatal(plan)
	}
	if catalogCalls.Load() != 0 || downloads.Load() != 0 {
		t.Fatal("package planning fetched irrelevant services or artifacts")
	}
}
func TestScopeAndSourceChangesInvalidateApproval(t *testing.T) {
	p := project()
	e := engine(t, p)
	first, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ProjectDir, "composer.lock"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("source drift not fingerprinted")
	}
	if _, err := e.Install(context.Background(), nil, first.ID); err == nil {
		t.Fatal("stale approval accepted")
	}
	other, err := New(p, t.TempDir(), e.Home, false)
	if err != nil {
		t.Fatal(err)
	}
	if other.scopeKey == e.scopeKey {
		t.Fatal("project scope collision")
	}
}
func TestPlanDoesNotPersistContext(t *testing.T) {
	e := engine(t, project())
	if _, err := e.Plan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.contextPath()); !os.IsNotExist(err) {
		t.Fatal("planning wrote learned outcomes")
	}
}

func TestPartialInstallReturnsExactRemainingPartition(t *testing.T) {
	p := project()
	p.Requirements = []Requirement{{Capability: "first", Delivery: "local"}, {Capability: "second", Delivery: "local"}}
	first, second := artifact(t), artifact(t)
	second.SHA256 = strings.Repeat("a", 64)
	p.Recipes = []Recipe{{ID: "first", Provides: []string{"first"}, Filename: "first", Artifact: first}, {ID: "second", Provides: []string{"second"}, Filename: "second", Artifact: second}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err == nil {
		t.Fatal("corrupt artifact was accepted")
	}
	if result.Decisions[0].Status != "installed" || len(result.Remaining) != 1 || result.Remaining[0].Capability != "second" {
		t.Fatalf("partial result contradicts observed work: %+v", result)
	}
}

func TestRemoteDescriptorDriftInvalidatesApproval(t *testing.T) {
	variant := "one"
	var source string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "archer.catalog/v1", "services": []catalog.Service{{ID: "same-id", Provides: []string{"database"}, Endpoint: source + "/" + variant, HealthURL: source + "/health", Version: variant}}})
		} else {
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	source = server.URL
	p := project()
	p.Registries = []string{source + "/catalog"}
	p.Services = []catalog.Service{{ID: "same-id", Provides: []string{"database"}, Endpoint: source, HealthURL: source + "/health"}}
	e := engine(t, p)
	first, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Decisions[0].Status != "service" {
		t.Fatal(first)
	}
	variant = "two"
	e.Home = t.TempDir() // fresh isolated catalog store, identical scope/profile revision
	second, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("changed service endpoint/version retained approval ID")
	}
}

func TestConcurrentContextRecordingKeepsEveryOutcome(t *testing.T) {
	e := engine(t, project())
	var workers sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			errs <- e.record(Plan{Decisions: []Decision{{Requirement: Requirement{Capability: fmt.Sprintf("cap-%d", i)}, Status: "local", Provider: "verified"}}})
		}(i)
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	profile, err := e.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Outcomes) != 8 || len(profile.Bindings) != 8 || profile.Revision != 8 {
		t.Fatalf("concurrent observations were lost: %+v", profile)
	}
}

func TestAdvertisedServiceNeedsAnExplicitBinding(t *testing.T) {
	var source string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schema": "archer.catalog/v1", "services": []catalog.Service{{ID: "relay", Provides: []string{"database"}, Endpoint: source, HealthURL: source + "/health"}}})
	}))
	defer server.Close()
	source = server.URL
	p := project()
	p.Registries = []string{source}
	result, err := engine(t, p).Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "unresolved" || result.Decisions[0].AIEligible || result.Decisions[0].Discovery != "fresh_match" {
		t.Fatal("advertisement was mistaken for configured access", result)
	}
	p.Services = []catalog.Service{{ID: "relay", Provides: []string{"database"}, Endpoint: source, HealthURL: source + "/health", TokenEnv: "ARCHER_TEST_MISSING_CREDENTIAL"}}
	t.Setenv("ARCHER_TEST_MISSING_CREDENTIAL", "")
	result, err = engine(t, p).Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions[0].Status != "unresolved" {
		t.Fatal("remote anonymous duplicate bypassed local credentials", result)
	}
}

func TestContextRefusesSymlinkedDirectory(t *testing.T) {
	e := engine(t, project())
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.Home, "contexts")); err != nil {
		t.Skip("symlink creation unavailable", err)
	}
	err := e.record(Plan{Decisions: []Decision{{Requirement: Requirement{Capability: "test"}, Status: "local", Provider: "local"}}})
	if err == nil {
		t.Fatal("context followed a symlink outside its scoped home")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) > 0 {
		t.Fatal("outside directory was modified")
	}
}

func TestNoCatalogDoesNotAuthorizeInstallationOrGeneration(t *testing.T) {
	p := project()
	p.Requirements = append(p.Requirements, Requirement{Capability: "unknown"})
	p.Recipes = []Recipe{{ID: "duplicate", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	result, err := e.Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Catalogs) != 0 || len(result.Remaining) != 2 || result.AIRequests != 0 {
		t.Fatalf("unexpected incomplete discovery result: %+v", result)
	}
	for _, decision := range result.Decisions {
		if decision.Status != "unresolved" || decision.AIEligible || decision.Discovery != "not_configured" || !strings.Contains(decision.Reason, "not configured") {
			t.Fatalf("missing catalogs were mistaken for complete discovery: %+v", decision)
		}
	}
	if _, err := os.Stat(e.target(p.Recipes[0])); !os.IsNotExist(err) {
		t.Fatal("unknown discovery caused an installation")
	}
}

func TestExplicitLocalPolicyAllowsInstallationWithoutCatalogs(t *testing.T) {
	p := project()
	p.Requirements[0].Delivery = "local"
	p.Recipes = []Recipe{{ID: "local-database", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	result, err := engine(t, p).Install(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if decision := result.Decisions[0]; decision.Status != "installed" || decision.Discovery != "local" || decision.AIEligible {
		t.Fatalf("explicit local policy did not permit deterministic installation: %+v", decision)
	}
}

func TestCatalogCoverageDistinguishesFreshEmptyAndUnknown(t *testing.T) {
	tests := []struct {
		name, body, wantState string
		status                int
		allowFallback         bool
	}{
		{"fresh empty", `{"schema":"archer.catalog/v1","services":[]}`, "fresh_no_match", http.StatusOK, true},
		{"stale empty", `{"schema":"archer.catalog/v1","services":[],"stale":true}`, catalog.StateStale, http.StatusOK, false},
		{"provider unavailable", `{"capabilities":[],"providers":{"parallax":{"available":false}}}`, catalog.StateUnavailable, http.StatusOK, false},
		{"fetch unavailable", ``, catalog.StateUnavailable, http.StatusServiceUnavailable, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			p := project()
			p.Requirements = append(p.Requirements, Requirement{Capability: "unknown"})
			p.Registries = []string{server.URL}
			p.Recipes = []Recipe{{ID: "database", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
			result, err := engine(t, p).Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, decision := range result.Decisions {
				if decision.Discovery != test.wantState {
					t.Fatalf("wrong discovery state: %+v", decision)
				}
			}
			if (result.Decisions[0].Status == "install") != test.allowFallback || result.Decisions[1].AIEligible != test.allowFallback {
				t.Fatalf("fallback disagrees with discovery evidence: %+v", result)
			}
			if !test.allowFallback && result.Decisions[0].AIEligible {
				t.Fatal("unknown discovery authorized generation")
			}
		})
	}
}

func TestFreshOneSourceCannotHideAnotherUnavailableSource(t *testing.T) {
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema":"archer.catalog/v1","services":[]}`))
	}))
	defer fresh.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unavailable.Close()
	for _, sources := range [][]string{{fresh.URL, unavailable.URL}, {unavailable.URL, fresh.URL}} {
		p := project()
		p.Registries = sources
		p.Recipes = []Recipe{{ID: "database", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
		result, err := engine(t, p).Plan(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if decision := result.Decisions[0]; decision.Status != "unresolved" || decision.AIEligible || decision.Discovery != catalog.StateUnavailable {
			t.Fatalf("partial freshness authorized fallback: %+v", decision)
		}
	}
}

func TestRemotePlanningCannotClaimItsOwnLocalEnvironment(t *testing.T) {
	p := project()
	p.Requirements[0].Delivery = "local"
	p.Recipes = []Recipe{{ID: "database", Provides: []string{"database"}, Filename: "database", Artifact: artifact(t)}}
	e := engine(t, p)
	installed, err := e.Install(context.Background(), nil, "")
	if err != nil || installed.Decisions[0].Status != "installed" {
		t.Fatalf("managed fixture was not installed: %+v %v", installed, err)
	}
	marker := filepath.Join(t.TempDir(), "probe-executed")
	t.Setenv("ARCHER_DISCOVERY_PROBE_MARKER", marker)
	e.Project.Local = []LocalCapability{{ID: "host-database", Provides: []string{"database"}, Probe: Probe{Argv: []string{os.Args[0], "-test.run=^TestDiscoveryProbeHelper$"}, Contains: "probe executed"}}}
	e.SkipLocalVerification = true
	remote, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision := remote.Decisions[0]; decision.Status != "install" || !decision.Cached {
		t.Fatalf("planning host falsely established worker installation: %+v", decision)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("remote route planning executed a local probe")
	}
	// The same command is a real successful probe when local verification is
	// enabled, ensuring the earlier absence was not merely a broken fixture.
	e.SkipLocalVerification = false
	live, err := e.Plan(context.Background(), nil)
	if err != nil || live.Decisions[0].Status != "local" {
		t.Fatalf("live host verification did not run: %+v %v", live, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("live probe left no evidence of execution", err)
	}
}

func TestDiscoveryProbeHelper(t *testing.T) {
	marker := os.Getenv("ARCHER_DISCOVERY_PROBE_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("probe executed"), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("probe executed")
}
