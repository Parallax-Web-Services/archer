// Package npm replays the placement and integrity recorded in npm v3 lockfiles.
// It does not resolve version ranges, peer dependencies, or mutable registry tags.
package npm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Parallax-Web-Services/archer/internal/parallax"
)

type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Libc string `json:"libc,omitempty"`
}

type Package struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Path             string            `json:"path"`
	URL              string            `json:"url"`
	Integrity        string            `json:"integrity"`
	Bin              map[string]string `json:"bin,omitempty"`
	Optional         bool              `json:"optional,omitempty"`
	Dev              bool              `json:"dev,omitempty"`
	HasInstallScript bool              `json:"has_install_script,omitempty"`
	NodeConstraint   string            `json:"node_constraint,omitempty"`
}

type Plan struct {
	Schema          string            `json:"schema"`
	ManifestSHA256  string            `json:"manifest_sha256"`
	LockSHA256      string            `json:"lock_sha256"`
	ID              string            `json:"id,omitempty"`
	Platform        Platform          `json:"platform"`
	Packages        []Package         `json:"packages"`
	Skipped         []Package         `json:"skipped"`
	Direct          map[string]string `json:"direct"`
	NodeConstraints []string          `json:"node_constraints"`
}

const maxManifestBytes = 2 << 20
const maxLockBytes = 16 << 20

var packageNamePattern = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
var exactVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var binNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var sourceDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var platformRulePattern = regexp.MustCompile(`^!?[a-z0-9][a-z0-9_-]*$`)

