package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/archer"
	"github.com/Parallax-Web-Services/archer/internal/parallax"
	"github.com/Parallax-Web-Services/archer/internal/store"
)

func project() archer.Project {
	return archer.Project{Schema: recipeSchema, Scope: "worker-test", Requirements: []archer.Requirement{{Capability: "tool", Delivery: "local"}}}
}
func engine(t *testing.T, p archer.Project, extensions parallax.Object) *archer.Engine {
	t.Helper()
	t.Setenv("PARALLAX_SAAS_INDEX_URL", "")
	dir := t.TempDir()
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	object, err := parallax.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range extensions {
		object[key] = value
	}
	encoded, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archer.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	e, err := archer.Load(dir, filepath.Join(dir, "home"), false)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func artifact(t *testing.T, name string, data []byte) archer.Recipe {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return archer.Recipe{ID: name, Provides: []string{name}, Filename: name, Artifact: store.Artifact{URL: path, SHA256: hex.EncodeToString(hash[:])}}
}
func resolve(t *testing.T, e *archer.Engine, requires []string, workerContext parallax.Object) parallax.Object {
	t.Helper()
	result, err := Resolve(context.Background(), e, parallax.Object{"requirements": requires, "context": workerContext})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func handoff(requires []string, approval parallax.Object) parallax.Object {
	return parallax.Object{
		"schema": "parallax.handoff/v1", "handoff_id": "worker-handoff", "mission_id": "worker-mission", "step_id": "install",
		"parent_step_id": nil, "from": "slipstream", "to": "archer", "owner": "archer", "objective": "Prepare fixture capabilities",
		"requires": requires, "context_refs": []string{}, "attempt": 1,
		"context": parallax.Object{"approved_resolution": approval},
	}
}
func list(t *testing.T, object parallax.Object, key string) []string {
	t.Helper()
	value, err := parallax.StringList(object[key], key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func checkNoHome(t *testing.T, e *archer.Engine) {
	t.Helper()
	if _, err := os.Stat(e.Home); !os.IsNotExist(err) {
		t.Fatalf("worker wrote home unexpectedly: %v", err)
	}
}
func noMarker(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("probe executed unexpectedly: %v", err)
	}
}
func proofProbe(t *testing.T, mode string, extra ...string) (archer.LocalCapability, string) {
	t.Helper()
	t.Setenv("ARCHER_WORKER_PROBE_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "probe-marker")
	argv := []string{executable, "-test.run=^TestWorkerProbeProcess$", "--", mode, marker}
	argv = append(argv, extra...)
	return archer.LocalCapability{ID: "local-tool", Provides: []string{"tool"}, Probe: archer.Probe{Argv: argv, Contains: "ready"}}, marker
}

func TestWorkerProbeProcess(t *testing.T) {
	if os.Getenv("ARCHER_WORKER_PROBE_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) < 2 {
		os.Exit(3)
	}
	if err := os.WriteFile(args[1], []byte("called"), 0600); err != nil {
		os.Exit(4)
	}
	switch args[0] {
	case "toggle":
		data, err := os.ReadFile(args[2])
		if err != nil || string(data) != "ready" {
			os.Exit(1)
		}
	case "change-source":
		if err := os.WriteFile(args[2], []byte(`{"changed":true}`), 0600); err != nil {
			os.Exit(5)
		}
	case "missing":
		os.Exit(1)
	}
	fmt.Println("ready")
	os.Exit(0)
}

func TestResolveExecuteRoundTripAndSubset(t *testing.T) {
	p := project()
	p.Requirements = append(p.Requirements, archer.Requirement{Capability: "mystery", Delivery: "local"})
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("verified executable fixture"))}
	e := engine(t, p, parallax.Object{"x-product": "café", "x-number": json.Number("1e3")})
	resolved := resolve(t, e, []string{"tool", "mystery"}, parallax.Object{"probe_installed": false})
	if resolved["status"] != "partial" || !reflect.DeepEqual(list(t, resolved, "installable"), []string{"tool"}) || !reflect.DeepEqual(list(t, resolved, "remaining"), []string{"mystery"}) {
		t.Fatalf("incorrect coverage: %#v", resolved)
	}
	checkNoHome(t, e)
	approval := resolved["approval"].(parallax.Object)
	t.Run("PHP associative relay preserves native approval digest", func(t *testing.T) {
		php, err := exec.LookPath("php")
		if err != nil {
			t.Skip("native PHP unavailable")
		}
		encoded, err := json.Marshal(approval)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(php, "-r", `echo json_encode(json_decode(stream_get_contents(STDIN), true, 512, JSON_THROW_ON_ERROR), JSON_THROW_ON_ERROR|JSON_UNESCAPED_SLASHES|JSON_UNESCAPED_UNICODE);`)
		command.Stdin = bytes.NewReader(encoded)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("PHP relay: %v: %s", err, output)
		}
		relayed, err := parallax.Decode(bytes.NewReader(output))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parallax.ValidateApproval(relayed, []string{"tool"}, "install"); err != nil {
			t.Fatalf("PHP changed digest: %v", err)
		}
	})
	request := handoff([]string{"tool"}, approval)
	request["x-trace"] = parallax.Object{"status": "extension status", "literal": "<&>"}
	result, err := Execute(context.Background(), e, request, false)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "satisfied" || !reflect.DeepEqual(list(t, result, "satisfied"), []string{"tool"}) || len(list(t, result, "remaining")) != 0 {
		t.Fatalf("incorrect subset result: %#v", result)
	}
	if result["request_extensions"].(parallax.Object)["x-trace"] == nil {
		t.Fatal("extension lost")
	}
	if err := parallax.ValidateResult(result, request, nil); err != nil {
		t.Fatal(err)
	}
	// Scoped learning changes plan identities but not the portable recipe binding.
	result, err = Execute(context.Background(), e, request, false)
	if err != nil || result["status"] != "satisfied" {
		t.Fatalf("repeat with original binding: %v %#v", err, result)
	}
}

func TestPreflightNoCatalogOrStateWrites(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"schema":"archer.catalog/v1","services":[]}`)
	}))
	defer server.Close()
	p := project()
	p.Requirements[0].Delivery = ""
	p.Registries = []string{server.URL}
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("preflight binary"))}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
	request := handoff([]string{"tool"}, resolved["approval"].(parallax.Object))
	before, count := files(t, e.Home), calls.Load()
	result, err := Execute(context.Background(), e, request, true)
	if err != nil {
		t.Fatal(err)
	}
	if result["preflight"] != true || result["operation"] != "install" || result["status"] != "unsatisfied" || !reflect.DeepEqual(list(t, result, "remaining"), []string{"tool"}) {
		t.Fatalf("incorrect preflight: %#v", result)
	}
	if !reflect.DeepEqual(before, files(t, e.Home)) || calls.Load() != count {
		t.Fatal("preflight wrote state or fetched a catalog")
	}
	if len(e.Project.Registries) != 1 {
		t.Fatal("preflight mutated the original project")
	}
	installed, err := Execute(context.Background(), e, request, false)
	if err != nil || installed["status"] != "satisfied" {
		t.Fatalf("install: %v %#v", err, installed)
	}
	before, count = files(t, e.Home), calls.Load()
	result, err = Execute(context.Background(), e, request, true)
	if err != nil || result["status"] != "satisfied" {
		t.Fatalf("verified preflight: %v %#v", err, result)
	}
	if !reflect.DeepEqual(before, files(t, e.Home)) || calls.Load() != count {
		t.Fatal("verified preflight wrote state or fetched a catalog")
	}
}
func files(t *testing.T, directory string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%x:%v", hash, info.ModTime())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRejectedBindingRunsNoProbes(t *testing.T) {
	for _, kind := range []string{"missing", "tampered", "legacy", "plan-id", "raw-extension", "source", "probe-route", "registry", "wrong-operation"} {
		t.Run(kind, func(t *testing.T) {
			p := project()
			probe, marker := proofProbe(t, "ready")
			p.Local = []archer.LocalCapability{probe}
			p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("frozen fixture"))}
			e := engine(t, p, parallax.Object{"x-policy": "frozen"})
			resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
			approval := resolved["approval"].(parallax.Object)
			request := handoff([]string{"tool"}, approval)
			code := "approval_drift"
			switch kind {
			case "missing":
				delete(request["context"].(parallax.Object), "approved_resolution")
				code = "approval_required"
			case "tampered":
				approval["sha256"] = strings.Repeat("0", 64)
			case "legacy":
				legacy, err := parallax.NewApproval([]string{"tool"}, approval["nodes"].(parallax.Object), nil)
				if err != nil {
					t.Fatal(err)
				}
				request["context"].(parallax.Object)["approved_resolution"] = legacy
				code = "unsupported_approval"
			case "plan-id":
				request["context"].(parallax.Object)["approved_resolution"] = approval["sha256"]
				code = "approval_required"
			case "raw-extension":
				data, _ := os.ReadFile(filepath.Join(e.ProjectDir, "archer.json"))
				data = bytes.Replace(data, []byte(`"frozen"`), []byte(`"changed"`), 1)
				if err := os.WriteFile(filepath.Join(e.ProjectDir, "archer.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := os.WriteFile(filepath.Join(e.ProjectDir, "package.json"), []byte(`{"new":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "probe-route":
				e.Project.Local[0].Probe.Contains = "new verification contract"
			case "registry":
				e.Project.Registries = []string{"https://unapproved.invalid/catalog"}
			case "wrong-operation":
				approval["operation"] = "remove"
				delete(approval, "sha256")
				digest, _ := parallax.Hash(approval)
				approval["sha256"] = digest
			}
			result, err := Execute(context.Background(), e, request, false)
			if err == nil || result == nil || result["status"] != "failed" || result["failure_code"] != code {
				t.Fatalf("expected %s: %v %#v", code, err, result)
			}
			if !reflect.DeepEqual(list(t, result, "remaining"), []string{"tool"}) {
				t.Fatalf("lost remaining request: %#v", result)
			}
			if err := parallax.ValidateResult(result, request, nil); err != nil {
				t.Fatal(err)
			}
			noMarker(t, marker)
			checkNoHome(t, e)
		})
	}
}

