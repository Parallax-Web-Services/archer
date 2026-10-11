package archer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Parallax-Web-Services/archer/internal/catalog"
	"github.com/Parallax-Web-Services/archer/internal/store"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Engine struct {
	Project    Project
	ProjectDir string
	Home       string
	Offline    bool
	// Remote planning may discover routes, but cannot prove the worker's
	// installed state by executing probes on the planning host.
	SkipLocalVerification bool
	scopeKey              string
	store                 *store.Store
}

func Load(projectDir, home string, offline bool) (*Engine, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	var p Project
	if err := readJSON(filepath.Join(abs, "archer.json"), &p); err != nil {
		return nil, fmt.Errorf("read archer.json: %w (run archer init first)", err)
	}
	if source := os.Getenv("PARALLAX_SAAS_INDEX_URL"); source != "" && !provides(p.Registries, source) {
		p.Registries = append(p.Registries, source)
	}
	return New(p, abs, home, offline)
}
func New(p Project, projectDir, home string, offline bool) (*Engine, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	key := digest([]byte(p.Scope + "\x00" + absProject))
	return &Engine{Project: p, ProjectDir: absProject, Home: absHome, Offline: offline, scopeKey: key, store: store.New(filepath.Join(absHome, "cache"))}, nil
}
func provides(values []string, name string) bool {
	for _, v := range values {
		if v == name {
			return true
		}
	}
	return false
}
func packageRequirement(r Requirement) bool {
	return r.Delivery == "package" || strings.HasPrefix(r.Capability, "composer:") || strings.HasPrefix(r.Capability, "npm:")
}
func (e *Engine) Plan(ctx context.Context, requirements []Requirement) (Plan, error) {
	if len(requirements) == 0 {
		requirements = e.Project.Requirements
	}
	check := e.Project
	check.Requirements = requirements
	if err := check.Validate(); err != nil {
		return Plan{}, err
	}
	sources, err := e.sourceHashes()
	if err != nil {
		return Plan{}, err
	}
	npmPlan, npmDecisions, err := e.npmPlan(requirements)
	if err != nil {
		return Plan{}, err
	}
	profile, err := e.Context()
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Schema: "archer.plan/v1", Scope: e.Project.Scope, Platform: runtime.GOOS + "/" + runtime.GOARCH, Decisions: []Decision{}, Remaining: []Requirement{}}
	plan.NPM = npmPlan
	services := append([]catalog.Service{}, e.Project.Services...)
	bindings := map[string]catalog.Service{}
	for _, service := range e.Project.Services {
		bindings[service.ID] = service
	}
	// Freshness refers only to the configured sources. No sources is unknown
	// discovery, never evidence that the Parallax ecosystem has no match.
	discoveryState := "not_configured"
	if len(e.Project.Registries) > 0 {
		discoveryState = catalog.StateFresh
	}
	discoverySources := e.Project.Registries
	needsDiscovery := false
	for _, r := range requirements {
		if !packageRequirement(r) && r.Delivery != "local" {
			needsDiscovery = true
		}
	}
	if !needsDiscovery {
		discoverySources = nil
	}
	for _, url := range discoverySources {
		var snapshot catalog.Snapshot
		if e.Offline {
			snapshot = catalog.Snapshot{Source: url, Stale: true, State: catalog.StateUnavailable, Error: "catalog refresh disabled in offline mode"}
		} else {
			snapshot, err = catalog.Fetch(ctx, url, filepath.Join(e.Home, "catalogs"))
			if err != nil {
				snapshot.Error = err.Error()
				snapshot.Stale = true
				snapshot.State = catalog.StateUnavailable
				snapshot.Source = url
			}
		}
		plan.Catalogs = append(plan.Catalogs, snapshot)
		if snapshot.Stale {
			if snapshot.State == catalog.StateUnavailable || discoveryState != catalog.StateUnavailable {
				discoveryState = snapshot.State
			}
			continue
		}
		services = append(services, snapshot.Services...)
	}
	discoveryUnknown := discoveryState != catalog.StateFresh
	sort.SliceStable(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	for _, r := range requirements {
		if d, ok := npmDecisions[npmDecisionKey(r)]; ok {
			plan.Decisions = append(plan.Decisions, d)
			if pendingDecision(d) {
				plan.Remaining = append(plan.Remaining, r)
			}
			continue
		}
		state := discoveryState
		if state == catalog.StateFresh {
			state = "fresh_no_match"
		}
		if r.Delivery == "local" {
			state = "local"
		}
		d := Decision{Requirement: r, Status: "unresolved", Reason: "no known capability; hand remaining intent to Slipstream", AIEligible: !discoveryUnknown || r.Delivery == "local", Discovery: state}
		if r.Constraint != "" && !packageRequirement(r) {
			d.Reason = "version-constrained capability needs the version resolver; availability alone does not establish compatibility"
			d.AIEligible = false
			plan.Decisions = append(plan.Decisions, d)
			plan.Remaining = append(plan.Remaining, r)
			continue
		}
		var candidates []catalog.Service
		seen := map[string]bool{}
		if !packageRequirement(r) && r.Delivery != "local" {
			for _, s := range services {
				if provides(s.Provides, r.Capability) && !seen[s.ID] {
					candidates = append(candidates, s)
					seen[s.ID] = true
				}
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				preferred := profile.Bindings[r.Capability]
				return candidates[i].ID == preferred && candidates[j].ID != preferred
			})
		}
		if len(candidates) > 0 && !discoveryUnknown {
			d.Discovery = "fresh_match"
		}
		for _, s := range candidates {
			if e.Offline {
				continue
			}
			binding, configured := bindings[s.ID]
			// A catalog advertises a service; only an explicit local binding establishes intended access.
			// Never retry a failed authenticated binding through an anonymous remote duplicate.
			if !configured || !provides(binding.Provides, r.Capability) {
				continue
			}
			evidence, err := catalog.Verify(ctx, binding)
			if err == nil {
				d = Decision{Requirement: r, Status: "service", Provider: s.ID, Reason: "reuse a verified configured service; no installation", Evidence: evidence, Discovery: "bound"}
				break
			}
		}
		if d.Status == "unresolved" && !e.SkipLocalVerification && r.Delivery != "service" && !packageRequirement(r) {
			for _, l := range e.Project.Local {
				if !provides(l.Provides, r.Capability) {
					continue
				}
				if evidence, err := e.probe(ctx, l.Probe); err == nil {
					d = Decision{Requirement: r, Status: "local", Provider: l.ID, Reason: "reuse a freshly verified local capability", Evidence: evidence, Discovery: "local"}
					break
				}
			}
		}
		if d.Status == "unresolved" && len(candidates) > 0 {
			d.Reason = "an existing service advertises this capability but needs configuration or successful verification; installation is blocked"
			d.Provider = candidates[0].ID
			d.AIEligible = false
		} else if d.Status == "unresolved" && discoveryUnknown && r.Delivery != "local" && !packageRequirement(r) {
			d.Reason = "service discovery is stale or unavailable; resolve discovery before installing or generating"
			if discoveryState == "not_configured" {
				d.Reason = "service discovery is not configured; configure catalog coverage or declare local delivery before installing or generating"
			}
			d.AIEligible = false
		} else if d.Status == "unresolved" && !packageRequirement(r) && r.Delivery != "service" {
			for _, recipe := range e.Project.Recipes {
				if !provides(recipe.Provides, r.Capability) || !e.supports(recipe) {
					continue
				}
				path, cached, err := e.store.Has(recipe.Artifact.SHA256)
				if err != nil {
					return Plan{}, fmt.Errorf("cache integrity for %s: %w", recipe.ID, err)
				}
				d = Decision{Requirement: r, Status: "install", Provider: recipe.ID, Reason: "install a pinned artifact through Archer", Path: e.target(recipe), Cached: cached, Discovery: d.Discovery}
				if cached {
					d.Reason = "reuse verified content from the shared artifact cache"
					d.Evidence = path
				}
				if !e.SkipLocalVerification && verifiedTarget(d.Path, strings.ToLower(recipe.Artifact.SHA256)) {
					d.Status = "installed"
					d.Reason = "reuse a freshly hash-verified managed artifact"
					d.Evidence = "sha256:" + recipe.Artifact.SHA256
				}
				break
			}
		}
		if packageRequirement(r) && d.Status == "unresolved" {
			d.Reason = "in-process package dependency needs a package resolver; a remote service cannot substitute for this library"
			d.AIEligible = false
			d.Discovery = "not_applicable"
		}
		plan.Decisions = append(plan.Decisions, d)
		if pendingDecision(d) {
			plan.Remaining = append(plan.Remaining, r)
		}
	}
	// Fingerprint selected routes, project sources and scoped context, excluding transient probe output and cache warmth.
	type selected struct {
		Requirement Requirement
		Status      string
		Provider    string
		Path        string
		Discovery   string
	}
	routes := []selected{}
	for _, d := range plan.Decisions {
		routes = append(routes, selected{d.Requirement, d.Status, d.Provider, d.Path, d.Discovery})
	}
	identity := struct {
		Project  Project
		Routes   []selected
		Scope    string
		Revision int
		Sources  map[string]string
		Platform string
		Services []catalog.Service
		NPM      string
	}{e.Project, routes, e.scopeKey, profile.Revision, sources, plan.Platform, services, ""}
	if npmPlan != nil {
		identity.NPM = npmPlan.ID
	}
	b, _ := json.Marshal(identity)
	plan.ID = digest(b)
	return plan, nil
}
func (e *Engine) supports(r Recipe) bool {
	return len(r.Platforms) == 0 || provides(r.Platforms, runtime.GOOS+"/"+runtime.GOARCH)
}
func (e *Engine) target(r Recipe) string {
	return filepath.Join(e.Home, "projects", e.scopeKey, "bin", r.Filename)
}

