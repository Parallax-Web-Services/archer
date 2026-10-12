package archer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/composer"
	"github.com/Parallax-Web-Services/archer/internal/npm"
	"github.com/Parallax-Web-Services/archer/internal/runtimes"
)

func (e *Engine) runtimeOptions() runtimes.Options {
	return runtimes.Options{Home: e.Home, ScopeKey: e.scopeKey, ProjectDir: e.ProjectDir, Offline: e.Offline, SkipLocalVerification: e.SkipLocalVerification}
}

// A selector for unrelated service work must not install the project's runtimes.
// Default project preparation includes every explicitly declared runtime.
func (e *Engine) runtimePlans(ctx context.Context, requirements []Requirement, all bool) ([]runtimes.Plan, map[string]*runtimes.Plan, error) {
	names := map[string]bool{}
	if all {
		for name := range e.Project.Runtimes {
			names[name] = true
		}
	}
	for _, requirement := range requirements {
		switch {
		case strings.HasPrefix(requirement.Capability, "runtime:"):
			names[strings.TrimPrefix(requirement.Capability, "runtime:")] = true
		case strings.HasPrefix(requirement.Capability, "composer:"):
			if _, exists := e.Project.Runtimes["php"]; exists {
				names["php"] = true
			}
		case strings.HasPrefix(requirement.Capability, "npm:"):
			if _, exists := e.Project.Runtimes["node"]; exists {
				names["node"] = true
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	plans := make([]runtimes.Plan, 0, len(ordered))
	for _, name := range ordered {
		spec, exists := e.Project.Runtimes[name]
		if !exists {
			continue // An undeclared explicit runtime selector remains unresolved.
		}
		if name == "php" && spec.Binary == "" && e.Project.Composer != nil {
			spec.Binary = e.Project.Composer.PHP
		}
		selected, err := runtimes.Inspect(ctx, name, spec, e.runtimeOptions())
		if err != nil {
			return nil, nil, fmt.Errorf("inspect %s runtime: %w", name, err)
		}
		if name == "php" && selected.Status == "local" {
			needsPHPGraph := false
			for _, requirement := range requirements {
				needsPHPGraph = needsPHPGraph || strings.HasPrefix(requirement.Capability, "composer:")
			}
			if all {
				if _, err := os.Stat(filepath.Join(e.ProjectDir, "composer.lock")); err == nil {
					needsPHPGraph = true
				} else if !os.IsNotExist(err) {
					return nil, nil, err
				}
			}
			if needsPHPGraph {
				config := ComposerConfig{}
				if e.Project.Composer != nil {
					config = *e.Project.Composer
				}
				if graph, loadErr := composer.Load(e.ProjectDir, config.SourcePolicy); loadErr == nil {
					info, readiness := e.phpRuntimeEvidence(ctx, config.PHP, &selected)
					if readiness == nil {
						readiness = composer.CheckRuntime(graph, info)
					}
					if readiness != nil && len(spec.Artifacts) > 0 {
						opts := e.runtimeOptions()
						opts.ExcludeLocal = true
						selected, err = runtimes.Inspect(ctx, name, spec, opts)
						if err != nil {
							return nil, nil, err
						}
					}
				}
			}
		}
		plans = append(plans, selected)
	}
	byKind := map[string]*runtimes.Plan{}
	for i := range plans {
		byKind[plans[i].Kind] = &plans[i]
	}
	return plans, byKind, nil
}

func checkPHPVersion(plan *composer.Plan, version string) error {
	for _, requirement := range plan.PlatformRequirements {
		if requirement.Name != "php" {
			continue
		}
		ok, err := composer.Satisfies(version, requirement.Constraint)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s requires PHP %s; pinned runtime is %s", requirement.From, requirement.Constraint, version)
		}
	}
	for _, conflict := range plan.PlatformConflicts {
		if conflict.Name != "php" {
			continue
		}
		ok, err := composer.Satisfies(version, conflict.Constraint)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%s conflicts with pinned PHP %s", conflict.From, version)
		}
	}
	return nil
}

type runtimeIdentity struct {
	ID, Kind, Version, Platform, Status, Path, Evidence string
	Environment                                         map[string]string
}

func runtimeIdentities(plans []runtimes.Plan) []runtimeIdentity {
	identities := []runtimeIdentity{}
	for _, plan := range plans {
		identities = append(identities, runtimeIdentity{plan.ID, plan.Kind, plan.Version, plan.Platform, plan.Status, plan.Path, plan.Evidence, plan.Environment})
	}
	return identities
}

func (e *Engine) prepareRuntime(ctx context.Context, plan runtimes.Plan) (runtimes.Plan, error) {
	if e.SkipLocalVerification {
		return plan, fmt.Errorf("runtime preparation requires verification on the execution host")
	}
	return runtimes.Install(ctx, plan, e.runtimeOptions())
}

func (e *Engine) checkSourceSnapshot(expected map[string]string) error {
	current, err := e.sourceHashes()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(expected)
	after, _ := json.Marshal(current)
	if !bytes.Equal(before, after) {
		return fmt.Errorf("project sources changed during verification; review a fresh plan")
	}
	return nil
}

func runtimeReady(plan *runtimes.Plan) bool {
	return plan != nil && (plan.Status == "local" || plan.Status == "managed")
}

func runtimeRequirement(plan runtimes.Plan) Requirement {
	return Requirement{Capability: "runtime:" + plan.Kind, Delivery: "local", Constraint: plan.Version}
}

func runtimeDecision(requirement Requirement, selected *runtimes.Plan) Decision {
	d := Decision{Requirement: requirement, Status: "unresolved", Reason: "declare an exact pinned runtime in archer.json runtimes", Discovery: "local"}
	if selected == nil {
		return d
	}
	d.Provider, d.Path, d.Cached = "runtime:"+selected.Kind, selected.Path, selected.Cached
	d.Reason, d.Evidence = selected.Reason, selected.Evidence
	if requirement.Delivery == "service" || requirement.Delivery == "package" {
		d.Reason = "a runtime interpreter requires local delivery"
		return d
	}
	if requirement.Constraint != "" && requirement.Constraint != selected.Version {
		d.Reason = "runtime constraint differs from its exact configured version"
		return d
	}
	if selected.Status != "unresolved" {
		d.Status = "runtime_" + selected.Status
	}
	return d
}

func selectedRuntime(plans []runtimes.Plan, kind string) *runtimes.Plan {
	for i := range plans {
		if plans[i].Kind == kind {
			return &plans[i]
		}
	}
	return nil
}

func runtimeEnvironment(base []string, values map[string]string) []string {
	keys := map[string]string{}
	for key, value := range values {
		canonical := key
		if runtime.GOOS == "windows" {
			canonical = strings.ToUpper(key)
		}
		keys[canonical] = value
	}
	out := make([]string, 0, len(base)+len(keys))
	for _, entry := range base {
		key, _, exists := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if _, replace := keys[key]; !exists || !replace {
			out = append(out, entry)
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		out = append(out, key+"="+keys[key])
	}
	return out
}

func (e *Engine) phpRuntimeEvidence(ctx context.Context, configured string, selected *runtimes.Plan) (composer.RuntimeInfo, error) {
	if selected == nil {
		return composer.ProbeRuntime(ctx, configured)
	}
	if !runtimeReady(selected) {
		return composer.RuntimeInfo{}, fmt.Errorf("PHP runtime is not prepared; run archer i")
	}
	info, err := composer.ProbeRuntimeWithEnv(ctx, selected.Path, runtimeEnvironment(os.Environ(), selected.Environment))
	if err != nil {
		return info, err
	}
	if err := runtimes.VerifySelection(ctx, *selected, e.runtimeOptions()); err != nil {
		return composer.RuntimeInfo{}, fmt.Errorf("PHP runtime changed during graph verification: %w", err)
	}
	return info, nil
}

// Run executes an explicit argv in the project directory. It neither installs
// dependencies nor evaluates a shell expression or package lifecycle script.
func (e *Engine) Run(ctx context.Context, argv []string, input io.Reader, output, diagnostics io.Writer) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 1, fmt.Errorf("run requires a command after --")
	}
	for _, value := range argv {
		if strings.ContainsRune(value, '\x00') {
			return 1, fmt.Errorf("run command contains a NUL byte")
		}
	}
	if e.SkipLocalVerification {
		return 1, fmt.Errorf("run requires fresh verification on the execution host")
	}
	sources, err := e.sourceHashes()
	if err != nil {
		return 1, err
	}
	plans, byKind, err := e.runtimePlans(ctx, nil, true)
	if err != nil {
		return 1, err
	}
	for i := range plans {
		if !runtimeReady(&plans[i]) {
			return 2, fmt.Errorf("%s runtime is not prepared: %s; run archer i", plans[i].Kind, plans[i].Reason)
		}
	}
	if selected := byKind["php"]; selected != nil {
		if _, err := os.Stat(filepath.Join(e.ProjectDir, "composer.lock")); err == nil {
			config := ComposerConfig{}
			if e.Project.Composer != nil {
				config = *e.Project.Composer
			}
			plan, err := composer.Load(e.ProjectDir, config.SourcePolicy)
			if err != nil {
				return 1, fmt.Errorf("check frozen PHP runtime requirements: %w", err)
			}
			info, err := e.phpRuntimeEvidence(ctx, config.PHP, selected)
			if err != nil {
				return 1, err
			}
			if err := composer.CheckRuntime(plan, info); err != nil {
				return 2, err
			}
		} else if !os.IsNotExist(err) {
			return 1, err
		}
	}
	if selected := byKind["node"]; selected != nil {
		if _, err := os.Stat(filepath.Join(e.ProjectDir, "package-lock.json")); err == nil {
			platform, err := npm.CurrentPlatform()
			if err != nil {
				return 1, err
			}
			plan, err := npm.Load(e.ProjectDir, platform)
			if err != nil {
				return 1, fmt.Errorf("check frozen Node runtime requirements: %w", err)
			}
			if err := runtimes.CheckNode(selected.Version, plan.NodeConstraints); err != nil {
				return 2, err
			}
		} else if !os.IsNotExist(err) {
			return 1, err
		}
	}
	values := map[string]string{}
	dirs := []string{}
	// Owned managed bins come first: a shared /usr/bin containing another
	// interpreter must not shadow the interpreter selected for this project.
	for _, status := range []string{"managed", "local"} {
		for _, selected := range plans {
			if selected.Status != status {
				continue
			}
			dirs = append(dirs, selected.BinDir)
			for key, value := range selected.Environment {
				values[key] = value
			}
		}
	}
	dirs = append(dirs, filepath.Join(e.Home, "projects", e.scopeKey, "bin"), filepath.Join(e.ProjectDir, "node_modules", ".bin"))
	path := strings.Join(dirs, string(os.PathListSeparator))
	if ambient := os.Getenv("PATH"); ambient != "" {
		path += string(os.PathListSeparator) + ambient
	}
	values["PATH"] = path
	values["ARCHER_HOME"] = e.Home
	binary := ""
	// An explicit php/node command must use the selected interpreter, including
	// local selections whose filename is not literally php or node.
	for kind, selected := range byKind {
		if argv[0] == kind || (runtime.GOOS == "windows" && (strings.EqualFold(argv[0], kind) || strings.EqualFold(argv[0], kind+".exe"))) {
			binary = selected.Path
		}
	}
	for kind, selected := range byKind {
		if resolved, lookupErr := resolveRunCommand(kind, e.ProjectDir, dirs); lookupErr == nil {
			found, foundErr := os.Stat(resolved)
			chosen, chosenErr := os.Stat(selected.Path)
			if foundErr != nil || chosenErr != nil || !os.SameFile(found, chosen) {
				return 1, fmt.Errorf("project PATH would select a different %s interpreter; use a runtime bundle with its own bin directory", kind)
			}
		}
	}
	if binary == "" {
		binary, err = resolveRunCommand(argv[0], e.ProjectDir, dirs)
		if err != nil {
			return 1, err
		}
	}
	if err := e.checkSourceSnapshot(sources); err != nil {
		return 1, err
	}
	command := exec.CommandContext(ctx, binary, argv[1:]...)
	command.Dir, command.Env = e.ProjectDir, runtimeEnvironment(os.Environ(), values)
	command.Stdin, command.Stdout, command.Stderr = input, output, diagnostics
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return 1, ctx.Err()
		}
		if exited, ok := err.(*exec.ExitError); ok && exited.ExitCode() >= 0 {
			return exited.ExitCode(), nil
		}
		return 1, fmt.Errorf("run %s: %w", argv[0], err)
	}
	return 0, nil
}

func resolveRunCommand(name, project string, dirs []string) (string, error) {
	if strings.ContainsAny(name, `/\\`) || filepath.IsAbs(name) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(project, name)
		}
		return exec.LookPath(name)
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, name)
		if found, err := exec.LookPath(path); err == nil {
			return found, nil
		}
	}
	return exec.LookPath(name)
}
