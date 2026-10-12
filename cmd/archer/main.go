package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/archer"
	"github.com/Parallax-Web-Services/archer/internal/parallax"
	"github.com/Parallax-Web-Services/archer/internal/worker"
)

var version = "0.3.0-dev"

type options struct {
	command, project, home, approval      string
	json, offline, plan, stdin, preflight bool
	requirements                          []archer.Requirement
	argv                                  []string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func parse(args []string) (options, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return options{}, err
	}
	home := os.Getenv("ARCHER_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return options{}, err
		}
		home = filepath.Join(user, ".archer")
	}
	o := options{command: "help", project: cwd, home: home}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--":
			if o.command != "run" {
				return o, fmt.Errorf("-- is supported only for run")
			}
			if i+1 == len(args) {
				return o, fmt.Errorf("run requires a command after --")
			}
			o.argv = append([]string{}, args[i+1:]...)
			return o, nil
		case "--json":
			o.json = true
		case "--offline":
			o.offline = true
		case "--plan":
			o.plan = true
		case "--stdin":
			o.stdin = true
		case "--preflight":
			o.preflight = true
		case "--project", "--home", "--approval":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("%s needs a value", arg)
			}
			switch arg {
			case "--project":
				o.project = args[i]
			case "--home":
				o.home = args[i]
			case "--approval":
				o.approval = args[i]
			}
		case "--version":
			o.command = "version"
		case "--help", "-h":
			o.command = "help"
		default:
			if strings.HasPrefix(arg, "-") {
				return o, fmt.Errorf("unknown option %s", arg)
			}
			if o.command == "help" {
				o.command = arg
			} else {
				o.requirements = append(o.requirements, archer.Requirement{Capability: arg})
			}
		}
	}
	return o, nil
}
func run(args []string, out, errout io.Writer) int {
	return runWithInput(args, os.Stdin, out, errout)
}

func runWithInput(args []string, in io.Reader, out, errout io.Writer) int {
	o, err := parse(args)
	if err != nil {
		return commandError(out, errout, err, o.stdin || o.command == "resolve" || o.command == "capabilities")
	}
	if o.command == "run" && (len(o.argv) == 0 || len(o.requirements) != 0 || o.stdin || o.preflight || o.plan || o.json || o.approval != "") {
		return commandError(out, errout, fmt.Errorf("run accepts --project, --home, --offline and requires an explicit command after --"), false)
	}
	machine := o.stdin || o.command == "resolve" || o.command == "capabilities"
	if o.stdin && o.command != "resolve" && o.command != "i" && o.command != "install" {
		return commandError(out, errout, fmt.Errorf("--stdin is supported by resolve and install only"), machine)
	}
	if o.preflight && (!o.stdin || (o.command != "i" && o.command != "install")) {
		return commandError(out, errout, fmt.Errorf("--preflight requires install --stdin"), machine)
	}
	if o.stdin && (o.plan || o.approval != "" || len(o.requirements) > 0) {
		return commandError(out, errout, fmt.Errorf("stdin requests cannot be combined with positional requirements, --plan, or --approval; use context.approved_resolution and --preflight"), machine)
	}
	switch o.command {
	case "version":
		fmt.Fprintln(out, "Archer", version)
		return 0
	case "help":
		fmt.Fprint(out, `Archer — straight & steady.
Usage:
  archer init                         inspect project package manifests and create archer.json
  archer plan [capability ...]         verify service/local reuse and explain remaining work
  archer i [capability ...]            replay locked PHP/npm packages or install pinned artifacts
  archer i --plan                      inspect without downloading or installing artifacts
  archer context                       show remembered project-scoped outcomes
  archer run [options] -- COMMAND ...   run argv with freshly verified project runtimes
  archer capabilities --json           advertise configured capability descriptors
  archer resolve --json --stdin        return a Parallax resolution for requirements/context
  archer install --json --stdin        execute an approved Parallax handoff
  archer install --preflight --stdin   verify an approved handoff without installation
  archer version
Options: --project DIR --home DIR --json --offline --approval PLAN_ID --plan
Worker options: --stdin --preflight (native archer.project/v1 recipes only)
No command invokes a model. Unknown work is returned explicitly.
`)
		return 0
	case "init":
		if err := initialize(o.project, out); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	case "plan", "doctor", "i", "install", "context", "resolve", "capabilities", "run":
	default:
		fmt.Fprintln(errout, "unknown command:", o.command)
		return 1
	}
	engine, err := archer.Load(o.project, o.home, o.offline)
	if err != nil {
		return commandError(out, errout, err, machine)
	}
	if o.command == "run" {
		code, err := engine.Run(context.Background(), o.argv, in, out, errout)
		if err != nil {
			fmt.Fprintln(errout, err)
		}
		return code
	}
	if o.command == "capabilities" {
		if len(o.requirements) > 0 {
			return commandError(out, errout, fmt.Errorf("capabilities does not accept requirement arguments"), true)
		}
		capabilities, err := worker.Capabilities(engine)
		if err != nil {
			return commandError(out, errout, err, true)
		}
		return encode(out, capabilities)
	}
	if o.command == "resolve" || o.stdin {
		var input parallax.Object
		if o.stdin {
			input, err = parallax.Decode(in)
		} else {
			selected, selectErr := selectRequirements(engine.Project, o.requirements)
			if selectErr != nil {
				return commandError(out, errout, selectErr, true)
			}
			if len(o.requirements) == 0 {
				selected = engine.Project.Requirements
			}
			requirements := []string{}
			for _, r := range selected {
				requirements = append(requirements, r.Capability+r.Constraint)
			}
			input = parallax.Object{"requirements": requirements, "context": parallax.Object{}}
		}
		if err != nil {
			return commandError(out, errout, err, true)
		}
		var result parallax.Object
		if o.command == "resolve" {
			result, err = worker.Resolve(context.Background(), engine, input)
		} else {
			result, err = worker.Execute(context.Background(), engine, input, o.preflight)
		}
		if result == nil {
			if err == nil {
				err = fmt.Errorf("worker returned no result")
			}
			return commandError(out, errout, err, true)
		}
		if code := encode(out, result); code != 0 {
			return code
		}
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		status, _ := result["status"].(string)
		if status == "failed" {
			return 1
		}
		if o.command == "resolve" {
			if status == "unsatisfied" {
				return 2
			}
			return 0
		}
		if status != "satisfied" {
			return 2
		}
		return 0
	}
	if o.command == "context" {
		value, err := engine.Context()
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return encode(out, value)
	}
	var plan archer.Plan
	o.requirements, err = selectRequirements(engine.Project, o.requirements)
	if err != nil {
		return commandError(out, errout, err, false)
	}
	if o.command == "i" || o.command == "install" {
		if o.plan {
			plan, err = engine.Plan(context.Background(), o.requirements)
		} else {
			plan, err = engine.Install(context.Background(), o.requirements, o.approval)
		}
	} else {
		plan, err = engine.Plan(context.Background(), o.requirements)
	}
	if err != nil {
		if plan.Schema != "" {
			_ = renderPlan(out, o.json, plan)
		}
		fmt.Fprintln(errout, err)
		return 1
	}
	if code := renderPlan(out, o.json, plan); code != 0 {
		return code
	}
	if len(plan.Remaining) > 0 {
		return 2
	}
	return 0
}