func verifiedTarget(path, want string) bool {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > store.MaxArtifactBytes || (runtime.GOOS != "windows" && before.Mode().Perm()&0111 == 0) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return false
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, store.MaxArtifactBytes+1))
	if err != nil || n > store.MaxArtifactBytes {
		return false
	}
	after, err := f.Stat()
	return err == nil && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && hex.EncodeToString(h.Sum(nil)) == want
}

func refreshRemaining(plan Plan) Plan {
	plan.Remaining = []Requirement{}
	for _, d := range plan.Decisions {
		if pendingDecision(d) {
			plan.Remaining = append(plan.Remaining, d.Requirement)
		}
	}
	return plan
}

func pendingDecision(d Decision) bool {
	return d.Status == "unresolved" || d.Status == "install" || d.Status == "npm_install" || d.Status == "npm_skipped"
}

const maxProjectSourceBytes = 16 << 20

func (e *Engine) sourceHashes() (map[string]string, error) {
	out := map[string]string{}
	for _, file := range []string{"composer.json", "composer.lock", "package.json", "package-lock.json", "npm-shrinkwrap.json", "archer.json"} {
		f, err := os.Open(filepath.Join(e.ProjectDir, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read project source %s: %w", file, err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxProjectSourceBytes {
			f.Close()
			return nil, fmt.Errorf("project source %s must be a regular file of at most %d bytes", file, maxProjectSourceBytes)
		}
		h := sha256.New()
		n, readErr := io.Copy(h, io.LimitReader(f, maxProjectSourceBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || n > maxProjectSourceBytes {
			return nil, fmt.Errorf("cannot hash bounded project source %s", file)
		}
		out[file] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}
func (e *Engine) probe(ctx context.Context, p Probe) (string, error) {
	if len(p.Argv) == 0 {
		return "", fmt.Errorf("empty probe")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
	command.Dir = e.ProjectDir
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return "", err
	}
	text := strings.TrimSpace(output.String())
	if p.Contains != "" && !strings.Contains(text, p.Contains) {
		return "", fmt.Errorf("probe output did not match")
	}
	return text, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := 16384 - b.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func (e *Engine) Install(ctx context.Context, requirements []Requirement, approval string) (Plan, error) {
	plan, err := e.Plan(ctx, requirements)
	if err != nil {
		return Plan{}, err
	}
	if approval != "" && approval != plan.ID {
		return plan, fmt.Errorf("plan changed since approval; review a fresh plan")
	}
	if plan.NPM != nil {
		report, err := e.installNPM(ctx, plan.NPM)
		plan.NPMReport = &report
		if err != nil {
			return refreshRemaining(plan), fmt.Errorf("replay npm lock: %w", err)
		}
		for i, d := range plan.Decisions {
			if d.Status == "npm_install" {
				plan.Decisions[i].Status = "npm_installed"
				plan.Decisions[i].Reason = "verified full frozen npm dependency tree; no lifecycle scripts executed and runtime compatibility is untested"
				plan.Decisions[i].Evidence = "npm-plan:" + plan.NPM.ID
				plan.Decisions[i].Cached = report.Downloaded == 0
			}
		}
	}
	for i, d := range plan.Decisions {
		if d.Status != "install" {
			continue
		}
		var recipe Recipe
		for _, r := range e.Project.Recipes {
			if r.ID == d.Provider {
				recipe = r
				break
			}
		}
		if e.Offline && !d.Cached {
			plan.Decisions[i].Status = "unresolved"
			plan.Decisions[i].Reason = "artifact unavailable in offline cache"
			continue
		}
		path, cached, err := e.store.Ensure(ctx, recipe.Artifact)
		if err != nil {
			return refreshRemaining(plan), fmt.Errorf("obtain %s: %w", recipe.ID, err)
		}
		if _, err := e.store.Publish(path, e.target(recipe)); err != nil {
			return refreshRemaining(plan), fmt.Errorf("publish %s: %w", recipe.ID, err)
		}
		plan.Decisions[i].Status = "installed"
		plan.Decisions[i].Cached = cached
		plan.Decisions[i].Reason = "verified artifact installed in the project-scoped Archer bin directory"
		plan.Decisions[i].Evidence = "sha256:" + recipe.Artifact.SHA256
	}
	plan.Remaining = []Requirement{}
	for _, d := range plan.Decisions {
		if pendingDecision(d) {
			plan.Remaining = append(plan.Remaining, d.Requirement)
		}
	}
	if err := e.record(plan); err != nil {
		return plan, fmt.Errorf("record scoped outcomes: %w", err)
	}
	return plan, nil
}