// Load binds exact source bytes and the normalized target platform. All entries
// are validated before optional platform filtering, then recorded paths are
// retained rather than flattened or independently resolved.
func Load(projectDir string, target Platform) (*Plan, error) {
	target, err := normalizePlatform(target)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(projectDir, "npm-shrinkwrap.json")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, fmt.Errorf("inspect npm-shrinkwrap.json: %w", err)
		}
		return nil, errors.New("npm-shrinkwrap.json takes precedence; shrinkwrap replay is not supported")
	}
	manifestBytes, err := readBoundedFile(filepath.Join(projectDir, "package.json"), maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	lockBytes, err := readBoundedFile(filepath.Join(projectDir, "package-lock.json"), maxLockBytes)
	if err != nil {
		return nil, fmt.Errorf("package-lock.json: %w", err)
	}
	manifest, err := decodeObject(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	lock, err := decodeObject(lockBytes)
	if err != nil {
		return nil, fmt.Errorf("package-lock.json: %w", err)
	}
	if lock["lockfileVersion"] != json.Number("3") {
		return nil, errors.New("only npm lockfileVersion 3 is supported")
	}
	if _, present := manifest["workspaces"]; present {
		return nil, errors.New("npm workspaces are not supported by frozen replay")
	}
	entries, ok := lock["packages"].(map[string]any)
	if !ok || entries == nil {
		return nil, errors.New("lock packages must be an object")
	}
	root, ok := entries[""].(map[string]any)
	if !ok || root == nil {
		return nil, errors.New("lock must contain its root package declaration")
	}
	if _, present := root["workspaces"]; present {
		return nil, errors.New("npm workspace locks are not supported")
	}
	direct := map[string]string{}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
		if value, present := manifest[field]; present && value == nil {
			return nil, fmt.Errorf("%s must be an object", field)
		}
		if value, present := root[field]; present && value == nil {
			return nil, fmt.Errorf("lock root %s must be an object", field)
		}
		declared, err := dependencyMap(manifest[field], field)
		if err != nil {
			return nil, err
		}
		locked, err := dependencyMap(root[field], "lock root "+field)
		if err != nil {
			return nil, err
		}
		if !sameMap(declared, locked) {
			return nil, fmt.Errorf("lock root %s differs from package.json", field)
		}
		for name, constraint := range declared {
			if previous, exists := direct[name]; exists && previous != constraint {
				return nil, fmt.Errorf("conflicting root declarations for %s are not supported", name)
			}
			direct[name] = constraint
		}
	}
	plan := &Plan{Schema: "archer.npm-plan/v1", ManifestSHA256: bytesHash(manifestBytes), LockSHA256: bytesHash(lockBytes), Platform: target, Packages: []Package{}, Skipped: []Package{}, Direct: direct, NodeConstraints: []string{}}
	constraints := map[string]bool{}
	if constraint, err := nodeConstraint(manifest); err != nil {
		return nil, err
	} else if constraint != "" {
		constraints[constraint] = true
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, recordedPath := range keys {
		entry, ok := entries[recordedPath].(map[string]any)
		if !ok || entry == nil {
			return nil, fmt.Errorf("package entry %s must be an object", recordedPath)
		}
		pkg, err := parsePackage(recordedPath, entry)
		if err != nil {
			return nil, err
		}
		matches, err := matchesPlatform(entry, target)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", recordedPath, err)
		}
		if !matches {
			if !pkg.Optional {
				return nil, fmt.Errorf("required package %s does not support the target platform", recordedPath)
			}
			plan.Skipped = append(plan.Skipped, pkg)
			continue
		}
		if pkg.HasInstallScript {
			return nil, fmt.Errorf("selected package %s requires an unsupported lifecycle script", recordedPath)
		}
		plan.Packages = append(plan.Packages, pkg)
		if pkg.NodeConstraint != "" {
			constraints[pkg.NodeConstraint] = true
		}
	}
	for name := range direct {
		if _, exists := entries["node_modules/"+name]; !exists {
			return nil, fmt.Errorf("root dependency %s is absent from the lock tree", name)
		}
	}
	if err := validateRootGraph(manifest, plan.Packages); err != nil {
		return nil, err
	}
	if err := validateGraph(entries, plan.Packages); err != nil {
		return nil, err
	}
	for constraint := range constraints {
		plan.NodeConstraints = append(plan.NodeConstraints, constraint)
	}
	sort.Strings(plan.NodeConstraints)
	plan.ID, err = parallax.Hash(plan)
	if err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// Validate verifies the integrity and shape of an in-memory plan. Its ID is an
// integrity binding; installation authorization and current source-byte checks
// remain the caller's responsibility.
func (plan *Plan) Validate() error {
	if plan == nil || plan.Schema != "archer.npm-plan/v1" || !sourceDigestPattern.MatchString(plan.ManifestSHA256) || !sourceDigestPattern.MatchString(plan.LockSHA256) || !sourceDigestPattern.MatchString(plan.ID) {
		return errors.New("invalid npm plan identity")
	}
	platform, err := normalizePlatform(plan.Platform)
	if err != nil || platform != plan.Platform {
		return errors.New("npm plan platform must be normalized")
	}
	if plan.Packages == nil || plan.Skipped == nil || plan.Direct == nil || plan.NodeConstraints == nil {
		return errors.New("npm plan collections must retain their JSON shapes")
	}
	placements := map[string]bool{}
	for _, group := range []struct {
		packages []Package
		skipped  bool
	}{{plan.Packages, false}, {plan.Skipped, true}} {
		previous := ""
		for _, pkg := range group.packages {
			if pkg.Path <= previous || placements[pkg.Path] {
				return errors.New("npm plan placements must be unique and sorted")
			}
			previous = pkg.Path
			placements[pkg.Path] = true
			entry := map[string]any{"name": pkg.Name, "version": pkg.Version, "resolved": pkg.URL, "integrity": pkg.Integrity, "optional": pkg.Optional, "dev": pkg.Dev, "hasInstallScript": pkg.HasInstallScript}
			if pkg.Bin != nil {
				bins := map[string]any{}
				for name, target := range pkg.Bin {
					bins[name] = target
				}
				entry["bin"] = bins
			}
			if pkg.NodeConstraint != "" {
				entry["engines"] = map[string]any{"node": pkg.NodeConstraint}
			}
			if _, err := parsePackage(pkg.Path, entry); err != nil {
				return err
			}
			if group.skipped && !pkg.Optional {
				return errors.New("npm plan may skip only optional packages")
			}
			if !group.skipped && pkg.HasInstallScript {
				return errors.New("npm plan selects an unsupported lifecycle script")
			}
		}
	}
	for name, constraint := range plan.Direct {
		if !packageNamePattern.MatchString(name) || strings.TrimSpace(constraint) == "" || !placements["node_modules/"+name] {
			return errors.New("npm plan direct dependency is invalid or missing")
		}
	}
	previous := ""
	for _, constraint := range plan.NodeConstraints {
		if strings.TrimSpace(constraint) == "" || constraint <= previous {
			return errors.New("npm plan engine constraints must be unique and sorted")
		}
		previous = constraint
	}
	body := *plan
	body.ID = ""
	digest, err := parallax.Hash(&body)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(digest), []byte(plan.ID)) != 1 {
		return errors.New("npm plan contents differ from its frozen ID")
	}
	return nil
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file, without symlinks")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return data, nil
}