func commandError(out, errout io.Writer, err error, machine bool) int {
	if machine {
		_ = encode(out, parallax.Object{"error": err.Error()})
	}
	fmt.Fprintln(errout, err)
	return 1
}

// A selector changes the requirement set, never its declared delivery or version policy.
func selectRequirements(project archer.Project, selectors []archer.Requirement) ([]archer.Requirement, error) {
	if len(selectors) == 0 {
		return nil, nil
	}
	selected := []archer.Requirement{}
	for _, selector := range selectors {
		match := selector
		found := false
		for _, requirement := range project.Requirements {
			if requirement.Capability != selector.Capability {
				continue
			}
			if found && match != requirement {
				return nil, fmt.Errorf("capability %s has conflicting project requirements", selector.Capability)
			}
			match, found = requirement, true
		}
		selected = append(selected, match)
	}
	return selected, nil
}
func renderPlan(out io.Writer, asJSON bool, plan archer.Plan) int {
	if asJSON {
		return encode(out, plan)
	} else {
		fmt.Fprintf(out, "Archer · %s · %s\n", plan.Scope, plan.Platform)
		for _, runtime := range plan.Runtimes {
			fmt.Fprintf(out, "%-10s %-24s %s\n", runtime.Status, "runtime:"+runtime.Kind, runtime.Reason)
			if runtime.Path != "" {
				fmt.Fprintln(out, "           "+runtime.Path)
			}
		}
		for _, d := range plan.Decisions {
			fmt.Fprintf(out, "%-10s %-24s %s\n", d.Status, d.Requirement.Capability, d.Reason)
			if d.Path != "" {
				fmt.Fprintln(out, "           "+d.Path)
			}
		}
		fmt.Fprintf(out, "Plan %s\n%d remaining · %d AI calls\n", plan.ID, len(plan.Remaining), plan.AIRequests)
	}
	return 0
}
func encode(out io.Writer, value any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return 1
	}
	return 0
}
func initialize(dir string, out io.Writer) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "archer.json")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("archer.json already exists; edit its explicit requirements and bindings")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	project := archer.Project{Schema: "archer.project/v1", Scope: filepath.Base(dir), Requirements: []archer.Requirement{}}
	seen := map[string]bool{}
	for _, source := range []struct {
		file, prefix string
		keys         []string
	}{
		{"composer.json", "composer:", []string{"require", "require-dev"}},
		{"package.json", "npm:", []string{"dependencies", "devDependencies", "optionalDependencies"}},
	} {
		b, err := os.ReadFile(filepath.Join(dir, source.file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(b, &manifest); err != nil {
			return fmt.Errorf("%s: %w", source.file, err)
		}
		for _, key := range source.keys {
			var packages map[string]string
			if manifest[key] == nil {
				continue
			}
			if err := json.Unmarshal(manifest[key], &packages); err != nil {
				return fmt.Errorf("%s %s: %w", source.file, key, err)
			}
			names := []string{}
			for name := range packages {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				cap := source.prefix + name
				if seen[cap] {
					continue
				}
				seen[cap] = true
				project.Requirements = append(project.Requirements, archer.Requirement{Capability: cap, Delivery: "package", Constraint: packages[name]})
			}
		}
	}
	b, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Fprintf(out, "Created %s with %d explicit package requirements. Add service bindings and pinned artifact recipes before installing.\n", path, len(project.Requirements))
	return nil
}
