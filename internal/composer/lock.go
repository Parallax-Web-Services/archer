// Package composer replays an immutable Composer lock without running Composer,
// a dependency solver, plugins, or project scripts.
package composer

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Parallax-Web-Services/archer/internal/parallax"
)

type SourcePolicy struct {
	Mode         string   `json:"mode"`
	TrustedHosts []string `json:"trusted_hosts"`
}

type Autoload struct {
	PSR4                map[string][]string `json:"psr-4,omitempty"`
	PSR0                map[string][]string `json:"psr-0,omitempty"`
	Classmap            []string            `json:"classmap,omitempty"`
	Files               []string            `json:"files,omitempty"`
	ExcludeFromClassmap []string            `json:"exclude-from-classmap,omitempty"`
}

type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	// DefaultBranch marks a branch version Composer aliases as 9999999-dev.
	DefaultBranch bool              `json:"default_branch,omitempty"`
	URL           string            `json:"url"`
	Reference     string            `json:"reference"`
	SourceURL     string            `json:"source_url,omitempty"`
	SHA1          string            `json:"sha1,omitempty"`
	Type          string            `json:"type"`
	Dev           bool              `json:"dev,omitempty"`
	Autoload      Autoload          `json:"autoload"`
	Bin           []string          `json:"bin,omitempty"`
	Require       map[string]string `json:"require,omitempty"`
	Provide       map[string]string `json:"provide,omitempty"`
	Replace       map[string]string `json:"replace,omitempty"`
	Conflict      map[string]string `json:"conflict,omitempty"`
	Extra         map[string]any    `json:"extra,omitempty"`
}

type Requirement struct {
	From       string `json:"from"`
	Name       string `json:"name"`
	Constraint string `json:"constraint"`
}

type RuntimeInfo struct {
	PHPVersion string            `json:"php_version"`
	Extensions map[string]string `json:"extensions"`
}

type Plan struct {
	Schema               string            `json:"schema"`
	ManifestSHA256       string            `json:"manifest_sha256"`
	LockSHA256           string            `json:"lock_sha256"`
	ContentHash          string            `json:"content_hash"`
	ID                   string            `json:"id,omitempty"`
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Policy               SourcePolicy      `json:"policy"`
	Packages             []Package         `json:"packages"`
	Autoload             Autoload          `json:"autoload"`
	AutoloadDev          Autoload          `json:"autoload_dev"`
	Require              map[string]string `json:"require"`
	RequireDev           map[string]string `json:"require_dev"`
	Direct               map[string]string `json:"direct"`
	PlatformRequirements []Requirement     `json:"platform_requirements"`
	PlatformConflicts    []Requirement     `json:"platform_conflicts,omitempty"`
	Conflict             map[string]string `json:"conflict,omitempty"`
	Scripts              map[string]any    `json:"scripts,omitempty"`
	Repositories         []string          `json:"repositories,omitempty"`
}

const maxManifestBytes = 2 << 20
const maxLockBytes = 16 << 20

// The autoloader profile implements InstalledVersions and binary proxy globals.
const RuntimeAPIVersion = "2.2.2"

var packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
var referencePattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var contentHashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var stableVersionPattern = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+){0,3}$`)
var branchVersionPattern = regexp.MustCompile(`^(?:dev-[A-Za-z0-9][A-Za-z0-9._/-]*|v?[0-9]+(?:\.[0-9]+){0,2}\.x-dev)$`)
var repositorySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var extensionPattern = regexp.MustCompile(`^ext-[a-z0-9][a-z0-9_-]*$`)
var hostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)
var ErrSourcePolicy = errors.New("Composer archive source policy requires review")

// Load validates source bytes, Composer's root content-hash, an exact locked
// graph, and the caller's explicit archive trust profile. Missing SHA-1 values
// require source-reference mode; first-seen SHA-256 is recorded by installation
// and is never described as a checksum published in the Composer lock.
func Load(projectDir string, policy SourcePolicy) (*Plan, error) {
	policy, err := normalizePolicy(policy)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := readBoundedFile(filepath.Join(projectDir, "composer.json"), maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	lockBytes, err := readBoundedFile(filepath.Join(projectDir, "composer.lock"), maxLockBytes)
	if err != nil {
		return nil, fmt.Errorf("composer.lock: %w", err)
	}
	manifest, err := decodeObject(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	lock, err := decodeObject(lockBytes)
	if err != nil {
		return nil, fmt.Errorf("composer.lock: %w", err)
	}
	if err := validateObjectFields(manifest, "require", "require-dev", "config", "extra", "autoload", "autoload-dev", "conflict", "replace", "provide", "scripts"); err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	contentHash, err := ContentHash(manifestBytes)
	if err != nil {
		return nil, err
	}
	if value, _ := lock["content-hash"].(string); value != contentHash {
		return nil, errors.New("composer.lock content-hash does not match composer.json")
	}
	for _, field := range []string{"aliases", "platform-overrides"} {
		if !emptyJSON(lock[field]) {
			return nil, fmt.Errorf("Composer %s is not supported by frozen replay", field)
		}
	}
	if config, ok := manifest["config"].(map[string]any); ok {
		if !emptyJSON(config["platform"]) {
			return nil, errors.New("Composer platform overrides are not supported")
		}
		for _, field := range []string{"vendor-dir", "bin-dir"} {
			if v, exists := config[field]; exists && v != map[string]string{"vendor-dir": "vendor", "bin-dir": "vendor/bin"}[field] {
				return nil, fmt.Errorf("custom Composer %s is not supported", field)
			}
		}
	}
	repositories, err := parseRepositories(manifest["repositories"])
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"provide", "replace"} {
		if !emptyJSON(manifest[field]) {
			return nil, fmt.Errorf("root Composer %s is not supported", field)
		}
	}
	for _, field := range []string{"target-dir", "include-path"} {
		if _, exists := manifest[field]; exists {
			return nil, fmt.Errorf("Composer %s is not supported", field)
		}
	}
	plan := &Plan{Schema: "archer.composer-plan/v1", ManifestSHA256: bytesHash(manifestBytes), LockSHA256: bytesHash(lockBytes), ContentHash: contentHash, Policy: policy, Packages: []Package{}, PlatformRequirements: []Requirement{}, Direct: map[string]string{}, Repositories: repositories}
	plan.Name, _ = manifest["name"].(string)
	if v, exists := manifest["name"]; exists {
		if _, ok := v.(string); !ok {
			return nil, errors.New("root name must be a string")
		}
	}
	if plan.Name == "" {
		plan.Name = "__root__"
	} else if !packagePattern.MatchString(plan.Name) {
		return nil, errors.New("invalid Composer root package name")
	}
	plan.Version, _ = manifest["version"].(string)
	if v, exists := manifest["version"]; exists {
		if _, ok := v.(string); !ok {
			return nil, errors.New("root version must be a string")
		}
	}
	if plan.Version == "" {
		plan.Version = "1.0.0"
	}
	if !stableVersionPattern.MatchString(plan.Version) {
		return nil, errors.New("root version must be a stable numeric version")
	}
	plan.Require, err = dependencyMap(manifest["require"], "root require")
	if err != nil {
		return nil, err
	}
	plan.RequireDev, err = dependencyMap(manifest["require-dev"], "root require-dev")
	if err != nil {
		return nil, err
	}
	plan.Conflict, err = dependencyMap(manifest["conflict"], "root conflict")
	if err != nil {
		return nil, err
	}
	for _, deps := range []map[string]string{plan.Require, plan.RequireDev} {
		for name, constraint := range deps {
			if previous, exists := plan.Direct[name]; exists && previous != constraint {
				return nil, fmt.Errorf("conflicting root requirements for %s", name)
			}
			plan.Direct[name] = constraint
		}
	}
	if err := validateStabilityFlags(lock["stability-flags"], plan.Direct); err != nil {
		return nil, err
	}
	plan.Autoload, err = parseAutoload(manifest["autoload"])
	if err != nil {
		return nil, fmt.Errorf("root autoload: %w", err)
	}
	plan.AutoloadDev, err = parseAutoload(manifest["autoload-dev"])
	if err != nil {
		return nil, fmt.Errorf("root autoload-dev: %w", err)
	}
	if v, exists := manifest["scripts"]; exists {
		var ok bool
		plan.Scripts, ok = v.(map[string]any)
		if !ok {
			return nil, errors.New("root scripts must be an object")
		}
	}
	seen := map[string]bool{}
	for _, field := range []string{"packages", "packages-dev"} {
		entries, ok := lock[field].([]any)
		if !ok {
			return nil, fmt.Errorf("Composer lock %s must be an array", field)
		}
		for _, value := range entries {
			entry, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("Composer locked package must be an object")
			}
			pkg, err := parsePackage(entry, field == "packages-dev", policy, repositories)
			if err != nil {
				return nil, err
			}
			if seen[pkg.Name] {
				return nil, fmt.Errorf("duplicate locked package %s", pkg.Name)
			}
			seen[pkg.Name] = true
			plan.Packages = append(plan.Packages, pkg)
		}
	}
	sort.Slice(plan.Packages, func(i, j int) bool { return plan.Packages[i].Name < plan.Packages[j].Name })
	for _, item := range []struct {
		field string
		deps  map[string]string
	}{{"platform", plan.Require}, {"platform-dev", plan.RequireDev}} {
		locked, err := dependencyMap(lock[item.field], item.field)
		if err != nil {
			return nil, err
		}
		want := map[string]string{}
		for name, c := range item.deps {
			if isPlatform(name) {
				want[name] = c
			}
		}
		if !sameMap(want, locked) {
			return nil, fmt.Errorf("lock %s differs from root platform requirements", item.field)
		}
	}
	for name, c := range plan.Direct {
		if isPlatform(name) {
			plan.PlatformRequirements = append(plan.PlatformRequirements, Requirement{From: plan.Name, Name: name, Constraint: c})
		}
	}
	for _, pkg := range plan.Packages {
		for name, c := range pkg.Require {
			if isPlatform(name) {
				plan.PlatformRequirements = append(plan.PlatformRequirements, Requirement{From: pkg.Name, Name: name, Constraint: c})
			}
		}
	}
	for name, c := range plan.Conflict {
		if isPlatform(name) {
			plan.PlatformConflicts = append(plan.PlatformConflicts, Requirement{From: plan.Name, Name: name, Constraint: c})
		}
	}
	for _, pkg := range plan.Packages {
		for name, c := range pkg.Conflict {
			if isPlatform(name) {
				plan.PlatformConflicts = append(plan.PlatformConflicts, Requirement{From: pkg.Name, Name: name, Constraint: c})
			}
		}
	}
	sort.Slice(plan.PlatformConflicts, func(i, j int) bool {
		a, b := plan.PlatformConflicts[i], plan.PlatformConflicts[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Constraint < b.Constraint
	})
	sort.Slice(plan.PlatformRequirements, func(i, j int) bool {
		a, b := plan.PlatformRequirements[i], plan.PlatformRequirements[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Constraint < b.Constraint
	})
	if err := validateGraph(plan); err != nil {
		return nil, err
	}
	plan.ID, err = parallax.Hash(plan)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func normalizePolicy(policy SourcePolicy) (SourcePolicy, error) {
	if policy.Mode == "" {
		policy.Mode = "checksum"
	}
	if policy.Mode != "checksum" && policy.Mode != "source-reference" {
		return policy, errors.New("Composer source policy must be checksum or source-reference")
	}
	if policy.TrustedHosts == nil {
		policy.TrustedHosts = []string{}
	} else {
		policy.TrustedHosts = append([]string{}, policy.TrustedHosts...)
	}
	seen := map[string]bool{}
	for _, host := range policy.TrustedHosts {
		if !hostPattern.MatchString(host) || len(host) > 253 {
			return policy, errors.New("trusted archive hosts must be lowercase hostnames without ports")
		}
		if seen[host] {
			return policy, errors.New("duplicate trusted archive host")
		}
		seen[host] = true
	}
	if policy.Mode == "source-reference" && len(policy.TrustedHosts) == 0 {
		return policy, errors.New("source-reference mode requires explicit trusted archive hosts")
	}
	sort.Strings(policy.TrustedHosts)
	return policy, nil
}

func parsePackage(entry map[string]any, dev bool, policy SourcePolicy, repositories []string) (Package, error) {
	if err := validateObjectFields(entry, "require", "provide", "replace", "conflict", "autoload", "extra"); err != nil {
		return Package{}, err
	}
	pkg := Package{Dev: dev}
	pkg.Name, _ = entry["name"].(string)
	pkg.Version, _ = entry["version"].(string)
	pkg.Type, _ = entry["type"].(string)
	if v, exists := entry["type"]; exists {
		if _, ok := v.(string); !ok {
			return pkg, errors.New("package type must be a string")
		}
	}
	if pkg.Type == "" {
		pkg.Type = "library"
	}
	if !packagePattern.MatchString(pkg.Name) || !stableVersionPattern.MatchString(pkg.Version) && !branchVersionPattern.MatchString(pkg.Version) {
		return pkg, errors.New("locked package requires a safe name and a stable numeric or branch version")
	}
	if v, exists := entry["default-branch"]; exists {
		var ok bool
		if pkg.DefaultBranch, ok = v.(bool); !ok || pkg.DefaultBranch && !strings.HasPrefix(pkg.Version, "dev-") {
			return pkg, fmt.Errorf("%s has invalid default-branch metadata", pkg.Name)
		}
	}
	if pkg.Type != "library" && pkg.Type != "project" {
		return pkg, fmt.Errorf("%s has unsupported package type %s", pkg.Name, pkg.Type)
	}
	pkg.Path = "vendor/" + pkg.Name
	for _, field := range []string{"target-dir", "include-path", "installation-source"} {
		if _, exists := entry[field]; exists {
			return pkg, fmt.Errorf("%s: unsupported %s", pkg.Name, field)
		}
	}
	dist, ok := entry["dist"].(map[string]any)
	if !ok || dist["type"] != "zip" {
		return pkg, fmt.Errorf("%s requires a locked ZIP distribution", pkg.Name)
	}
	pkg.URL, _ = dist["url"].(string)
	pkg.Reference, _ = dist["reference"].(string)
	pkg.SHA1, _ = dist["shasum"].(string)
	if v, exists := dist["shasum"]; exists {
		if _, ok := v.(string); !ok {
			return pkg, fmt.Errorf("%s dist shasum must be a string", pkg.Name)
		}
	}
	if !referencePattern.MatchString(pkg.Reference) {
		return pkg, fmt.Errorf("%s requires a full immutable 40-digit reference", pkg.Name)
	}
	if pkg.SHA1 != "" && !referencePattern.MatchString(pkg.SHA1) {
		return pkg, fmt.Errorf("%s has invalid dist shasum", pkg.Name)
	}
	if policy.Mode == "checksum" && pkg.SHA1 == "" {
		return pkg, fmt.Errorf("%w: %s has no lock-published dist shasum; explicit source-reference policy is required", ErrSourcePolicy, pkg.Name)
	}
	if source, exists := entry["source"]; exists {
		obj, ok := source.(map[string]any)
		if !ok || obj["type"] != "git" || obj["reference"] != pkg.Reference {
			return pkg, fmt.Errorf("%s source and dist references must match immutable Git references", pkg.Name)
		}
		pkg.SourceURL, _ = obj["url"].(string)
	}
	if err := validateArchiveURL(pkg.URL, pkg.Reference, policy); err != nil {
		return pkg, fmt.Errorf("%s: %w", pkg.Name, err)
	}
	repository := archiveRepository(pkg.URL, pkg.Reference)
	if pkg.SourceURL != "" {
		if err := validateSourceRepository(pkg.SourceURL, repository); err != nil {
			return pkg, fmt.Errorf("%s: %w", pkg.Name, err)
		}
	}
	if !strings.HasPrefix(repository, "github.com/") && !containsString(repositories, repository) {
		return pkg, fmt.Errorf("%s: archive repository is not declared in composer.json repositories", pkg.Name)
	}
	var err error
	pkg.Autoload, err = parseAutoload(entry["autoload"])
	if err != nil {
		return pkg, fmt.Errorf("%s autoload: %w", pkg.Name, err)
	}
	pkg.Require, err = dependencyMap(entry["require"], pkg.Name+" require")
	if err != nil {
		return pkg, err
	}
	pkg.Provide, err = dependencyMap(entry["provide"], pkg.Name+" provide")
	if err != nil {
		return pkg, err
	}
	pkg.Replace, err = dependencyMap(entry["replace"], pkg.Name+" replace")
	if err != nil {
		return pkg, err
	}
	pkg.Conflict, err = dependencyMap(entry["conflict"], pkg.Name+" conflict")
	if err != nil {
		return pkg, err
	}
	if v, exists := entry["bin"]; exists {
		pkg.Bin, err = stringList(v, false)
		if err != nil {
			return pkg, fmt.Errorf("%s bin: %w", pkg.Name, err)
		}
		for _, bin := range pkg.Bin {
			if !safeRelativePath(bin, false) {
				return pkg, fmt.Errorf("%s has unsafe binary path", pkg.Name)
			}
		}
	}
	if v, exists := entry["extra"]; exists {
		var ok bool
		pkg.Extra, ok = v.(map[string]any)
		if !ok {
			return pkg, fmt.Errorf("%s extra must be an object", pkg.Name)
		}
	}
	return pkg, nil
}

func validateArchiveURL(raw, reference string, policy SourcePolicy) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("archive URL must be an absolute clean URL without credentials, query, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	_, gitlab := gitlabArchiveProject(u, reference)
	gitlab = gitlab && host != "api.github.com" && host != "codeload.github.com"
	if !gitlab && (u.RawQuery != "" || u.RawPath != "" || u.Path != path.Clean(u.Path)) {
		return errors.New("archive URL must be an absolute clean URL without credentials, query, or fragment")
	}
	if !gitlab && !loopback && host != "api.github.com" && host != "codeload.github.com" {
		return fmt.Errorf("%w: only GitHub repository archives and explicitly trusted GitLab archives are supported", ErrSourcePolicy)
	}
	if !loopback && u.Port() != "" && u.Port() != "443" {
		return errors.New("archive URLs must use the HTTPS default port")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return errors.New("archive URL requires HTTPS")
	}
	if !gitlab {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		valid := false
		if (host == "api.github.com" || loopback) && len(parts) == 5 && parts[0] == "repos" && parts[1] != "" && parts[2] != "" && parts[3] == "zipball" && parts[4] == reference {
			valid = true
		}
		if (host == "codeload.github.com" || loopback) && len(parts) == 4 && parts[0] != "" && parts[1] != "" && (parts[2] == "zip" || parts[2] == "legacy.zip") && parts[3] == reference {
			valid = true
		}
		if !valid {
			return errors.New("archive URL must contain an exact supported repository/ref path")
		}
	}
	// GitLab archives are never implicitly trusted, even under checksum policy.
	if gitlab || policy.Mode == "source-reference" {
		trusted := false
		for _, allowed := range policy.TrustedHosts {
			if host == allowed {
				trusted = true
			}
		}
		if !trusted {
			return fmt.Errorf("%w: archive host is outside the explicit source-reference trust policy", ErrSourcePolicy)
		}
	}
	return nil
}

// gitlabArchiveProject returns the namespaced project of a GitLab API archive
// whose query pins exactly one immutable commit.
func gitlabArchiveProject(u *url.URL, reference string) (string, bool) {
	const prefix, suffix = "/api/v4/projects/", "/repository/archive.zip"
	escaped := u.EscapedPath()
	if !strings.HasPrefix(escaped, prefix) || !strings.HasSuffix(escaped, suffix) || u.RawQuery != "sha="+reference || u.Fragment != "" {
		return "", false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(escaped, prefix), suffix)
	if strings.Contains(encoded, "/") {
		return "", false
	}
	project, err := url.PathUnescape(encoded)
	if err != nil || !repositoryPath(project) {
		return "", false
	}
	return project, true
}

func repositoryPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !repositorySegmentPattern.MatchString(part) || part == ".." {
			return false
		}
	}
	return true
}

// archiveRepository names the repository of a validated archive URL as
// host/path. GitHub API, codeload, and loopback fixture routes name github.com.
func archiveRepository(raw, reference string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if project, ok := gitlabArchiveProject(u, reference); ok && host != "api.github.com" && host != "codeload.github.com" {
		return repositoryKey(host, project)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "repos" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return ""
	}
	return repositoryKey("github.com", parts[0]+"/"+parts[1])
}

func repositoryKey(host, project string) string {
	return strings.ToLower(host) + "/" + strings.ToLower(strings.TrimSuffix(project, ".git"))
}

func validateSourceRepository(sourceRaw, repository string) error {
	source, err := url.Parse(sourceRaw)
	if err != nil || source.Scheme != "https" || source.Host == "" || source.Port() != "" || source.User != nil || source.RawQuery != "" || source.Fragment != "" || source.RawPath != "" || source.Path != path.Clean(source.Path) {
		return errors.New("Git source provenance must use an exact HTTPS repository URL")
	}
	project := strings.Trim(source.Path, "/")
	if !repositoryPath(strings.TrimSuffix(project, ".git")) || (source.Host == "github.com" && strings.Count(project, "/") != 1) {
		return errors.New("Git source provenance must identify one repository")
	}
	if repositoryKey(source.Host, project) != repository {
		return errors.New("Git source repository does not match the locked distribution repository")
	}
	return nil
}

// parseRepositories accepts VCS repository declarations. Packages they supply
// still need immutable commit archives from an explicitly trusted host.
func parseRepositories(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	entries := []any{}
	switch v := value.(type) {
	case []any:
		entries = v
	case map[string]any:
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entries = append(entries, v[name])
		}
	default:
		return nil, errors.New("Composer repositories must be an array or object")
	}
	seen := map[string]bool{}
	result := []string{}
	for _, entry := range entries {
		repository, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("only VCS Composer repositories are supported; Packagist cannot be disabled")
		}
		for field := range repository {
			if field != "type" && field != "url" && field != "name" {
				return nil, fmt.Errorf("Composer repository field %s is not supported", field)
			}
		}
		if name, exists := repository["name"]; exists {
			if _, ok := name.(string); !ok {
				return nil, errors.New("Composer repository name must be a string")
			}
		}
		switch repository["type"] {
		case "vcs", "git", "github", "gitlab":
		default:
			return nil, fmt.Errorf("Composer repository type %v is not supported", repository["type"])
		}
		raw, _ := repository["url"].(string)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != path.Clean(u.Path) || !repositoryPath(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")) {
			return nil, errors.New("Composer VCS repositories require an exact HTTPS repository URL")
		}
		key := repositoryKey(u.Host, strings.Trim(u.Path, "/"))
		if !seen[key] {
			seen[key] = true
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result, nil
}

// Stability flags only steer Composer's solver; replay accepts them for
// direct requirements because the locked graph is already resolved.
func validateStabilityFlags(value any, direct map[string]string) error {
	if emptyJSON(value) {
		return nil
	}
	flags, ok := value.(map[string]any)
	if !ok {
		return errors.New("Composer stability-flags must be an object")
	}
	for name, flag := range flags {
		number, ok := flag.(json.Number)
		if _, declared := direct[name]; !ok || !declared {
			return fmt.Errorf("Composer stability flag for %s is not supported by frozen replay", name)
		}
		switch number.String() {
		case "0", "5", "10", "15", "20":
		default:
			return fmt.Errorf("invalid Composer stability flag for %s", name)
		}
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// branchConstraint reports the branch named by an exact branch constraint,
// ignoring a stability suffix and returning any #commit pin.
func branchConstraint(constraint string) (branch, reference string, ok bool) {
	constraint, _, _ = strings.Cut(constraint, "@")
	branch, reference, _ = strings.Cut(constraint, "#")
	return branch, reference, branchVersionPattern.MatchString(branch)
}

func branchSatisfies(pkg Package, constraint string) bool {
	branch, reference, ok := branchConstraint(constraint)
	return ok && branch == pkg.Version && (reference == "" || reference == pkg.Reference)
}

func validateObjectFields(obj map[string]any, fields ...string) error {
	for _, field := range fields {
		if v, exists := obj[field]; exists {
			if _, ok := v.(map[string]any); !ok {
				return fmt.Errorf("%s must be an object", field)
			}
		}
	}
	return nil
}

// ValidatePlan detects mutation of a loaded plan before an installer acts on it.
// Current source-byte checks are performed separately by installation.
func ValidatePlan(plan *Plan) error {
	if plan == nil || plan.Schema != "archer.composer-plan/v1" || !sha256Pattern.MatchString(plan.ManifestSHA256) || !sha256Pattern.MatchString(plan.LockSHA256) || !contentHashPattern.MatchString(plan.ContentHash) || !sha256Pattern.MatchString(plan.ID) {
		return errors.New("invalid Composer plan identity")
	}
	policy, err := normalizePolicy(plan.Policy)
	if err != nil {
		return err
	}
	if policy.Mode != plan.Policy.Mode || strings.Join(policy.TrustedHosts, "\x00") != strings.Join(plan.Policy.TrustedHosts, "\x00") {
		return errors.New("Composer policy is not normalized")
	}
	copyPlan := *plan
	copyPlan.ID = ""
	expected, err := parallax.Hash(copyPlan)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(plan.ID)) != 1 {
		return errors.New("Composer plan was modified")
	}
	return nil
}

func (plan *Plan) Validate() error { return ValidatePlan(plan) }

// CheckRuntime verifies real PHP and extension versions, without accepting
// Composer platform overrides or a lock's declarations as runtime evidence.
func CheckRuntime(plan *Plan, info RuntimeInfo) error {
	if err := ValidatePlan(plan); err != nil {
		return err
	}
	if _, err := parseVersion(info.PHPVersion); err != nil {
		return fmt.Errorf("PHP runtime: %w", err)
	}
	for _, req := range plan.PlatformRequirements {
		version := info.PHPVersion
		if req.Name != "php" {
			name := strings.TrimPrefix(req.Name, "ext-")
			var exists bool
			version, exists = info.Extensions[name]
			if !exists {
				return fmt.Errorf("%s requires missing PHP extension %s", req.From, name)
			}
			if req.Constraint == "*" {
				continue
			}
			if version == "" {
				return fmt.Errorf("extension %s does not report a version for constraint %s", name, req.Constraint)
			}
		}
		ok, err := Satisfies(version, req.Constraint)
		if err != nil {
			return fmt.Errorf("%s requires %s %s: %w", req.From, req.Name, req.Constraint, err)
		}
		if !ok {
			return fmt.Errorf("%s requires %s %s; runtime reports %s", req.From, req.Name, req.Constraint, version)
		}
	}
	for _, req := range plan.PlatformConflicts {
		version := info.PHPVersion
		if req.Name != "php" {
			var exists bool
			version, exists = info.Extensions[strings.TrimPrefix(req.Name, "ext-")]
			if !exists {
				continue
			}
		}
		if req.Constraint == "*" {
			return fmt.Errorf("%s conflicts with runtime %s", req.From, req.Name)
		}
		ok, err := Satisfies(version, req.Constraint)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%s conflicts with %s %s", req.From, req.Name, version)
		}
	}
	return nil
}

func validateGraph(plan *Plan) error {
	byName := map[string]Package{}
	for _, pkg := range plan.Packages {
		byName[pkg.Name] = pkg
	}
	providers := map[string][]struct{ version, constraint string }{}
	for _, pkg := range plan.Packages {
		for _, group := range []map[string]string{pkg.Provide, pkg.Replace} {
			for name, c := range group {
				if c == "self.version" {
					c = pkg.Version
				}
				if _, _, branch := branchConstraint(c); branch {
					providers[name] = append(providers[name], struct{ version, constraint string }{pkg.Version, c})
					continue
				}
				if _, err := parseConstraint(c); err != nil {
					return fmt.Errorf("%s provides/replaces %s: %w", pkg.Name, name, err)
				}
				providers[name] = append(providers[name], struct{ version, constraint string }{pkg.Version, c})
			}
		}
	}
	groups := []struct {
		from string
		deps map[string]string
	}{{plan.Name, plan.Direct}}
	for _, pkg := range plan.Packages {
		groups = append(groups, struct {
			from string
			deps map[string]string
		}{pkg.Name, pkg.Require})
	}
	for _, group := range groups {
		for name, c := range group.deps {
			if branch, _, ok := branchConstraint(c); ok {
				satisfied := false
				if pkg, exists := byName[name]; exists {
					satisfied = branchSatisfies(pkg, c)
				}
				for _, provider := range providers[name] {
					if provider.constraint == branch {
						satisfied = true
					}
				}
				if !satisfied {
					return fmt.Errorf("%s requires %s %s, absent or incompatible in the frozen lock", group.from, name, c)
				}
				continue
			}
			if _, err := parseConstraint(c); err != nil {
				return fmt.Errorf("%s requires %s: %w", group.from, name, err)
			}
			if isPlatform(name) {
				continue
			}
			if name == "composer-runtime-api" {
				ok, err := Satisfies(RuntimeAPIVersion, c)
				if err != nil || !ok {
					return fmt.Errorf("%s requires unsupported composer-runtime-api %s", group.from, c)
				}
				continue
			}
			satisfied := false
			if pkg, exists := byName[name]; exists && !branchVersionPattern.MatchString(pkg.Version) {
				ok, err := Satisfies(pkg.Version, c)
				if err != nil {
					return err
				}
				satisfied = ok
			}
			if !satisfied {
				for _, provider := range providers[name] {
					if _, _, branch := branchConstraint(provider.constraint); branch {
						continue
					}
					ok, err := constraintsOverlap(c, provider.constraint)
					if err != nil {
						return err
					}
					if ok {
						satisfied = true
						break
					}
				}
			}
			if !satisfied {
				return fmt.Errorf("%s requires %s %s, absent or incompatible in the frozen lock", group.from, name, c)
			}
		}
	}
	conflicts := []struct {
		from string
		deps map[string]string
	}{{plan.Name, plan.Conflict}}
	for _, pkg := range plan.Packages {
		conflicts = append(conflicts, struct {
			from string
			deps map[string]string
		}{pkg.Name, pkg.Conflict})
	}
	for _, group := range conflicts {
		for name, c := range group.deps {
			if _, _, ok := branchConstraint(c); ok {
				if pkg, exists := byName[name]; exists && pkg.Name != group.from && branchSatisfies(pkg, c) {
					return fmt.Errorf("%s conflicts with locked %s %s", group.from, name, pkg.Version)
				}
				continue
			}
			if _, err := parseConstraint(c); err != nil {
				return fmt.Errorf("%s conflicts with %s: %w", group.from, name, err)
			}
			if isPlatform(name) {
				continue
			}
			if name == "composer-runtime-api" {
				ok, err := Satisfies(RuntimeAPIVersion, c)
				if err != nil {
					return err
				}
				if ok {
					return fmt.Errorf("%s conflicts with the supported Composer runtime API", group.from)
				}
				continue
			}
			if pkg, exists := byName[name]; exists && pkg.Name != group.from {
				if branchVersionPattern.MatchString(pkg.Version) {
					return fmt.Errorf("%s conflict %s with branch version %s of %s cannot be evaluated by frozen replay", group.from, c, pkg.Version, name)
				}
				ok, err := Satisfies(pkg.Version, c)
				if err != nil {
					return err
				}
				if ok {
					return fmt.Errorf("%s conflicts with locked %s %s", group.from, name, pkg.Version)
				}
			}
		}
	}
	return nil
}

func dependencyMap(value any, label string) (map[string]string, error) {
	result := map[string]string{}
	if value == nil {
		return result, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	for name, v := range obj {
		c, ok := v.(string)
		if !ok || c == "" || c != strings.TrimSpace(c) {
			return nil, fmt.Errorf("%s has invalid requirement for %s", label, name)
		}
		if !packagePattern.MatchString(name) && !isPlatform(name) && name != "composer-runtime-api" {
			return nil, fmt.Errorf("%s has unsupported requirement %s", label, name)
		}
		result[name] = c
	}
	return result, nil
}

func isPlatform(name string) bool { return name == "php" || extensionPattern.MatchString(name) }
func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
func emptyJSON(v any) bool {
	if v == nil {
		return true
	}
	switch v := v.(type) {
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}
func bytesHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func parseAutoload(value any) (Autoload, error) {
	a := Autoload{PSR4: map[string][]string{}, PSR0: map[string][]string{}}
	if value == nil {
		return a, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return a, errors.New("autoload must be an object")
	}
	for field, value := range obj {
		switch field {
		case "psr-4", "psr-0":
			m, ok := value.(map[string]any)
			if !ok {
				return a, errors.New("PSR autoload maps must be objects")
			}
			target := a.PSR4
			if field == "psr-0" {
				target = a.PSR0
			}
			for prefix, value := range m {
				if strings.ContainsAny(prefix, "\x00\r\n/\"'") {
					return a, errors.New("invalid autoload prefix")
				}
				if field == "psr-4" && prefix != "" && !strings.HasSuffix(prefix, "\\") {
					return a, errors.New("PSR-4 namespace prefix must end in a backslash")
				}
				paths, err := stringList(value, true)
				if err != nil {
					return a, err
				}
				for _, p := range paths {
					if !safeRelativePath(p, true) {
						return a, fmt.Errorf("unsafe autoload path %q", p)
					}
				}
				target[prefix] = paths
			}
		case "classmap", "files", "exclude-from-classmap":
			list, err := stringList(value, false)
			if err != nil {
				return a, err
			}
			for _, p := range list {
				candidate := p
				if field == "exclude-from-classmap" {
					candidate = strings.TrimPrefix(candidate, "/")
				}
				if !safeRelativePath(candidate, field == "classmap" || field == "exclude-from-classmap") {
					return a, fmt.Errorf("unsafe autoload path %q", p)
				}
			}
			switch field {
			case "classmap":
				a.Classmap = list
			case "files":
				a.Files = list
			case "exclude-from-classmap":
				a.ExcludeFromClassmap = list
			}
		default:
			return a, fmt.Errorf("unsupported autoload field %s", field)
		}
	}
	return a, nil
}

func stringList(value any, allowString bool) ([]string, error) {
	if allowString {
		if str, ok := value.(string); ok {
			return []string{str}, nil
		}
	}
	list, ok := value.([]any)
	if !ok {
		return nil, errors.New("expected an array of strings")
	}
	result := []string{}
	for _, v := range list {
		str, ok := v.(string)
		if !ok {
			return nil, errors.New("expected an array of strings")
		}
		result = append(result, str)
	}
	return result, nil
}

func safeRelativePath(p string, allowEmpty bool) bool {
	if p == "" || p == "." || p == "./" {
		return allowEmpty
	}
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\:\x00\r\n") || strings.Contains(p, "//") {
		return false
	}
	for _, component := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if component == ".." || component == "" {
			return false
		}
	}
	return true
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	stat, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("source must be a regular file without symlinks")
	}
	if stat.Size() > limit {
		return nil, fmt.Errorf("source exceeds %d bytes", limit)
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("source exceeds %d bytes", limit)
	}
	return data, nil
}

// Composer only sorts the top-level dependency-relevant keys. Nested object
// insertion order, escaped slashes/Unicode, and PHP's empty-array shape affect
// this compatibility MD5; it is not an archive integrity hash.
func ContentHash(data []byte) (string, error) {
	value, err := decodeOrdered(data)
	if err != nil {
		return "", err
	}
	root, ok := value.(orderedObject)
	if !ok {
		return "", errors.New("composer.json must be an object")
	}
	relevant := map[string]any{}
	keys := map[string]bool{}
	for _, key := range []string{"name", "version", "require", "require-dev", "conflict", "replace", "provide", "minimum-stability", "prefer-stable", "repositories", "extra"} {
		keys[key] = true
	}
	for _, field := range root {
		if keys[field.key] {
			relevant[field.key] = field.value
		}
		if field.key == "config" {
			if config, ok := field.value.(orderedObject); ok {
				for _, f := range config {
					if f.key == "platform" && f.value != nil {
						relevant["config"] = orderedObject{{key: "platform", value: f.value}}
					}
				}
			}
		}
	}
	sorted := make([]string, 0, len(relevant))
	for key := range relevant {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	ordered := orderedObject{}
	for _, key := range sorted {
		ordered = append(ordered, orderedField{key, relevant[key]})
	}
	var buffer bytes.Buffer
	if err := encodePHPJSON(&buffer, ordered); err != nil {
		return "", err
	}
	sum := md5.Sum(buffer.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

type orderedField struct {
	key   string
	value any
}
type orderedObject []orderedField

func decodeObject(data []byte) (map[string]any, error) {
	v, err := decodeOrdered(data)
	if err != nil {
		return nil, err
	}
	obj, ok := toPlain(v).(map[string]any)
	if !ok {
		return nil, errors.New("JSON input must be an object")
	}
	return obj, nil
}
func decodeOrdered(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("JSON input must be valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON input")
		}
		return nil, err
	}
	return v, nil
}
func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting exceeds 128 levels")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		obj := orderedObject{}
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, errors.New("object key must be a string")
			}
			if seen[key] {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj = append(obj, orderedField{key, v})
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated object")
		}
		return obj, nil
	case '[':
		list := []any{}
		for d.More() {
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated array")
		}
		return list, nil
	}
	return nil, errors.New("unexpected JSON delimiter")
}
func toPlain(v any) any {
	switch v := v.(type) {
	case orderedObject:
		m := map[string]any{}
		for _, field := range v {
			m[field.key] = toPlain(field.value)
		}
		return m
	case []any:
		list := make([]any, len(v))
		for i, item := range v {
			list[i] = toPlain(item)
		}
		return list
	}
	return v
}
func encodePHPJSON(buffer *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case orderedObject:
		array := true
		for i, f := range value {
			if f.key != strconv.Itoa(i) {
				array = false
				break
			}
		}
		if array {
			buffer.WriteByte('[')
		} else {
			buffer.WriteByte('{')
		}
		for i, f := range value {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if !array {
				writePHPString(buffer, f.key)
				buffer.WriteByte(':')
			}
			if err := encodePHPJSON(buffer, f.value); err != nil {
				return err
			}
		}
		if array {
			buffer.WriteByte(']')
		} else {
			buffer.WriteByte('}')
		}
	case []any:
		buffer.WriteByte('[')
		for i, v := range value {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if err := encodePHPJSON(buffer, v); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case string:
		writePHPString(buffer, value)
	case json.Number:
		n, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return errors.New("Composer content-hash profile requires 64-bit integer JSON numbers")
		}
		buffer.WriteString(strconv.FormatInt(n, 10))
	case nil:
		buffer.WriteString("null")
	case bool:
		buffer.WriteString(strconv.FormatBool(value))
	default:
		return errors.New("unsupported Composer content-hash JSON value")
	}
	return nil
}
func writePHPString(buffer *bytes.Buffer, value string) {
	buffer.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\', '/':
			buffer.WriteByte('\\')
			buffer.WriteRune(r)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if r < 32 || r > 127 {
				if r > 0xffff {
					a, b := utf16.EncodeRune(r)
					fmt.Fprintf(buffer, `\u%04x\u%04x`, a, b)
				} else {
					fmt.Fprintf(buffer, `\u%04x`, r)
				}
			} else {
				buffer.WriteRune(r)
			}
		}
	}
	buffer.WriteByte('"')
}

type numericVersion [4]uint64
type interval struct {
	lower, upper                                   numericVersion
	hasLower, hasUpper, includeLower, includeUpper bool
}

func parseVersion(raw string) (numericVersion, error) {
	v := numericVersion{}
	raw = strings.TrimPrefix(raw, "v")
	if !stableVersionPattern.MatchString(raw) {
		return v, fmt.Errorf("unsupported stable numeric version %q", raw)
	}
	for i, part := range strings.Split(raw, ".") {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return v, errors.New("version segment is out of range")
		}
		v[i] = n
	}
	return v, nil
}
func compareVersion(a, b numericVersion) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
func incrementVersion(v numericVersion, index int) (numericVersion, error) {
	if v[index] >= 1<<32-1 {
		return v, errors.New("constraint upper bound is out of range")
	}
	v[index]++
	for i := index + 1; i < len(v); i++ {
		v[i] = 0
	}
	return v, nil
}
func intersect(a, b interval) (interval, bool) {
	result := a
	if b.hasLower && (!a.hasLower || compareVersion(b.lower, a.lower) > 0) {
		result.lower = b.lower
		result.hasLower = true
		result.includeLower = b.includeLower
	} else if b.hasLower && a.hasLower && compareVersion(b.lower, a.lower) == 0 {
		result.includeLower = a.includeLower && b.includeLower
	}
	if b.hasUpper && (!a.hasUpper || compareVersion(b.upper, a.upper) < 0) {
		result.upper = b.upper
		result.hasUpper = true
		result.includeUpper = b.includeUpper
	} else if b.hasUpper && a.hasUpper && compareVersion(b.upper, a.upper) == 0 {
		result.includeUpper = a.includeUpper && b.includeUpper
	}
	if result.hasLower && result.hasUpper {
		cmp := compareVersion(result.lower, result.upper)
		if cmp > 0 || cmp == 0 && (!result.includeLower || !result.includeUpper) {
			return result, false
		}
	}
	return result, true
}

var constraintOperatorWhitespace = regexp.MustCompile(`(>=|<=|!=|==|>|<|=|\^|~)\s+`)

func parseConstraint(raw string) ([]interval, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return nil, errors.New("empty or padded version constraint")
	}
	if strings.ContainsAny(raw, "@+()") || strings.Contains(raw, "dev") {
		return nil, fmt.Errorf("unsupported version constraint %q", raw)
	}
	branches := strings.Split(strings.ReplaceAll(raw, "||", "|"), "|")
	result := []interval{}
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			return nil, errors.New("empty version alternative")
		}
		if strings.Contains(branch, " - ") {
			parts := strings.Split(branch, " - ")
			if len(parts) != 2 {
				return nil, errors.New("invalid hyphen range")
			}
			lo, err := parseVersion(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, err
			}
			rhs := strings.TrimSpace(parts[1])
			hi, err := parseVersion(rhs)
			if err != nil {
				return nil, err
			}
			inclusive := true
			count := len(strings.Split(strings.TrimPrefix(rhs, "v"), "."))
			if count < 3 {
				hi, err = incrementVersion(hi, count-1)
				if err != nil {
					return nil, err
				}
				inclusive = false
			}
			r := interval{lower: lo, upper: hi, hasLower: true, hasUpper: true, includeLower: true, includeUpper: inclusive}
			if _, ok := intersect(r, interval{}); !ok {
				return nil, errors.New("empty hyphen range")
			}
			result = append(result, r)
			continue
		}
		branch = constraintOperatorWhitespace.ReplaceAllString(strings.ReplaceAll(branch, ",", " "), "$1")
		terms := strings.Fields(branch)
		r := interval{}
		valid := true
		for _, term := range terms {
			t, err := constraintTerm(term)
			if err != nil {
				return nil, err
			}
			if valid {
				r, valid = intersect(r, t)
			}
		}
		if valid {
			result = append(result, r)
		}
	}
	return result, nil
}
func constraintTerm(term string) (interval, error) {
	if term == "*" || term == "x" || term == "X" {
		return interval{}, nil
	}
	op := ""
	for _, prefix := range []string{">=", "<=", "==", "!=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(term, prefix) {
			op = prefix
			term = strings.TrimPrefix(term, prefix)
			break
		}
	}
	if op == "!=" {
		return interval{}, errors.New("version != exclusions are not supported")
	}
	parts := strings.Split(strings.TrimPrefix(term, "v"), ".")
	if len(parts) > 4 {
		return interval{}, errors.New("constraints support at most four version segments")
	}
	wildcard := -1
	for i, p := range parts {
		if p == "*" || p == "x" || p == "X" {
			if wildcard < 0 {
				wildcard = i
			}
		} else if wildcard >= 0 {
			return interval{}, errors.New("wildcard must end a constraint")
		}
	}
	if wildcard >= 0 {
		if op != "" && op != "=" && op != "==" {
			return interval{}, errors.New("wildcards with range operators are unsupported")
		}
		if wildcard == 0 {
			return interval{}, nil
		}
		base, err := parseVersion(strings.Join(parts[:wildcard], "."))
		if err != nil {
			return interval{}, err
		}
		upper, err := incrementVersion(base, wildcard-1)
		return interval{lower: base, upper: upper, hasLower: true, hasUpper: true, includeLower: true}, err
	}
	v, err := parseVersion(term)
	if err != nil {
		return interval{}, err
	}
	r := interval{}
	switch op {
	case "", "=", "==":
		r = interval{lower: v, upper: v, hasLower: true, hasUpper: true, includeLower: true, includeUpper: true}
	case ">":
		r = interval{lower: v, hasLower: true}
	case ">=":
		r = interval{lower: v, hasLower: true, includeLower: true}
	case "<":
		r = interval{upper: v, hasUpper: true}
	case "<=":
		r = interval{upper: v, hasUpper: true, includeUpper: true}
	case "^", "~":
		index := 0
		if op == "~" {
			if len(parts) >= 2 {
				index = len(parts) - 2
			}
		} else {
			for index < len(parts)-1 && index < 2 && v[index] == 0 {
				index++
			}
		}
		upper, err := incrementVersion(v, index)
		if err != nil {
			return r, err
		}
		r = interval{lower: v, upper: upper, hasLower: true, hasUpper: true, includeLower: true}
	}
	return r, nil
}

// Satisfies deliberately supports stable numeric Composer constraints only.
// Unsupported prereleases, aliases, exclusions, and stability flags fail closed.
func Satisfies(version, constraint string) (bool, error) {
	v, err := parseVersion(version)
	if err != nil {
		return false, err
	}
	ranges, err := parseConstraint(constraint)
	if err != nil {
		return false, err
	}
	for _, r := range ranges {
		if r.hasLower {
			cmp := compareVersion(v, r.lower)
			if cmp < 0 || cmp == 0 && !r.includeLower {
				continue
			}
		}
		if r.hasUpper {
			cmp := compareVersion(v, r.upper)
			if cmp > 0 || cmp == 0 && !r.includeUpper {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}
func constraintsOverlap(a, b string) (bool, error) {
	first, err := parseConstraint(a)
	if err != nil {
		return false, err
	}
	second, err := parseConstraint(b)
	if err != nil {
		return false, err
	}
	for _, x := range first {
		for _, y := range second {
			if _, ok := intersect(x, y); ok {
				return true, nil
			}
		}
	}
	return false, nil
}