func decodeObject(data []byte) (map[string]any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("JSON must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("JSON must contain exactly one value")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, errors.New("JSON input must be an object")
	}
	return object, nil
}

func readJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting exceeds 128 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid JSON object key")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		list := []any{}
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return list, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

func dependencyMap(value any, field string) (map[string]string, error) {
	result := map[string]string{}
	if value == nil {
		return result, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	for name, value := range object {
		constraint, ok := value.(string)
		if !packageNamePattern.MatchString(name) || !ok || strings.TrimSpace(constraint) == "" {
			return nil, fmt.Errorf("%s contains an invalid package declaration", field)
		}
		if strings.ContainsAny(constraint, ":/\\#") || strings.HasPrefix(constraint, ".") {
			return nil, fmt.Errorf("non-registry declaration %s is unsupported", name)
		}
		result[name] = constraint
	}
	return result, nil
}

func sameMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, constraint := range left {
		if right[name] != constraint {
			return false
		}
	}
	return true
}

func parsePackage(recordedPath string, entry map[string]any) (Package, error) {
	name, err := packagePathName(recordedPath)
	if err != nil {
		return Package{}, err
	}
	if link, exists := entry["link"]; exists && link != false {
		return Package{}, fmt.Errorf("linked package %s is unsupported", recordedPath)
	}
	if declared, exists := entry["name"]; exists && declared != name {
		return Package{}, fmt.Errorf("aliased package %s is unsupported", recordedPath)
	}
	version, ok := entry["version"].(string)
	if !ok || !exactVersionPattern.MatchString(version) {
		return Package{}, fmt.Errorf("package %s lacks an exact version", recordedPath)
	}
	resolved, ok := entry["resolved"].(string)
	if !ok {
		return Package{}, fmt.Errorf("package %s lacks a registry URL", recordedPath)
	}
	if err := validateURL(resolved); err != nil {
		return Package{}, fmt.Errorf("%s: %w", recordedPath, err)
	}
	integrity, ok := entry["integrity"].(string)
	if !ok || !strings.HasPrefix(integrity, "sha512-") {
		return Package{}, fmt.Errorf("package %s requires SHA512 SRI", recordedPath)
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(integrity, "sha512-"))
	if err != nil || len(digest) != 64 || strings.ContainsAny(integrity, "\r\n\t ") {
		return Package{}, fmt.Errorf("package %s has invalid SHA512 SRI", recordedPath)
	}
	pkg := Package{Name: name, Version: version, Path: recordedPath, URL: resolved, Integrity: integrity, Bin: map[string]string{}}
	for _, field := range []struct {
		name   string
		target *bool
	}{{"optional", &pkg.Optional}, {"dev", &pkg.Dev}, {"hasInstallScript", &pkg.HasInstallScript}} {
		if value, exists := entry[field.name]; exists {
			flag, ok := value.(bool)
			if !ok {
				return Package{}, fmt.Errorf("%s %s must be boolean", recordedPath, field.name)
			}
			*field.target = flag
		}
	}
	if _, exists := entry["inBundle"]; exists {
		return Package{}, fmt.Errorf("bundled package %s is unsupported", recordedPath)
	}
	if value, exists := entry["bin"]; exists {
		var bins map[string]any
		switch value := value.(type) {
		case map[string]any:
			bins = value
		case string:
			bins = map[string]any{path.Base(name): value}
		default:
			return Package{}, fmt.Errorf("%s bin must be an object or string", recordedPath)
		}
		for binName, value := range bins {
			target, ok := value.(string)
			if !binNamePattern.MatchString(binName) || !ok || !safeBinTarget(target) {
				return Package{}, fmt.Errorf("%s contains an unsafe executable link", recordedPath)
			}
			pkg.Bin[binName] = path.Clean(target)
		}
	}
	pkg.NodeConstraint, err = nodeConstraint(entry)
	if err != nil {
		return Package{}, fmt.Errorf("%s: %w", recordedPath, err)
	}
	return pkg, nil
}

