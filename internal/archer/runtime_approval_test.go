package archer

import (
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/composer"
	"github.com/Parallax-Web-Services/archer/internal/npm"
	"github.com/Parallax-Web-Services/archer/internal/runtimes"
)

func runtimeApprovalFixture() Plan {
	return Plan{
		Schema: "archer.plan/v1", Scope: "reviewed", Platform: "linux/amd64",
		Decisions: []Decision{{Requirement: Requirement{Capability: "tool", Delivery: "local"}, Status: "local", Provider: "existing-tool", Path: "/tools/tool"}},
		Runtimes:  []runtimes.Plan{{ID: "pinned-node", Kind: "node", Version: "1.2.3", Platform: "linux/amd64", Status: "install", Path: "/managed/node/bin/node", BinDir: "/managed/node/bin", Spec: runtimes.Spec{Version: "1.2.3"}}},
	}
}

func preparedRuntimeFixture(approved Plan) Plan {
	fresh := approved
	fresh.Decisions = append([]Decision(nil), approved.Decisions...)
	fresh.Runtimes = append([]runtimes.Plan(nil), approved.Runtimes...)
	for i := range fresh.Runtimes {
		if fresh.Runtimes[i].Status == "install" {
			fresh.Runtimes[i].Status = "managed"
			fresh.Runtimes[i].Evidence = "sha256:verified"
		}
	}
	return fresh
}

func TestRuntimeCompletionCannotAddRawInstallerWhenLocalCapabilityDisappears(t *testing.T) {
	approved := runtimeApprovalFixture()
	fresh := preparedRuntimeFixture(approved)
	fresh.Decisions[0].Status = "install"
	fresh.Decisions[0].Provider = "pinned-alternate"
	fresh.Decisions[0].Path = "/managed/tool"
	if err := compareRuntimeCompletion(approved, fresh); err == nil {
		t.Fatal("runtime completion authorized an unreviewed alternate raw installer")
	}
}

func TestRuntimeCompletionAllowsOnlyReviewedEffectTransitions(t *testing.T) {
	for _, scenario := range []struct {
		before, after string
		allowed       bool
	}{
		{"local", "local", true},
		{"local", "install", false},
		{"service", "install", false},
		{"unresolved", "install", false},
		{"installed", "install", false},
		{"install", "install", true},
		{"install", "installed", true},
		{"local", "unresolved", true},
		{"service", "unresolved", true},
		{"npm_install", "npm_install", true},
		{"npm_install", "npm_installed", true},
		{"npm_installed", "npm_install", false},
		{"unresolved", "npm_install", false},
		{"composer_install", "composer_install", true},
		{"composer_install", "composer_installed", true},
		{"composer_installed", "composer_install", false},
		{"composer_platform_pending", "composer_platform", true},
		{"runtime_install", "runtime_managed", true},
		{"runtime_local", "runtime_managed", false},
		{"runtime_install", "runtime_install", false},
	} {
		t.Run(scenario.before+" to "+scenario.after, func(t *testing.T) {
			approved := runtimeApprovalFixture()
			approved.Decisions[0].Status = scenario.before
			fresh := preparedRuntimeFixture(approved)
			fresh.Decisions[0].Status = scenario.after
			if allowed := compareRuntimeCompletion(approved, fresh) == nil; allowed != scenario.allowed {
				t.Fatalf("allowed=%v, expected %v", allowed, scenario.allowed)
			}
		})
	}
}

func TestRuntimeCompletionRejectsChangedRuntimeAndPackageChoices(t *testing.T) {
	for _, scenario := range []string{"provider", "path", "requirement", "runtime source", "runtime path", "runtime install", "runtime local", "new npm graph", "changed npm graph", "new Composer graph", "changed Composer graph", "local runtime evidence"} {
		t.Run(scenario, func(t *testing.T) {
			approved := runtimeApprovalFixture()
			if scenario == "changed npm graph" {
				approved.NPM = &npm.Plan{ID: "old-npm"}
			}
			if scenario == "changed Composer graph" {
				approved.Composer = &composer.Plan{ID: "old-composer"}
			}
			if scenario == "local runtime evidence" {
				approved.Runtimes[0].Status = "local"
				approved.Runtimes[0].Evidence = "old-binary"
			}
			fresh := preparedRuntimeFixture(approved)
			switch scenario {
			case "provider":
				fresh.Decisions[0].Provider = "another-provider"
			case "path":
				fresh.Decisions[0].Path = "/other/tool"
			case "requirement":
				fresh.Decisions[0].Requirement.Capability = "other"
			case "runtime source":
				fresh.Runtimes[0].Spec.Version = "1.2.4"
			case "runtime path":
				fresh.Runtimes[0].Path = "/another/node"
			case "runtime install":
				fresh.Runtimes[0].Status = "install"
			case "runtime local":
				fresh.Runtimes[0].Status = "local"
			case "new npm graph", "changed npm graph":
				fresh.NPM = &npm.Plan{ID: "new-npm"}
			case "new Composer graph", "changed Composer graph":
				fresh.Composer = &composer.Plan{ID: "new-composer"}
			case "local runtime evidence":
				fresh.Runtimes[0].Evidence = "new-binary"
			}
			if err := compareRuntimeCompletion(approved, fresh); err == nil {
				t.Fatal("accepted changed reviewed choice")
			}
		})
	}
}

func TestRuntimeCompletionKeepsUnavailableWorkPending(t *testing.T) {
	approved := runtimeApprovalFixture()
	fresh := preparedRuntimeFixture(approved)
	fresh.Runtimes[0].Status = "unresolved"
	fresh.Runtimes[0].Path = ""
	fresh.Decisions[0].Status = "unresolved"
	fresh.Decisions[0].Provider, fresh.Decisions[0].Path = "", ""
	if err := compareRuntimeCompletion(approved, fresh); err != nil {
		t.Fatal("missing readiness introduced no new effects", err)
	}
}