func TestOwnershipAndPlatformRejectedBeforeProbes(t *testing.T) {
	p := project()
	probe, marker := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
	approval := resolved["approval"].(parallax.Object)
	request := handoff([]string{"tool"}, approval)
	request["to"], request["owner"] = "other", "other"
	result, err := Execute(context.Background(), e, request, false)
	if err == nil || result != nil {
		t.Fatalf("wrong owner accepted: %v %#v", err, result)
	}
	request = handoff([]string{"tool"}, approval)
	request["context"].(parallax.Object)["client_environment"] = parallax.Object{"platform": "unsupported-os/amd64"}
	result, err = Execute(context.Background(), e, request, false)
	if err == nil || result["failure_code"] != "platform_mismatch" {
		t.Fatalf("platform accepted: %v %#v", err, result)
	}
	_, err = Resolve(context.Background(), e, parallax.Object{"requirements": []string{"tool"}, "context": parallax.Object{"architecture": "unsupported-arch"}})
	if err == nil {
		t.Fatal("planning accepted incompatible architecture")
	}
	noMarker(t, marker)
	checkNoHome(t, e)
}

func TestLiveRouteDriftCannotSelectAnotherInstaller(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(state, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	p := project()
	probe, _ := proofProbe(t, "toggle", state)
	p.Local = []archer.LocalCapability{probe}
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("unapproved alternate installer"))}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{})
	if err := os.WriteFile(state, []byte("missing"), 0600); err != nil {
		t.Fatal(err)
	}
	request := handoff([]string{"tool"}, resolved["approval"].(parallax.Object))
	result, err := Execute(context.Background(), e, request, false)
	if err == nil || result["failure_code"] != "approval_drift" {
		t.Fatalf("alternate route accepted: %v %#v", err, result)
	}
	checkNoHome(t, e)
}