func packagePathName(recordedPath string) (string, error) {
	parts := strings.Split(recordedPath, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("unsafe package path %q", recordedPath)
	}
	last := ""
	for i := 0; i < len(parts); {
		if parts[i] != "node_modules" || i+1 >= len(parts) {
			return "", fmt.Errorf("unsafe package path %q", recordedPath)
		}
		i++
		name := parts[i]
		i++
		if strings.HasPrefix(name, "@") {
			if i >= len(parts) {
				return "", fmt.Errorf("unsafe package path %q", recordedPath)
			}
			name += "/" + parts[i]
			i++
		}
		if !packageNamePattern.MatchString(name) {
			return "", fmt.Errorf("unsafe package path %q", recordedPath)
		}
		last = name
	}
	return last, nil
}

func safeBinTarget(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\:\x00\r\n\t") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	clean := path.Clean(value)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

// validateGraph checks recorded Node lookup placement, not version-range
// resolution. Exact source hashes bind the producer's frozen versions/ranges.
// Required dependencies and peers must exist in the selected tree; only
// explicitly optional edges may be absent on the target platform.
func validateGraph(entries map[string]any, selected []Package) error {
	present := map[string]bool{}
	for _, pkg := range selected {
		present[pkg.Path] = true
	}
	for _, pkg := range selected {
		if index := strings.LastIndex(pkg.Path, "/node_modules/"); index >= 0 && !present[pkg.Path[:index]] {
			return fmt.Errorf("selected nested package %s has a missing parent package", pkg.Path)
		}
		entry := entries[pkg.Path].(map[string]any)
		required, err := graphDependencies(entry, "dependencies", pkg.Path)
		if err != nil {
			return err
		}
		optional, err := graphDependencies(entry, "optionalDependencies", pkg.Path)
		if err != nil {
			return err
		}
		peers, err := graphDependencies(entry, "peerDependencies", pkg.Path)
		if err != nil {
			return err
		}
		optionalPeers := map[string]bool{}
		if metadata, exists := entry["peerDependenciesMeta"]; exists {
			object, ok := metadata.(map[string]any)
			if !ok {
				return fmt.Errorf("%s peerDependenciesMeta must be an object", pkg.Path)
			}
			for name, value := range object {
				meta, ok := value.(map[string]any)
				if !packageNamePattern.MatchString(name) || !ok {
					return fmt.Errorf("%s has invalid peer metadata", pkg.Path)
				}
				if value, exists := meta["optional"]; exists {
					flag, ok := value.(bool)
					if !ok {
						return fmt.Errorf("%s optional peer flag must be boolean", pkg.Path)
					}
					optionalPeers[name] = flag
				}
			}
		}
		for name := range optional {
			delete(required, name)
		}
		for _, edge := range []struct {
			dependencies map[string]string
			optional     map[string]bool
			kind         string
		}{
			{required, nil, "dependency"}, {peers, optionalPeers, "peer dependency"},
		} {
			for name := range edge.dependencies {
				if resolvePlacement(pkg.Path, name, present) == "" && !edge.optional[name] {
					return fmt.Errorf("%s missing required %s %s in recorded Node lookup paths", pkg.Path, edge.kind, name)
				}
			}
		}
	}
	return nil
}

func validateRootGraph(manifest map[string]any, selected []Package) error {
	present := map[string]bool{}
	for _, pkg := range selected {
		present[pkg.Path] = true
	}
	optional, err := dependencyMap(manifest["optionalDependencies"], "root optionalDependencies")
	if err != nil {
		return err
	}
	for _, field := range []string{"dependencies", "devDependencies"} {
		required, err := dependencyMap(manifest[field], "root "+field)
		if err != nil {
			return err
		}
		for name := range required {
			if _, isOptional := optional[name]; !isOptional && !present["node_modules/"+name] {
				return fmt.Errorf("root required dependency %s is absent from the selected tree", name)
			}
		}
	}
	return nil
}

func graphDependencies(entry map[string]any, field, packagePath string) (map[string]string, error) {
	value, exists := entry[field]
	if exists && value == nil {
		return nil, fmt.Errorf("%s %s must be an object", packagePath, field)
	}
	dependencies, err := dependencyMap(value, packagePath+" "+field)
	return dependencies, err
}

func resolvePlacement(consumer, dependency string, present map[string]bool) string {
	for directory := consumer; ; directory = path.Dir(directory) {
		if path.Base(directory) != "node_modules" {
			candidate := path.Join(directory, "node_modules", dependency)
			if present[candidate] {
				return candidate
			}
		}
		if directory == "." {
			return ""
		}
	}
}

func validateURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("package URL must be absolute without credentials or fragments")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("package URL requires HTTPS or direct loopback HTTP")
}

