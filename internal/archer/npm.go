package archer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/npm"
	"github.com/Parallax-Web-Services/archer/internal/runtimes"
)

// Keep distinct constraints/delivery modes distinct even for one package name.
func npmDecisionKey(requirement Requirement) string {
	return requirement.Capability + "\x00" + requirement.Delivery + "\x00" + requirement.Constraint
}

// npmPlan selects only explicit root package requirements. Selecting one library
// replays its entire producer-frozen npm tree; it never substitutes a service,
// solves ranges independently, or executes package code to prove compatibility.
func (e *Engine) npmPlan(requirements []Requirement, runtimeSelection ...*runtimes.Plan) (*npm.Plan, map[string]Decision, error) {
	var node *runtimes.Plan
	if len(runtimeSelection) != 0 {
		node = runtimeSelection[0]
	}
	decisions := map[string]Decision{}
	for _, requirement := range requirements {
		if !strings.HasPrefix(requirement.Capability, "npm:") {
			continue
		}
		decisions[npmDecisionKey(requirement)] = Decision{Requirement: requirement, Status: "unresolved", Reason: "only directly declared package.json dependencies can be selected for frozen npm replay", Discovery: "not_applicable"}
	}
	if len(decisions) == 0 {
		return nil, decisions, nil
	}
	target, err := npm.CurrentPlatform()
	if err != nil {
		return nil, decisions, fmt.Errorf("detect npm target: %w", err)
	}
	plan, err := npm.Load(e.ProjectDir, target)
	if err != nil {
		return nil, decisions, fmt.Errorf("load frozen npm tree: %w", err)
	}
	if node != nil {
		if err := runtimes.CheckNode(node.Version, plan.NodeConstraints); err != nil {
			return nil, decisions, fmt.Errorf("Node runtime readiness: %w", err)
		}
	}
	selected := map[string]npm.Package{}
	skipped := map[string]npm.Package{}
	for _, pkg := range plan.Packages {
		selected[pkg.Name+"\x00"+pkg.Path] = pkg
	}
	for _, pkg := range plan.Skipped {
		skipped[pkg.Name+"\x00"+pkg.Path] = pkg
	}
	active := false
	for _, requirement := range requirements {
		if !strings.HasPrefix(requirement.Capability, "npm:") {
			continue
		}
		key := npmDecisionKey(requirement)
		decision := decisions[key]
		name := strings.TrimPrefix(requirement.Capability, "npm:")
		constraint, declared := plan.Direct[name]
		if !declared {
			continue
		}
		if node != nil && node.Status == "unresolved" {
			decision.Reason = "Node runtime readiness blocked: " + node.Reason
			decisions[key] = decision
			continue
		}
		if requirement.Delivery == "service" {
			decision.Reason = "npm lock replay provides a project library and cannot fulfill service delivery"
			decisions[key] = decision
			continue
		}
		if requirement.Constraint != "" && requirement.Constraint != constraint {
			decision.Reason = fmt.Sprintf("requested npm constraint %q differs from frozen package.json declaration %q", requirement.Constraint, constraint)
			decisions[key] = decision
			continue
		}
		path := "node_modules/" + name
		decision.Provider = "npm-lock-v3"
		if pkg, excluded := skipped[name+"\x00"+path]; excluded {
			decision.Status = "npm_skipped"
			decision.Reason = fmt.Sprintf("optional npm package %s@%s is excluded on %s/%s; this requirement remains unsatisfied", name, pkg.Version, target.OS, target.Arch)
			decisions[key] = decision
			continue
		}
		pkg, present := selected[name+"\x00"+path]
		if !present {
			return nil, decisions, fmt.Errorf("direct npm package %s has no selected or skipped frozen placement", name)
		}
		active = true
		decision.Status = "npm_install"
		decision.Path = filepath.Join(e.ProjectDir, filepath.FromSlash(path))
		decision.Reason = fmt.Sprintf("replay the full frozen npm tree (%d packages) for %s@%s; ranges are not independently solved and runtime compatibility is untested", len(plan.Packages), name, pkg.Version)
		if node != nil {
			decision.Reason = fmt.Sprintf("replay the full frozen npm tree (%d packages) for %s@%s with pinned Node %s; scripts are not executed", len(plan.Packages), name, pkg.Version, node.Version)
		}
		decisions[key] = decision
	}
	if !active {
		return nil, decisions, nil
	}
	if !e.SkipLocalVerification {
		verified, err := npm.Verify(e.ProjectDir, plan)
		if err != nil {
			return nil, decisions, fmt.Errorf("verify frozen npm tree: %w", err)
		}
		if verified {
			for key, decision := range decisions {
				if decision.Status != "npm_install" {
					continue
				}
				decision.Status = "npm_installed"
				decision.Reason = fmt.Sprintf("reuse the freshly verified full frozen npm tree (%d packages); runtime compatibility is untested", len(plan.Packages))
				if node != nil {
					decision.Reason = fmt.Sprintf("reuse the freshly verified full frozen npm tree (%d packages); locked engines match pinned Node %s", len(plan.Packages), node.Version)
				}
				decision.Evidence = "npm-plan:" + plan.ID
				decision.Cached = true
				decisions[key] = decision
			}
		}
	}
	return plan, decisions, nil
}

func (e *Engine) installNPM(ctx context.Context, plan *npm.Plan, selected ...*runtimes.Plan) (npm.Report, error) {
	if len(selected) != 0 && selected[0] != nil {
		if !runtimeReady(selected[0]) {
			return npm.Report{}, fmt.Errorf("Node runtime is not prepared")
		}
		if err := runtimes.CheckNode(selected[0].Version, plan.NodeConstraints); err != nil {
			return npm.Report{}, err
		}
	}
	if err := plan.Validate(); err != nil {
		return npm.Report{}, err
	}
	target, err := npm.CurrentPlatform()
	if err != nil {
		return npm.Report{}, err
	}
	if plan.Platform != target {
		return npm.Report{}, fmt.Errorf("npm execution host %s/%s differs from the frozen target %s/%s", target.OS, target.Arch, plan.Platform.OS, plan.Platform.Arch)
	}
	return npm.Install(ctx, e.ProjectDir, plan, npm.Options{Home: e.Home, Offline: e.Offline})
}