func TestExistingVerifiedStateMaySkipApprovedInstaller(t *testing.T) {
	p := project()
	probe, _ := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("unused reviewed installer"))}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
	result, err := Execute(context.Background(), e, handoff([]string{"tool"}, resolved["approval"].(parallax.Object)), false)
	if err != nil || result["status"] != "satisfied" {
		t.Fatalf("verified reuse: %v %#v", err, result)
	}
	if len(result["artifacts"].([]parallax.Object)) != 0 {
		t.Fatal("installed unused artifact")
	}
	if _, err := os.Stat(filepath.Join(e.Home, "cache")); !os.IsNotExist(err) {
		t.Fatal("unused installer touched artifact cache")
	}
}

func TestConstraintsRemainExactAndProjectPolicyPersists(t *testing.T) {
	for _, kind := range []string{"request", "project"} {
		t.Run(kind, func(t *testing.T) {
			p := project()
			probe, marker := proofProbe(t, "ready")
			p.Local = []archer.LocalCapability{probe}
			requires := []string{"tool >= 2.0"}
			if kind == "project" {
				p.Requirements[0].Constraint = ">=2.0"
				requires = []string{"tool"}
			}
			e := engine(t, p, nil)
			resolved := resolve(t, e, requires, parallax.Object{})
			if !reflect.DeepEqual(list(t, resolved, "remaining"), requires) || len(list(t, resolved, "satisfied")) != 0 {
				t.Fatalf("constraint falsely satisfied: %#v", resolved)
			}
			result, err := Execute(context.Background(), e, handoff(requires, resolved["approval"].(parallax.Object)), true)
			if err != nil || result["status"] != "unsatisfied" || !reflect.DeepEqual(list(t, result, "remaining"), requires) {
				t.Fatalf("constraint lost: %v %#v", err, result)
			}
			noMarker(t, marker)
			checkNoHome(t, e)
		})
	}
	p := project()
	p.Requirements = append(p.Requirements, archer.Requirement{Capability: "tool", Delivery: "service"})
	e := engine(t, p, nil)
	if _, err := Resolve(context.Background(), e, parallax.Object{"requirements": []string{"tool"}}); err == nil {
		t.Fatal("conflicting duplicate project policy accepted")
	}
}

