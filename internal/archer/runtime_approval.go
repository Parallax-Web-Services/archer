package archer

import (
	"encoding/json"
	"fmt"

	"github.com/Parallax-Web-Services/archer/internal/runtimes"
)

// Runtime preparation may establish previously pending readiness. It cannot
// authorize a different installer for other capabilities in a reviewed plan.
func compareRuntimeCompletion(approved, fresh Plan) error {
	if approved.Schema != fresh.Schema || approved.Scope != fresh.Scope || approved.Platform != fresh.Platform || len(approved.Decisions) != len(fresh.Decisions) {
		return fmt.Errorf("plan changed after runtime preparation; review a fresh plan")
	}
	if fresh.NPM != nil && (approved.NPM == nil || approved.NPM.ID != fresh.NPM.ID) {
		return fmt.Errorf("npm installation route changed after runtime preparation; review a fresh plan")
	}
	if fresh.Composer != nil && (approved.Composer == nil || approved.Composer.ID != fresh.Composer.ID) {
		return fmt.Errorf("Composer installation route changed after runtime preparation; review a fresh plan")
	}
	if err := comparePreparedRuntimeRoutes(approved.Runtimes, fresh.Runtimes); err != nil {
		return err
	}
	for i, current := range fresh.Decisions {
		previous := approved.Decisions[i]
		if previous.Requirement != current.Requirement {
			return fmt.Errorf("requested capability changed after runtime preparation; review a fresh plan")
		}
		// Missing readiness returns remaining work without selecting an effect.
		if current.Status == "unresolved" || current.Status == "npm_skipped" || current.Status == "composer_platform_pending" {
			continue
		}
		if previous.Provider != current.Provider || previous.Path != current.Path {
			return fmt.Errorf("route for %s changed after runtime preparation; review a fresh plan", current.Requirement.Capability)
		}
		allowed := previous.Status == current.Status
		switch current.Status {
		case "install", "npm_install", "composer_install":
			// Even an approved reuse route does not authorize restoring it by
			// installation if the environment changed during preparation.
			allowed = previous.Status == current.Status
		case "installed":
			allowed = allowed || previous.Status == "install"
		case "npm_installed":
			allowed = allowed || previous.Status == "npm_install"
		case "composer_installed":
			allowed = allowed || previous.Status == "composer_install"
		case "composer_platform":
			allowed = allowed || previous.Status == "composer_platform_pending"
		case "runtime_managed":
			allowed = allowed || previous.Status == "runtime_install"
		case "runtime_install":
			allowed = false // Preparation must not introduce another runtime effect.
		}
		if !allowed {
			return fmt.Errorf("effect for %s changed from %s to %s after runtime preparation; review a fresh plan", current.Requirement.Capability, previous.Status, current.Status)
		}
	}
	return nil
}

func comparePreparedRuntimeRoutes(approved, fresh []runtimes.Plan) error {
	previous := map[string]runtimes.Plan{}
	for _, selected := range approved {
		if _, duplicate := previous[selected.Kind]; duplicate {
			return fmt.Errorf("approved runtime routes are ambiguous")
		}
		previous[selected.Kind] = selected
	}
	if len(previous) != len(fresh) {
		return fmt.Errorf("runtime choices changed after preparation; review a fresh plan")
	}
	seen := map[string]bool{}
	for _, selected := range fresh {
		old, exists := previous[selected.Kind]
		if !exists || seen[selected.Kind] || old.ID != selected.ID || old.Version != selected.Version || old.Platform != selected.Platform || !sameRuntimeValue(old.Spec, selected.Spec) {
			return fmt.Errorf("runtime %s choice changed after preparation; review a fresh plan", selected.Kind)
		}
		seen[selected.Kind] = true
		if selected.Status == "unresolved" {
			continue
		}
		if old.Path != selected.Path || old.BinDir != selected.BinDir {
			return fmt.Errorf("runtime %s path changed after preparation; review a fresh plan", selected.Kind)
		}
		switch selected.Status {
		case "managed":
			if old.Status != "install" && old.Status != "managed" {
				return fmt.Errorf("runtime %s installer changed after preparation; review a fresh plan", selected.Kind)
			}
			if old.Status == "managed" && (old.Evidence != selected.Evidence || !sameRuntimeValue(old.Environment, selected.Environment)) {
				return fmt.Errorf("managed runtime %s evidence changed after preparation; review a fresh plan", selected.Kind)
			}
		case "local":
			if old.Status != "local" || old.Evidence != selected.Evidence || !sameRuntimeValue(old.Environment, selected.Environment) {
				return fmt.Errorf("local runtime %s changed after preparation; review a fresh plan", selected.Kind)
			}
		default:
			return fmt.Errorf("runtime %s is not ready after preparation; review a fresh plan", selected.Kind)
		}
	}
	return nil
}

func sameRuntimeValue(left, right any) bool {
	first, firstErr := json.Marshal(left)
	second, secondErr := json.Marshal(right)
	return firstErr == nil && secondErr == nil && string(first) == string(second)
}
