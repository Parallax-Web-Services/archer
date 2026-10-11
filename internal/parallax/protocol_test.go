package parallax

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) Object {
	t.Helper()
	file, err := os.Open("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	object, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func clone(t *testing.T, object Object) Object {
	t.Helper()
	data, err := Canonical(object)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return copy
}

func rehash(t *testing.T, approval Object) {
	t.Helper()
	body := Object{}
	for key, value := range approval {
		if key != "sha256" {
			body[key] = value
		}
	}
	digest, err := Hash(body)
	if err != nil {
		t.Fatal(err)
	}
	approval["sha256"] = digest
}

func TestSharedParallaxFixtures(t *testing.T) {
	request, result := fixture(t, "handoff"), fixture(t, "result")
	if err := ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResult(result, request, nil); err != nil {
		t.Fatal(err)
	}
	if request["x-trace"].(Object)["project"] != "demo" || result["x-provider"].(Object)["version"] != json.Number("1") {
		t.Fatal("extension lost")
	}
	resolution := fixture(t, "resolution")
	approval := resolution["approval"].(Object)
	closure, err := ValidateApproval(approval, []string{"laravel"}, "install")
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != 3 || closure["laravel"] == nil || closure["php"] == nil || closure["composer"] == nil {
		t.Fatalf("wrong subset closure: %v", closure)
	}
	if closure["custom-community-ranking"] != nil {
		t.Fatal("unselected work included")
	}
	if approval["sha256"] != "8027fe510d84b16ce6a14cb168b993ec8a5fd1cb2e81d162cda7a0c7bdc37c6c" {
		t.Fatal("fixture approval changed")
	}
}

func TestDecodePreservesShapePrecisionAndStableKeys(t *testing.T) {
	object, err := Decode(strings.NewReader(`{"z":9007199254740993,"x-empty":{},"x-list":[{},[]],"a":"https://example.test/<é>&"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Canonical(object)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"https://example.test/<é>&","x-empty":{},"x-list":[{},[]],"z":9007199254740993}`
	if string(data) != want {
		t.Fatalf("canonical shape/precision mismatch:\n%s", data)
	}
	var reordered Object = Object{"a": object["a"], "z": object["z"], "x-list": object["x-list"], "x-empty": object["x-empty"]}
	digest, _ := Hash(object)
	other, _ := Hash(reordered)
	if digest != other {
		t.Fatal("insertion order changed hash")
	}
	typed := struct {
		Z int    `json:"z"`
		A string `json:"a"`
	}{Z: 1, A: "first"}
	data, err = Canonical(typed)
	if err != nil || string(data) != `{"a":"first","z":1}` {
		t.Fatalf("typed object keys were not sorted: %s %v", data, err)
	}
}

func TestDecodeRejectsAmbiguousOrUnboundedInputs(t *testing.T) {
	inputs := []string{`[]`, `null`, `{`, `{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{} {}`, `{} garbage`, `{"n":NaN}`, `{"p":` + strings.Repeat("[", 130) + strings.Repeat("]", 130) + `}`, `{"p":"` + strings.Repeat("x", MaxInputBytes) + `"}`}
	for i, input := range inputs {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid input %d accepted", i)
		}
	}
	if _, err := Decode(bytes.NewReader([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestRequestRejectsOwnershipAttemptsAndShape(t *testing.T) {
	mutations := map[string]any{"owner": "slipstream", "attempt": json.Number("4"), "context": []any{}, "requires": []any{"relay", "relay"}, "context_refs": []any{""}, "parent_step_id": json.Number("1"), "schema": "parallax.handoff/v2"}
	for field, value := range mutations {
		request := fixture(t, "handoff")
		request[field] = value
		if err := ValidateRequest(request); err == nil {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	for _, value := range []any{nil, "1", json.Number("1.0"), json.Number("1e0"), 0, 4} {
		request := fixture(t, "handoff")
		request["attempt"] = value
		if err := ValidateRequest(request); err == nil {
			t.Fatalf("invalid attempt %v accepted", value)
		}
	}
	for _, field := range []string{"handoff_id", "mission_id", "step_id", "from", "to", "owner", "objective", "requires", "context_refs", "attempt"} {
		request := fixture(t, "handoff")
		delete(request, field)
		if err := ValidateRequest(request); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
}

func TestResultRejectsFalseCoverageAndMissingEvidence(t *testing.T) {
	request := fixture(t, "handoff")
	mutations := map[string]any{"owner": "nexus", "handoff_id": "other", "status": "satisfied", "remaining": []any{}, "satisfied": []any{"laravel", "relay", "relay"}, "evidence": []any{}, "artifacts": Object{}}
	for field, value := range mutations {
		result := fixture(t, "result")
		result[field] = value
		if err := ValidateResult(result, request, nil); err == nil {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	result := fixture(t, "result")
	result["remaining"] = []string{"unrequested"}
	if err := ValidateResult(result, request, nil); err == nil {
		t.Fatal("new work accepted")
	}
	result = fixture(t, "result")
	result["remaining"] = []string{"laravel", "custom-community-ranking"}
	if err := ValidateResult(result, request, nil); err == nil {
		t.Fatal("overlapping partition accepted")
	}
	result = fixture(t, "result")
	result["status"] = "failed"
	if err := ValidateResult(result, request, nil); err != nil {
		t.Fatalf("verified partial failure rejected: %v", err)
	}
}

func TestResultHistoryShrinksAndStopsStalls(t *testing.T) {
	request, result := fixture(t, "handoff"), fixture(t, "result")
	prior := clone(t, result)
	prior["status"], prior["satisfied"], prior["remaining"], prior["evidence"] = "unsatisfied", []string{}, request["requires"], []any{}
	if err := ValidateResult(result, request, []Object{prior}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResult(result, request, []Object{result, result}); err == nil {
		t.Fatal("third unchanged result accepted")
	}
	if err := ValidateResult(prior, request, []Object{result}); err == nil {
		t.Fatal("expanded work accepted")
	}
	if err := ValidateResult(result, request, []Object{result, prior}); err == nil {
		t.Fatal("expansion within prior history accepted")
	}
	if err := ValidateResult(result, request, []Object{prior, result}); err != nil {
		t.Fatalf("valid shrinking history rejected: %v", err)
	}
	request["attempt"] = 3
	result["next_owner"] = "archer"
	if err := ValidateResult(result, request, nil); err == nil {
		t.Fatal("attempt limit allowed same-owner continuation")
	}
}

func TestResultVersionEvidence(t *testing.T) {
	for _, test := range []struct {
		requirement, version string
		valid                bool
	}{
		{"relay>=2", "2.1.0", true}, {"relay>=2", "1.0.0", false}, {"relay^0.2.3", "0.2.9", true}, {"relay^0.2.3", "0.3.0", false},
		{"relay~1.2.3", "1.2.9", true}, {"relay~1.2.3", "1.3.0", false}, {"relay=2.0.0", "2.0.0", true},
		{"relay>2", "2.0", false}, {"relay<2.0", "2", false}, {"relay=2", "2.0.0", true},
		{"relay=2.0.0", "2", true}, {"relay>2", "2.0.1", true}, {"relay<2.0", "1.9.9", true},
		{"relay>=2", "2.1.0-beta", false}, {"relay>=2.*", "2.1.0", false}, {"relay>=2", "", false},
	} {
		request := fixture(t, "handoff")
		request["requires"] = []string{test.requirement}
		result := Object{"schema": "parallax.handoff-result/v1", "handoff_id": request["handoff_id"], "owner": "archer", "status": "satisfied", "satisfied": []string{test.requirement}, "remaining": []string{}, "artifacts": []any{}, "next_owner": "slipstream", "evidence": []any{Object{"type": "already_installed", "capability": "relay", "status": "ok", "version": test.version}}}
		err := ValidateResult(result, request, nil)
		if (err == nil) != test.valid {
			t.Errorf("%s evidence %s valid=%v: %v", test.requirement, test.version, test.valid, err)
		}
	}
}

func TestApprovalChecksEveryIncomingDiamondConstraint(t *testing.T) {
	makeNode := func(constraints, dependencies []string) Object {
		return Object{"manifest": nil, "manifest_sha256": nil, "driver": nil, "route_sha256": nil, "requires": dependencies, "version_constraint": constraints}
	}
	nodes := Object{
		"a":   makeNode([]string{"a"}, []string{"php>=8.4"}),
		"b":   makeNode([]string{"b"}, []string{"php>=9"}),
		"php": makeNode([]string{"php>=8.4", "php>=9"}, []string{}),
	}
	approval, err := NewApproval([]string{"a", "b"}, nodes, nil)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := ValidateApproval(approval, []string{"a", "b"}, "install")
	if err != nil || len(closure) != 3 {
		t.Fatalf("valid shared dependency rejected: %v", err)
	}
	approval["nodes"].(Object)["php"].(Object)["version_constraint"] = []string{"php>=8.4"}
	rehash(t, approval)
	if _, err := ValidateApproval(approval, []string{"a", "b"}, "install"); err == nil {
		t.Fatal("second incoming dependency constraint bypassed validation")
	}
}

func TestApprovalRejectsTamperSubsetClosureAndConstraints(t *testing.T) {
	original := fixture(t, "resolution")["approval"].(Object)
	approval := clone(t, original)
	approval["operation"] = "remove"
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("operation mismatch accepted")
	}
	approval = clone(t, original)
	approval["x-trace"] = "changed"
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("body extension tamper accepted")
	}
	if _, err := ValidateApproval(original, []string{"redis"}, "install"); err == nil {
		t.Fatal("unapproved subset accepted")
	}
	approval = clone(t, original)
	delete(approval["nodes"].(Object), "php")
	rehash(t, approval)
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("missing dependency accepted")
	}
	approval = clone(t, original)
	approval["nodes"].(Object)["php"].(Object)["version_constraint"] = []string{"redis>=2"}
	rehash(t, approval)
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("foreign constraint accepted")
	}
	approval = clone(t, original)
	approval["nodes"].(Object)["php"].(Object)["version_constraint"] = []string{"php>=8.3"}
	rehash(t, approval)
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("missing exact dependency constraint accepted")
	}
	approval = clone(t, original)
	approval["nodes"].(Object)["php"].(Object)["requires"] = []string{"laravel"}
	rehash(t, approval)
	if _, err := ValidateApproval(approval, []string{"laravel"}, "install"); err == nil {
		t.Fatal("cyclic dependency accepted")
	}
}

func TestNewApprovalBindsExtensionsAndDoesNotMutateRequirements(t *testing.T) {
	nodes := fixture(t, "resolution")["approval"].(Object)["nodes"].(Object)
	requirements := []string{"laravel", "custom-community-ranking"}
	approval, err := NewApproval(requirements, nodes, Object{"x-go-dialect": "archer.project/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if requirements[0] != "laravel" {
		t.Fatal("caller requirement order mutated")
	}
	if _, err := ValidateApproval(approval, requirements, "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewApproval(requirements, nodes, Object{"operation": "remove"}); err == nil {
		t.Fatal("reserved extension accepted")
	}
}

func TestRequirementGrammar(t *testing.T) {
	for _, input := range []string{"laravel", "php>=8.4", "relay ^ 2.0", "service_name~1.2"} {
		if _, _, err := ParseRequirement(input); err != nil {
			t.Fatalf("valid requirement %s: %v", input, err)
		}
	}
	name, constraint, err := ParseRequirement("php >= 8.4")
	if err != nil || name != "php" || constraint != ">=8.4" {
		t.Fatal("constraint normalization failed")
	}
	for _, input := range []string{"Laravel", "vendor/package", " relay", "relay ", "relay|other", "relay>=", "relay;rm"} {
		if _, _, err := ParseRequirement(input); err == nil {
			t.Fatalf("invalid requirement %s accepted", input)
		}
	}
}
