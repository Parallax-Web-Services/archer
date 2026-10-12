package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/archer"
	"github.com/Parallax-Web-Services/archer/internal/parallax"
	"github.com/Parallax-Web-Services/archer/internal/runtimes"
)

func TestRuntimeWorkRejectedBeforeWorkerPlanningOrEffects(t *testing.T) {
	for _, scenario := range []struct {
		name, capability, runtime string
	}{
		{"explicit PHP", "runtime:php", ""},
		{"explicit Node", "runtime:node", ""},
		{"unknown runtime", "runtime:unknown", ""},
		{"configured Composer dependency", "composer:library", "php"},
		{"configured npm dependency", "npm:library", "node"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"schema":"archer.catalog/v1","services":[]}`)
			}))
			defer server.Close()
			p := project()
			p.Requirements[0].Delivery = ""
			p.Requirements = append(p.Requirements, archer.Requirement{Capability: scenario.capability})
			probe, marker := proofProbe(t, "ready")
			p.Local = []archer.LocalCapability{probe}
			p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("must not publish an allowed subset"))}
			p.Registries = []string{server.URL}
			if scenario.runtime != "" {
				p.Runtimes = map[string]runtimes.Spec{scenario.runtime: {Version: "1.2.3", Binary: filepath.Join(t.TempDir(), "missing-runtime")}}
			}
			e := engine(t, p, nil)
			// Unsupported worker work must be rejected even before package parsing.
			for _, manifest := range []string{"composer.json", "package.json"} {
				if err := os.WriteFile(filepath.Join(e.ProjectDir, manifest), []byte("invalid source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			requires := []string{"tool", scenario.capability}
			// The native guard remains necessary if a later transport can map
			// these local names. Current Parallax v1 rejects ':' before mapping.
			if err := rejectRuntimeEffects(p, []archer.Requirement{{Capability: "tool"}, {Capability: scenario.capability}}); !errors.Is(err, unsupportedApproval) || !strings.Contains(err.Error(), "local CLI only") {
				t.Fatalf("native runtime effects were accepted: %v", err)
			}
			for _, suppressed := range []bool{false, true} {
				result, err := Resolve(context.Background(), e, parallax.Object{"requirements": requires, "context": parallax.Object{"probe_installed": !suppressed}})
				if err == nil || result != nil {
					t.Fatalf("unsupported runtime planning was accepted: %v %#v", err, result)
				}
			}
			request := handoff(requires, parallax.Object{})
			for _, preflight := range []bool{false, true} {
				result, err := Execute(context.Background(), e, request, preflight)
				if err == nil || result != nil {
					t.Fatalf("runtime effects accepted: %v %#v", err, result)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("unsupported runtime selection fetched a catalog")
			}
			noMarker(t, marker)
			checkNoHome(t, e)
		})
	}
}

func TestConfiguredRuntimesDoNotBlockUnrelatedWorkerSelection(t *testing.T) {
	p := project()
	p.Requirements = append(p.Requirements, archer.Requirement{Capability: "npm:library"}, archer.Requirement{Capability: "composer:library"})
	p.Runtimes = map[string]runtimes.Spec{
		"php":  {Version: "1.2.3", Binary: filepath.Join(t.TempDir(), "missing-php")},
		"node": {Version: "1.2.3", Binary: filepath.Join(t.TempDir(), "missing-node")},
	}
	p.Recipes = []archer.Recipe{artifact(t, "tool", []byte("unrelated approved artifact"))}
	e := engine(t, p, nil)
	resolved := resolve(t, e, []string{"tool"}, parallax.Object{"probe_installed": false})
	if !reflect.DeepEqual(list(t, resolved, "installable"), []string{"tool"}) {
		t.Fatalf("unrelated route was blocked by runtime configuration: %#v", resolved)
	}
	checkNoHome(t, e)
	result, err := Execute(context.Background(), e, handoff([]string{"tool"}, resolved["approval"].(parallax.Object)), false)
	if err != nil || result["status"] != "satisfied" || !reflect.DeepEqual(list(t, result, "satisfied"), []string{"tool"}) {
		t.Fatalf("unrelated route failed: %v %#v", err, result)
	}
	if _, err := os.Stat(filepath.Join(e.Home, "projects")); err != nil {
		t.Fatal("approved raw artifact was not published", err)
	}
}
