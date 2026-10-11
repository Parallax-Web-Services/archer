package archer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/composer"
)

func composerDecisionKey(r Requirement) string {
	return r.Capability + "\x00" + r.Delivery + "\x00" + r.Constraint
}

func (e *Engine) composerPlan(ctx context.Context, requirements []Requirement) (*composer.Plan, *composer.RuntimeInfo, map[string]Decision, error) {
	decisions := map[string]Decision{}
	for _, r := range requirements {
		if strings.HasPrefix(r.Capability, "composer:") {
			decisions[composerDecisionKey(r)] = Decision{Requirement: r, Status: "unresolved", Discovery: "not_applicable", Reason: "only directly declared Composer requirements can be selected for frozen replay"}
		}
	}
	if len(decisions) == 0 {
		return nil, nil, decisions, nil
	}
	config := ComposerConfig{}
	if e.Project.Composer != nil {
		config = *e.Project.Composer
	}
	plan, err := composer.Load(e.ProjectDir, config.SourcePolicy)
	if err != nil {
		if errors.Is(err, composer.ErrSourcePolicy) {
			for key, d := range decisions {
				d.Reason = err.Error()
				decisions[key] = d
			}
			return nil, nil, decisions, nil
		}
		return nil, nil, decisions, fmt.Errorf("load frozen Composer tree: %w", err)
	}
	var runtime *composer.RuntimeInfo
	runtimeError := "runtime verification is unavailable on the planning host"
	if !e.SkipLocalVerification {
		info, err := composer.ProbeRuntime(ctx, config.PHP)
		if err == nil {
			err = composer.CheckRuntime(plan, info)
			runtime = &info
		}
		if err != nil {
			runtimeError = err.Error()
		} else {
			runtimeError = ""
		}
	}
	packages := map[string]composer.Package{}
	for _, p := range plan.Packages {
		packages[p.Name] = p
	}
	active := false
	for _, r := range requirements {
		if !strings.HasPrefix(r.Capability, "composer:") {
			continue
		}
		key := composerDecisionKey(r)
		d := decisions[key]
		name := strings.TrimPrefix(r.Capability, "composer:")
		constraint, declared := plan.Direct[name]
		if !declared {
			continue
		}
		if r.Delivery == "service" {
			d.Reason = "Composer replay supplies a local library or PHP capability and cannot fulfill service delivery"
		} else if r.Constraint != "" && r.Constraint != constraint {
			d.Reason = fmt.Sprintf("requested Composer constraint %q differs from frozen declaration %q", r.Constraint, constraint)
		} else if runtimeError != "" {
			d.Reason = "Composer runtime readiness blocked: " + runtimeError
		} else if name == "php" || strings.HasPrefix(name, "ext-") {
			d.Status = "composer_platform"
			d.Provider = "php-runtime"
			d.Reason = "PHP and extension requirements freshly verified against the complete frozen graph"
			d.Evidence = "php:" + runtime.PHPVersion
		} else if p, exists := packages[name]; exists {
			active = true
			d.Status = "composer_install"
			d.Provider = "composer-lock"
			d.Path = filepath.Join(e.ProjectDir, filepath.FromSlash(p.Path))
			d.Reason = fmt.Sprintf("replay the full frozen Composer tree (%d packages) for %s@%s; scripts and plugins are not executed", len(plan.Packages), name, p.Version)
		} else {
			d.Reason = "direct Composer requirement is virtual or replaced; selecting it as a standalone installation is unsupported"
		}
		decisions[key] = d
	}
	if !active {
		return nil, runtime, decisions, nil
	}
	verified, err := composer.Verify(e.ProjectDir, plan)
	if err != nil {
		return nil, runtime, decisions, fmt.Errorf("verify frozen Composer tree: %w", err)
	}
	if verified {
		for key, d := range decisions {
			if d.Status == "composer_install" {
				d.Status = "composer_installed"
				d.Cached = true
				d.Evidence = "composer-plan:" + plan.ID
				d.Reason = "reuse the freshly verified full frozen Composer tree and generated autoloader"
				decisions[key] = d
			}
		}
	}
	return plan, runtime, decisions, nil
}

func (e *Engine) installComposer(ctx context.Context, plan *composer.Plan) (composer.Report, error) {
	config := ComposerConfig{}
	if e.Project.Composer != nil {
		config = *e.Project.Composer
	}
	info, err := composer.ProbeRuntime(ctx, config.PHP)
	if err == nil {
		err = composer.CheckRuntime(plan, info)
	}
	if err != nil {
		return composer.Report{}, fmt.Errorf("Composer runtime readiness: %w", err)
	}
	return composer.Install(ctx, e.ProjectDir, plan, composer.Options{Home: e.Home, Offline: e.Offline})
}