func nodeConstraint(entry map[string]any) (string, error) {
	value, exists := entry["engines"]
	if !exists {
		return "", nil
	}
	engines, ok := value.(map[string]any)
	if !ok {
		return "", errors.New("engines must be an object")
	}
	value, exists = engines["node"]
	if !exists {
		return "", nil
	}
	constraint, ok := value.(string)
	if !ok || strings.TrimSpace(constraint) == "" {
		return "", errors.New("Node engine constraint must be a nonempty string")
	}
	return constraint, nil
}

func matchesPlatform(entry map[string]any, target Platform) (bool, error) {
	matches := true
	for _, axis := range []struct{ name, value string }{{"os", target.OS}, {"cpu", target.Arch}, {"libc", target.Libc}} {
		value, exists := entry[axis.name]
		if !exists {
			continue
		}
		rules, err := parallax.StringList(value, axis.name)
		if err != nil {
			return false, err
		}
		positive, accepted := false, false
		for _, rule := range rules {
			if !platformRulePattern.MatchString(rule) {
				return false, fmt.Errorf("invalid %s platform rule", axis.name)
			}
			if rule == "!"+axis.value {
				matches = false
			}
			if strings.HasPrefix(rule, "!") {
				continue
			}
			positive = true
			if rule == axis.value || rule == "any" {
				accepted = true
			}
		}
		if positive && !accepted {
			matches = false
		}
	}
	return matches, nil
}

func normalizePlatform(target Platform) (Platform, error) {
	if target.OS == "windows" {
		target.OS = "win32"
	}
	switch target.Arch {
	case "amd64":
		target.Arch = "x64"
	case "386":
		target.Arch = "ia32"
	case "aarch64":
		target.Arch = "arm64"
	}
	if target.OS == "" || target.Arch == "" || strings.ContainsAny(target.OS+target.Arch, "/\\:. \t\r\n") {
		return Platform{}, errors.New("target OS and CPU must be specified")
	}
	if target.OS == "linux" {
		if target.Libc != "glibc" && target.Libc != "musl" {
			return Platform{}, errors.New("Linux target requires glibc or musl")
		}
	} else if target.Libc != "" {
		return Platform{}, errors.New("libc selection applies only to Linux")
	}
	return target, nil
}

// CurrentPlatform detects the execution host; caller-supplied planning fields
// cannot substitute for this runtime evidence during installation.
func CurrentPlatform() (Platform, error) {
	target := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if target.OS == "linux" {
		for _, probe := range [][]string{{"getconf", "GNU_LIBC_VERSION"}, {"ldd", "--version"}} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			command := exec.CommandContext(ctx, probe[0], probe[1:]...)
			output := &boundedOutput{limit: 4096}
			command.Stdout, command.Stderr = output, output
			err := command.Run()
			cancel()
			text := strings.ToLower(output.buffer.String())
			if strings.Contains(text, "musl") {
				target.Libc = "musl"
				break
			}
			if err == nil && (strings.Contains(text, "glibc") || strings.Contains(text, "gnu libc") || strings.Contains(text, "gnu c library")) {
				target.Libc = "glibc"
				break
			}
		}
		if target.Libc == "" {
			return Platform{}, errors.New("could not verify Linux libc; specify a target only for planning")
		}
	}
	return normalizePlatform(target)
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	length := len(data)
	remaining := output.limit - output.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = output.buffer.Write(data)
	}
	return length, nil
}

func bytesHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