func TestProbeSuppressionAndClientAssertions(t *testing.T) {
	p := project()
	probe, marker := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{
		"probe_installed": false, "installed": parallax.Object{"tool": true},
		"available_drivers": []string{"binary"}, "client_environment": parallax.Object{"platform": runtime.GOOS + "/" + runtime.GOARCH},
	})
	if len(list(t, resolved, "satisfied")) != 0 || !reflect.DeepEqual(list(t, resolved, "remaining"), []string{"tool"}) {
		t.Fatalf("assertions counted as verification: %#v", resolved)
	}
	noMarker(t, marker)
	checkNoHome(t, e)
}

func TestEmptyRequestsCannotDefaultToFullProject(t *testing.T) {
	p := project()
	probe, marker := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("must not install"))}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{}, parallax.Object{})
	if len(list(t, resolved, "satisfied")) != 0 || len(list(t, resolved, "installable")) != 0 || len(list(t, resolved, "remaining")) != 0 {
		t.Fatal("empty resolve expanded to project")
	}
	request := handoff([]string{}, resolved["approval"].(parallax.Object))
	for _, preflight := range []bool{false, true} {
		result, err := Execute(context.Background(), e, request, preflight)
		if err != nil || result["status"] != "satisfied" || len(list(t, result, "satisfied")) != 0 {
			t.Fatalf("empty execution: %v %#v", err, result)
		}
	}
	noMarker(t, marker)
	checkNoHome(t, e)
}

func TestPartialFailureRetainsVerifiedSubset(t *testing.T) {
	p := project()
	p.Requirements = append(p.Requirements, archer.Requirement{Capability: "missing", Delivery: "local"})
	good := artifact(t, "tool", []byte("first successful artifact"))
	bad := artifact(t, "missing", []byte("second missing artifact"))
	if err := os.Remove(bad.Artifact.URL); err != nil {
		t.Fatal(err)
	}
	p.Recipes = []archer.Recipe{good, bad}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool", "missing"}, parallax.Object{})
	request := handoff([]string{"tool", "missing"}, resolved["approval"].(parallax.Object))
	request["from"] = "coordinator"
	result, err := Execute(context.Background(), e, request, false)
	if err == nil || result["status"] != "failed" || result["next_owner"] != "coordinator" {
		t.Fatalf("expected execution failure: %v %#v", err, result)
	}
	if !reflect.DeepEqual(list(t, result, "satisfied"), []string{"tool"}) || !reflect.DeepEqual(list(t, result, "remaining"), []string{"missing"}) {
		t.Fatalf("partial result lost: %#v", result)
	}
	if err := parallax.ValidateResult(result, request, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProbeSourceMutationRejectedBeforeInstall(t *testing.T) {
	source := filepath.Join(t.TempDir(), "unused")
	p := project()
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("must not publish after source mutation"))}
	e := engine(t, p, nil)
	source = filepath.Join(e.ProjectDir, "package.json")
	if err := os.WriteFile(source, []byte(`{"changed":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	probe, _ := proofProbe(t, "change-source", source)
	e.Project.Local = []archer.LocalCapability{probe}
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
	result, err := Execute(context.Background(), e, handoff([]string{"tool"}, resolved["approval"].(parallax.Object)), false)
	if err == nil || result["failure_code"] != "approval_drift" {
		t.Fatalf("source mutation accepted: %v %#v", err, result)
	}
	checkNoHome(t, e)
}

func TestNativeDependencyNodesUnsupportedBeforeProbes(t *testing.T) {
	p := project()
	probe, marker := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool", "dependency"}, parallax.Object{"probe_installed": false})
	old := resolved["approval"].(parallax.Object)
	nodes := old["nodes"].(parallax.Object)
	nodes["tool"].(parallax.Object)["requires"] = []string{"dependency"}
	approval, err := parallax.NewApproval([]string{"tool", "dependency"}, nodes, parallax.Object{"recipe_schema": recipeSchema})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Execute(context.Background(), e, handoff([]string{"tool"}, approval), false)
	if err == nil || result["failure_code"] != "unsupported_approval" {
		t.Fatalf("dependency accepted: %v %#v", err, result)
	}
	noMarker(t, marker)
	checkNoHome(t, e)
}

func TestCapabilitiesOnlyAdvertiseConfiguredProviders(t *testing.T) {
	p := project()
	p.Requirements = append(p.Requirements, archer.Requirement{Capability: "unknown-intent"})
	probe, marker := proofProbe(t, "ready")
	p.Local = []archer.LocalCapability{probe}
	e := engine(t, p, nil)
	descriptors, err := Capabilities(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 || descriptors[0]["capability"] != "tool" {
		t.Fatalf("advertised intentions instead of providers: %#v", descriptors)
	}
	noMarker(t, marker)
	checkNoHome(t, e)
}
