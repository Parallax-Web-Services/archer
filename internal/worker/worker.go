// Package worker adapts Archer's native projects to Parallax v1 transport.
// PHP recipe approvals cannot authorize this different executor.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/archer"
	"github.com/Parallax-Web-Services/archer/internal/parallax"
)

const recipeSchema = "archer.project/v1"
const maxSourceBytes = 16 << 20

var unsupportedApproval = errors.New("unsupported native approval")

// Capabilities describes configured names without probes or catalog effects.
func Capabilities(e *archer.Engine) ([]parallax.Object, error) {
	if e == nil {
		return nil, errors.New("Archer engine is required")
	}
	names := map[string]bool{}
	add := func(values []string) {
		for _, value := range values {
			name, constraint, err := parallax.ParseRequirement(value)
			if err == nil && constraint == "" {
				names[name] = true
			}
		}
	}
	for _, s := range e.Project.Services {
		add(s.Provides)
	}
	for _, l := range e.Project.Local {
		add(l.Provides)
	}
	for _, r := range e.Project.Recipes {
		add(r.Provides)
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	out := []parallax.Object{}
	for _, name := range ordered {
		out = append(out, parallax.Object{
			"schema": "parallax.capability/v1", "provider": "archer", "capability": name,
			"actions": []string{"resolve", "install"}, "provides": []string{name}, "requires": []string{},
			"metadata": parallax.Object{"deterministic": true, "code_generation": false, "recipe_schema": recipeSchema},
		})
	}
	return out, nil
}

// Resolve separates installable coverage from verified completion.
func Resolve(ctx context.Context, e *archer.Engine, input parallax.Object) (parallax.Object, error) {
	if e == nil {
		return nil, errors.New("Archer engine is required")
	}
	requirements, err := parallax.StringList(input["requirements"], "requirements")
	if err != nil {
		return nil, err
	}
	workerContext, err := objectContext(input)
	if err != nil {
		return nil, err
	}
	if err := validatePlatform(workerContext); err != nil {
		return nil, err
	}
	mapped, err := mapRequirements(e.Project, requirements)
	if err != nil {
		return nil, err
	}
	clone := *e
	if value, exists := mergedContext(workerContext)["probe_installed"]; exists {
		probe, ok := value.(bool)
		if !ok {
			return nil, errors.New("context.probe_installed must be boolean")
		}
		clone.SkipLocalVerification = !probe
	}
	plan := emptyPlan(e)
	if len(mapped) != 0 {
		plan, err = clone.Plan(ctx, mapped)
		if err != nil {
			return nil, err
		}
	}
	nodes, err := approvalNodes(e, requirements, mapped, plan)
	if err != nil {
		return nil, err
	}
	approval, err := parallax.NewApproval(requirements, nodes, parallax.Object{"recipe_schema": recipeSchema})
	if err != nil {
		return nil, err
	}
	satisfied, installable, remaining := []string{}, []string{}, []string{}
	evidence, dependencies := []parallax.Object{}, []parallax.Object{}
	statuses := map[string]string{}
	routes, providers, constraints := parallax.Object{}, parallax.Object{}, parallax.Object{}
	for i, original := range requirements {
		d := plan.Decisions[i]
		name, _, _ := parallax.ParseRequirement(original)
		status := "unsatisfied"
		if verified(d) {
			status = "satisfied"
		} else if d.Status == "install" {
			status = "installable"
		}
		previous := statuses[name]
		if previous == "" || status == "unsatisfied" || (status == "installable" && previous == "satisfied") {
			statuses[name] = status
		}
		driver, routeHash, err := decisionRoute(e, d)
		if err != nil {
			return nil, err
		}
		routes[original] = parallax.Object{"driver": driver, "route_sha256": routeHash, "provider": nullable(d.Provider), "already_installed": verified(d), "status": d.Status, "reason": d.Reason}
		switch {
		case verified(d):
			satisfied = append(satisfied, original)
			evidence = append(evidence, verification(original, d))
			providers[original] = "archer"
		case d.Status == "install":
			installable = append(installable, original)
			providers[original] = "archer"
		default:
			remaining = append(remaining, original)
		}
	}
	for _, name := range sortedKeys(nodes) {
		node := nodes[name].(parallax.Object)
		constraints[name] = node["version_constraint"]
		dependencies = append(dependencies, parallax.Object{"name": name, "status": statuses[name], "requires": []string{}, "manifest": name, "plan": parallax.Object{"driver": node["driver"], "route_sha256": node["route_sha256"]}})
	}
	result := parallax.Object{
		"schema": "parallax.resolution/v1", "provider": "archer", "status": coverageStatus(len(satisfied)+len(installable), len(remaining)),
		"platform": plan.Platform, "satisfied": satisfied, "installable": installable, "remaining": remaining,
		"routes": routes, "evidence": evidence, "providers": providers, "approval": approval,
		"dependencies": dependencies, "constraints": constraints, "dry_run": true, "ai_requests": 0,
	}
	preserveExtensions(result, input, []string{"requirements"})
	return result, nil
}

// Execute requires a frozen native approval. Invalid envelopes return no result;
// operational/drift failures return a valid failed result and an error.
func Execute(ctx context.Context, e *archer.Engine, request parallax.Object, preflight bool) (parallax.Object, error) {
	if err := parallax.ValidateRequest(request); err != nil {
		return nil, err
	}
	if request["to"] != "archer" || request["owner"] != "archer" {
		return nil, errors.New("Archer only executes handoffs addressed to and owned by archer")
	}
	if e == nil {
		return failed(request, preflight, "execution_failed", errors.New("Archer engine is required"), archer.Plan{})
	}
	requirements, _ := parallax.StringList(request["requires"], "requires")
	workerContext, err := objectContext(request)
	if err != nil {
		return nil, err
	}
	approval, ok := workerContext["approved_resolution"].(parallax.Object)
	if !ok || approval == nil {
		return failed(request, preflight, "approval_required", errors.New("context.approved_resolution is required"), archer.Plan{})
	}
	nodes, err := parallax.ValidateApproval(approval, requirements, "install")
	if err != nil {
		return failed(request, preflight, "approval_drift", err, archer.Plan{})
	}
	if approval["recipe_schema"] != recipeSchema {
		return failed(request, preflight, "unsupported_approval", errors.New("approval is not for native archer.project/v1 recipes; resolve again with this worker"), archer.Plan{})
	}
	if err := validatePlatform(workerContext); err != nil {
		return failed(request, preflight, "platform_mismatch", err, archer.Plan{})
	}
	mapped, err := mapRequirements(e.Project, requirements)
	if err != nil {
		return failed(request, preflight, "approval_drift", err, archer.Plan{})
	}
	if err := validateNativeBinding(e, nodes); err != nil {
		code := "approval_drift"
		if errors.Is(err, unsupportedApproval) {
			code = "unsupported_approval"
		}
		return failed(request, preflight, code, err, archer.Plan{})
	}
	if len(requirements) == 0 {
		return resultForPlan(request, preflight, emptyPlan(e), nil, "")
	}
	clone := *e
	clone.SkipLocalVerification = false // Client assertions never prove worker state.
	planRequirements := append([]archer.Requirement{}, mapped...)
	if preflight {
		clone.Project = e.Project
		clone.Project.Registries = nil // No catalog fetch/cache writes.
		for i, requirement := range requirements {
			name, _, _ := parallax.ParseRequirement(requirement)
			node := nodes[name].(parallax.Object)
			if node["driver"] == "binary" {
				// Only inspect this already-reviewed binary route. Its original
				// delivery policy remains frozen in the native definition hash.
				planRequirements[i].Delivery = "local"
			}
		}
	}
	plan, err := clone.Plan(ctx, planRequirements)
	if err != nil {
		return failed(request, preflight, "execution_failed", err, plan)
	}
	if err := validateLiveRoutes(e, requirements, nodes, plan, preflight); err != nil {
		return failed(request, preflight, "approval_drift", err, plan)
	}
	if err := validateNativeBinding(e, nodes); err != nil {
		return failed(request, preflight, "approval_drift", err, plan)
	}
	if preflight {
		return resultForPlan(request, true, plan, nil, "")
	}
	// Install replans. Bind its second plan to the one whose live routes passed
	// the portable checks, rejecting changed routes before artifact publication.
	installed, err := clone.Install(ctx, mapped, plan.ID)
	code := ""
	if err != nil {
		code = "execution_failed"
		if strings.Contains(err.Error(), "plan changed since approval") {
			code = "approval_drift"
		}
	}
	return resultForPlan(request, false, installed, err, code)
}

func objectContext(input parallax.Object) (parallax.Object, error) {
	value, exists := input["context"]
	if !exists {
		return parallax.Object{}, nil
	}
	object, ok := value.(parallax.Object)
	if !ok || object == nil {
		return nil, errors.New("context must be an object")
	}
	if value, exists := object["client_environment"]; exists {
		if child, ok := value.(parallax.Object); !ok || child == nil {
			return nil, errors.New("context.client_environment must be an object")
		}
	}
	return object, nil
}
func mergedContext(value parallax.Object) parallax.Object {
	merged := parallax.Object{}
	if child, ok := value["client_environment"].(parallax.Object); ok {
		for key, item := range child {
			merged[key] = item
		}
	}
	for key, item := range value {
		merged[key] = item
	}
	return merged
}
func validatePlatform(value parallax.Object) error {
	value = mergedContext(value)
	if supplied, exists := value["platform"]; exists {
		platform, ok := supplied.(string)
		if !ok || platform == "" {
			return errors.New("context.platform must be a nonempty string")
		}
		parts := strings.Split(strings.ToLower(platform), "/")
		if len(parts) > 2 {
			return errors.New("unsupported client platform format")
		}
		family := strings.Split(parts[0], ".")[0]
		switch family {
		case "macos", "osx":
			family = "darwin"
		case "win32", "win64":
			family = "windows"
		}
		if family != runtime.GOOS {
			return fmt.Errorf("client platform %q differs from worker host %s/%s", platform, runtime.GOOS, runtime.GOARCH)
		}
		if len(parts) == 2 && architecture(parts[1]) != runtime.GOARCH {
			return fmt.Errorf("client platform architecture %q differs from worker host %s", parts[1], runtime.GOARCH)
		}
	}
	if supplied, exists := value["architecture"]; exists {
		arch, ok := supplied.(string)
		if !ok || architecture(arch) != runtime.GOARCH {
			return fmt.Errorf("client architecture differs from worker host %s", runtime.GOARCH)
		}
	}
	return nil
}
func architecture(value string) string {
	switch strings.ToLower(value) {
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return strings.ToLower(value)
}

func mapRequirements(project archer.Project, originals []string) ([]archer.Requirement, error) {
	policies := map[string]archer.Requirement{}
	for _, r := range project.Requirements {
		if prior, exists := policies[r.Capability]; exists && prior != r {
			return nil, fmt.Errorf("conflicting project policies for capability %s", r.Capability)
		}
		policies[r.Capability] = r
	}
	out := make([]archer.Requirement, 0, len(originals))
	for _, original := range originals {
		name, constraint, err := parallax.ParseRequirement(original)
		if err != nil {
			return nil, err
		}
		r := policies[name]
		r.Capability = name
		if constraint != "" {
			if r.Constraint != "" && strings.Join(strings.Fields(r.Constraint), "") != constraint {
				return nil, fmt.Errorf("requested constraint conflicts with project policy for %s", name)
			}
			r.Constraint = constraint
		}
		out = append(out, r)
	}
	return out, nil
}

func approvalNodes(e *archer.Engine, originals []string, mapped []archer.Requirement, plan archer.Plan) (parallax.Object, error) {
	nodes, groups := parallax.Object{}, map[string][]string{}
	for _, original := range originals {
		name, _, _ := parallax.ParseRequirement(original)
		groups[name] = append(groups[name], original)
	}
	for name, constraints := range groups {
		sort.Strings(constraints)
		definition, err := definitionHash(e, constraints)
		if err != nil {
			return nil, err
		}
		nodes[name] = parallax.Object{"manifest": name, "manifest_sha256": definition, "requires": []string{}, "version_constraint": constraints, "driver": nil, "route_sha256": nil}
	}
	for i, d := range plan.Decisions {
		node := nodes[mapped[i].Capability].(parallax.Object)
		driver, route, err := decisionRoute(e, d)
		if err != nil {
			return nil, err
		}
		if driver != nil {
			if node["driver"] != nil && (node["driver"] != driver || node["route_sha256"] != route) {
				return nil, fmt.Errorf("requirements for %s select incompatible native routes", mapped[i].Capability)
			}
			node["driver"], node["route_sha256"] = driver, route
		}
	}
	return nodes, nil
}

// Native bindings retain additive project fields, effective executor inputs,
// and source identities, excluding host paths, learned revision and probe output.
func definitionHash(e *archer.Engine, originals []string) (string, error) {
	projectObject := any(e.Project)
	file, err := os.Open(filepath.Join(e.ProjectDir, "archer.json"))
	if err == nil {
		projectObject, err = parallax.Decode(file)
		closeErr := file.Close()
		if err != nil {
			return "", fmt.Errorf("read native project definition: %w", err)
		}
		if closeErr != nil {
			return "", closeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	mapped, err := mapRequirements(e.Project, originals)
	if err != nil {
		return "", err
	}
	semantics := map[string]archer.Requirement{}
	for i, original := range originals {
		semantics[original] = mapped[i]
	}
	sources := parallax.Object{}
	for _, name := range []string{"composer.json", "composer.lock", "package.json", "package-lock.json", "npm-shrinkwrap.json"} {
		hash, exists, err := sourceHash(filepath.Join(e.ProjectDir, name))
		if err != nil {
			return "", err
		}
		if exists {
			sources[name] = hash
		}
	}
	return parallax.Hash(parallax.Object{"project": projectObject, "effective_project": e.Project, "effective_registries": append([]string{}, e.Project.Registries...), "requirement_semantics": semantics, "source_hashes": sources})
}
func sourceHash(path string) (string, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("source file %s must be a regular file", filepath.Base(path))
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxSourceBytes+1))
	if err != nil {
		return "", false, err
	}
	if n > maxSourceBytes {
		return "", false, fmt.Errorf("source file %s exceeds 16 MiB", filepath.Base(path))
	}
	return hex.EncodeToString(hash.Sum(nil)), true, nil
}

func validateNativeBinding(e *archer.Engine, nodes parallax.Object) error {
	for name, value := range nodes {
		node := value.(parallax.Object)
		dependencies, _ := parallax.StringList(node["requires"], "requires")
		if len(dependencies) != 0 {
			return fmt.Errorf("%w: dependency execution for %s", unsupportedApproval, name)
		}
		constraints, _ := parallax.StringList(node["version_constraint"], "version_constraint")
		definition, err := definitionHash(e, constraints)
		if err != nil {
			return err
		}
		if node["manifest"] != name || node["manifest_sha256"] != definition {
			return fmt.Errorf("native recipe definition or source changed for %s", name)
		}
		if node["driver"] == nil {
			if node["route_sha256"] != nil {
				return fmt.Errorf("native route binding is malformed for %s", name)
			}
			continue
		}
		if !containsRoute(e, name, node["driver"], node["route_sha256"]) {
			return fmt.Errorf("approved native route changed or disappeared for %s", name)
		}
	}
	return nil
}
func containsRoute(e *archer.Engine, capability string, driver, digest any) bool {
	match := func(values []string, route any) bool {
		for _, name := range values {
			if name == capability {
				hash, err := parallax.Hash(route)
				return err == nil && digest == hash
			}
		}
		return false
	}
	switch driver {
	case "service":
		for _, s := range e.Project.Services {
			if match(s.Provides, s) {
				return true
			}
		}
	case "local":
		for _, l := range e.Project.Local {
			if match(l.Provides, l) {
				return true
			}
		}
	case "binary":
		for _, r := range e.Project.Recipes {
			if match(r.Provides, r) {
				return true
			}
		}
	}
	return false
}
func decisionRoute(e *archer.Engine, d archer.Decision) (any, any, error) {
	var driver string
	var route any
	switch d.Status {
	case "service":
		driver = "service"
	case "local":
		driver = "local"
	case "install", "installed":
		driver = "binary"
	case "unresolved":
		// Failed readiness of a configured service remains that bound route.
		for _, s := range e.Project.Services {
			if s.ID == d.Provider {
				for _, capability := range s.Provides {
					if capability == d.Requirement.Capability {
						driver, route = "service", s
					}
				}
			}
		}
	}
	if driver == "service" && route == nil {
		for _, s := range e.Project.Services {
			if s.ID == d.Provider {
				route = s
			}
		}
	}
	if driver == "local" {
		for _, l := range e.Project.Local {
			if l.ID == d.Provider {
				route = l
			}
		}
	}
	if driver == "binary" {
		for _, r := range e.Project.Recipes {
			if r.ID == d.Provider {
				route = r
			}
		}
	}
	if route == nil {
		return nil, nil, nil
	}
	hash, err := parallax.Hash(route)
	return driver, hash, err
}
func validateLiveRoutes(e *archer.Engine, originals []string, nodes parallax.Object, plan archer.Plan, preflight bool) error {
	if len(plan.Decisions) != len(originals) {
		return errors.New("native plan omitted requested work")
	}
	for i, original := range originals {
		d := plan.Decisions[i]
		if verified(d) {
			continue
		} // Verified state skips installation, never selects a new installer.
		name, _, _ := parallax.ParseRequirement(original)
		node := nodes[name].(parallax.Object)
		driver, route, err := decisionRoute(e, d)
		if err != nil {
			return err
		}
		if driver == node["driver"] && route == node["route_sha256"] {
			continue
		}
		if preflight && d.Status == "unresolved" {
			continue
		} // Suppressed discovery is pending, not execution.
		return fmt.Errorf("execution would select an unapproved or unavailable route for %s", original)
	}
	return nil
}

func failed(request parallax.Object, preflight bool, code string, err error, plan archer.Plan) (parallax.Object, error) {
	return resultForPlan(request, preflight, plan, err, code)
}
func resultForPlan(request parallax.Object, preflight bool, plan archer.Plan, failure error, code string) (parallax.Object, error) {
	if code == "approval_drift" || code == "unsupported_approval" {
		plan = archer.Plan{}
	}
	originals, _ := parallax.StringList(request["requires"], "requires")
	satisfied, remaining := []string{}, []string{}
	evidence, artifacts := []parallax.Object{}, []parallax.Object{}
	for i, original := range originals {
		if i < len(plan.Decisions) && verified(plan.Decisions[i]) {
			d := plan.Decisions[i]
			satisfied = append(satisfied, original)
			evidence = append(evidence, verification(original, d))
			if d.Status == "installed" {
				artifacts = append(artifacts, parallax.Object{"capability": original, "path": d.Path, "provider": d.Provider, "evidence": d.Evidence})
			}
		} else {
			remaining = append(remaining, original)
		}
	}
	status := coverageStatus(len(satisfied), len(remaining))
	if failure != nil {
		status = "failed"
	}
	next, _ := request["from"].(string)
	if next == "archer" || next == "" {
		next = "slipstream"
	}
	result := parallax.Object{
		"schema": "parallax.handoff-result/v1", "handoff_id": request["handoff_id"], "owner": "archer",
		"status": status, "satisfied": satisfied, "remaining": remaining, "artifacts": artifacts,
		"next_owner": next, "evidence": evidence, "operation": "install", "ai_requests": 0,
	}
	if preflight {
		result["preflight"] = true
	}
	if failure != nil {
		result["failure_code"], result["failure_reason"] = code, failure.Error()
	}
	preserveExtensions(result, request, []string{"schema", "handoff_id", "mission_id", "step_id", "parent_step_id", "from", "to", "owner", "objective", "requires", "context_refs", "attempt"})
	if err := parallax.ValidateResult(result, request, nil); err != nil {
		return nil, fmt.Errorf("invalid worker result: %w", err)
	}
	return result, failure
}
func preserveExtensions(result, input parallax.Object, known []string) {
	exclude := map[string]bool{}
	for _, key := range known {
		exclude[key] = true
	}
	extensions := parallax.Object{}
	for key, value := range input {
		if !exclude[key] {
			extensions[key] = value
		}
	}
	if len(extensions) != 0 {
		result["request_extensions"] = extensions
	}
}
func emptyPlan(e *archer.Engine) archer.Plan {
	return archer.Plan{Schema: "archer.plan/v1", Scope: e.Project.Scope, Platform: runtime.GOOS + "/" + runtime.GOARCH, Decisions: []archer.Decision{}, Remaining: []archer.Requirement{}}
}
func verified(d archer.Decision) bool {
	return d.Status == "service" || d.Status == "local" || d.Status == "installed"
}
func verification(original string, d archer.Decision) parallax.Object {
	return parallax.Object{"type": "verification", "capability": original, "status": "ok", "provider": d.Provider, "details": d.Evidence}
}
func coverageStatus(covered, remaining int) string {
	if remaining == 0 {
		return "satisfied"
	}
	if covered == 0 {
		return "unsatisfied"
	}
	return "partial"
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func sortedKeys(object parallax.Object) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
