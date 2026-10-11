package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/archer"
	"github.com/Parallax-Web-Services/archer/internal/parallax"
	"github.com/Parallax-Web-Services/archer/internal/store"
)

func cliProject(t *testing.T, p archer.Project) (string, string) {
	t.Helper()
	t.Setenv("PARALLAX_SAAS_INDEX_URL", "")
	dir, home := t.TempDir(), t.TempDir()
	if p.Schema == "" {
		p.Schema = "archer.project/v1"
	}
	if p.Scope == "" {
		p.Scope = "cli-worker-test"
	}
	if p.Requirements == nil {
		p.Requirements = []archer.Requirement{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archer.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, home
}

func cliArtifactProject(t *testing.T) archer.Project {
	t.Helper()
	b := []byte("pinned CLI artifact fixture; not executed")
	digest := sha256.Sum256(b)
	path := filepath.Join(t.TempDir(), "example-tool")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return archer.Project{
		Requirements: []archer.Requirement{{Capability: "example-tool", Delivery: "local"}},
		Recipes:      []archer.Recipe{{ID: "example-tool", Provides: []string{"example-tool"}, Filename: "example-tool", Artifact: store.Artifact{URL: path, SHA256: hex.EncodeToString(digest[:])}}},
	}
}

func cliJSON(t *testing.T, args []string, input any) (int, parallax.Object, string) {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := runWithInput(args, bytes.NewReader(b), &out, &errout)
	value, err := parallax.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("expected one JSON object: code=%d stdout=%s stderr=%s decode=%v", code, out.String(), errout.String(), err)
	}
	return code, value, errout.String()
}

func handoffFor(approval any) parallax.Object {
	return parallax.Object{
		"schema": "parallax.handoff/v1", "handoff_id": "cli-handoff", "mission_id": "test-mission", "step_id": "dependencies",
		"from": "slipstream", "to": "archer", "owner": "archer", "objective": "Prepare pinned local tool",
		"requires": []string{"example-tool"}, "context_refs": []string{}, "attempt": 1,
		"context": parallax.Object{"approved_resolution": approval}, "x-trace": "cli-test",
	}
}

func TestCLIWorkerResolveInstallAndPreflight(t *testing.T) {
	dir, home := cliProject(t, cliArtifactProject(t))
	common := []string{"--project", dir, "--home", home, "--json", "--stdin"}
	code, resolution, diagnostic := cliJSON(t, append([]string{"resolve"}, common...), parallax.Object{"requirements": []string{"example-tool"}, "context": parallax.Object{"probe_installed": false}})
	if code != 0 || resolution["schema"] != "parallax.resolution/v1" || resolution["status"] != "satisfied" {
		t.Fatalf("resolution failed: %d %+v %s", code, resolution, diagnostic)
	}
	installed, _ := parallax.StringList(resolution["satisfied"], "satisfied")
	installable, _ := parallax.StringList(resolution["installable"], "installable")
	if len(installed) != 0 || len(installable) != 1 {
		t.Fatal("planning confused installability with verified installation", resolution)
	}
	request := handoffFor(resolution["approval"])
	code, result, diagnostic := cliJSON(t, append([]string{"install", "--preflight"}, common...), request)
	if code != 2 || result["status"] != "unsatisfied" || result["preflight"] != true {
		t.Fatalf("missing capability preflight: %d %+v %s", code, result, diagnostic)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("planning/preflight wrote state: %v %v", entries, err)
	}
	code, result, diagnostic = cliJSON(t, append([]string{"install"}, common...), request)
	if code != 0 || result["status"] != "satisfied" || result["handoff_id"] != request["handoff_id"] {
		t.Fatalf("approved installation: %d %+v %s", code, result, diagnostic)
	}
	if err := parallax.ValidateResult(result, request, nil); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, home)
	code, result, diagnostic = cliJSON(t, append([]string{"install", "--preflight"}, common...), request)
	if code != 0 || result["status"] != "satisfied" {
		t.Fatalf("ready preflight: %d %+v %s", code, result, diagnostic)
	}
	after := stateBytes(t, home)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight changed managed state")
	}
}

func stateBytes(t *testing.T, root string) []byte {
	t.Helper()
	state := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		state[rel] = string(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCLISelectorsPreserveDeclaredPolicy(t *testing.T) {
	for _, policy := range []archer.Requirement{
		{Capability: "example-tool", Delivery: "local"},
		{Capability: "example-tool", Delivery: "service"},
		{Capability: "example-tool", Constraint: ">=8.4"},
	} {
		t.Run(policy.Delivery+policy.Constraint, func(t *testing.T) {
			p := cliArtifactProject(t)
			p.Requirements = []archer.Requirement{policy}
			dir, home := cliProject(t, p)
			var out, errout bytes.Buffer
			code := run([]string{"i", "example-tool", "--plan", "--json", "--project", dir, "--home", home}, &out, &errout)
			if code != 2 {
				t.Fatal(code, errout.String())
			}
			var plan archer.Plan
			if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Decisions) != 1 || plan.Decisions[0].Requirement != policy {
				t.Fatal("selector changed declared delivery or version policy", plan)
			}
			if policy.Delivery == "local" && plan.Decisions[0].Status != "install" {
				t.Fatal("explicit local route lost", plan)
			}
			if policy.Delivery != "local" && plan.Decisions[0].Status != "unresolved" {
				t.Fatal("selector bypassed declared policy", plan)
			}
		})
	}
}

func TestCLIRejectsInvalidWorkerInputAndFlagCombinations(t *testing.T) {
	dir, home := cliProject(t, archer.Project{})
	for _, input := range []string{`[]`, `{}`, `{} {}`, `{"requirements":[],"requirements":["other"]}`, strings.Repeat(" ", parallax.MaxInputBytes+1)} {
		var out, errout bytes.Buffer
		code := runWithInput([]string{"resolve", "--stdin", "--project", dir, "--home", home}, strings.NewReader(input), &out, &errout)
		if code != 1 {
			t.Fatalf("accepted malformed request: %d %s", code, input[:min(len(input), 100)])
		}
		object, err := parallax.Decode(bytes.NewReader(out.Bytes()))
		if err != nil || object["error"] == nil {
			t.Fatalf("malformed request did not yield one error object: %s %v", out.String(), err)
		}
	}
	for _, args := range [][]string{{"plan", "--stdin"}, {"install", "--preflight"}, {"install", "--stdin", "--plan"}, {"install", "--stdin", "example-tool"}, {"install", "--stdin", "--approval", "local-plan-id"}} {
		var out, errout bytes.Buffer
		if code := runWithInput(args, strings.NewReader(`{}`), &out, &errout); code != 1 || errout.Len() == 0 {
			t.Fatal("invalid worker flag combination accepted", args, code)
		}
	}
}

func TestCapabilitiesAreAdvertisementsWithoutProbesOrWrites(t *testing.T) {
	dir, home := cliProject(t, archer.Project{Local: []archer.LocalCapability{{ID: "unavailable", Provides: []string{"php"}, Probe: archer.Probe{Argv: []string{"archer-missing-probe-never-run"}}}}})
	var out, errout bytes.Buffer
	if code := run([]string{"capabilities", "--json", "--project", dir, "--home", home}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	var capabilities []parallax.Object
	if err := json.Unmarshal(out.Bytes(), &capabilities); err != nil || len(capabilities) != 1 || capabilities[0]["capability"] != "php" {
		t.Fatal("bad capability advertisements", out.String(), err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("capabilities wrote state", entries, err)
	}
}
