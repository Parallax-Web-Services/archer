package npm

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var linuxTarget = Platform{OS: "linux", Arch: "amd64", Libc: "glibc"}

func smallFixture() (map[string]any, map[string]any) {
	manifest := map[string]any{"dependencies": map[string]any{"alpha": "^1.0.0"}, "devDependencies": map[string]any{"@scope/tool": "~2.0.0"}, "optionalDependencies": map[string]any{"mac-only": "1.0.0"}, "engines": map[string]any{"node": ">=22"}}
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	entry := func(version string) map[string]any {
		return map[string]any{"version": version, "resolved": "https://registry.npmjs.org/package/-/package-" + version + ".tgz", "integrity": integrity}
	}
	alpha := entry("1.0.2")
	alpha["engines"] = map[string]any{"node": ">=18"}
	tool := entry("2.0.7")
	tool["dev"] = true
	tool["bin"] = map[string]any{"tool": "./bin/tool.js"}
	mac := entry("1.0.0")
	mac["optional"] = true
	mac["os"] = []any{"darwin"}
	mac["hasInstallScript"] = true
	lock := map[string]any{"lockfileVersion": 3, "packages": map[string]any{"": manifest, "node_modules/alpha": alpha, "node_modules/@scope/tool": tool, "node_modules/mac-only": mac, "node_modules/alpha/node_modules/nested": entry("3.1.0")}}
	return manifest, lock
}

func writeFixture(t *testing.T, manifest, lock map[string]any) string {
	t.Helper()
	directory := t.TempDir()
	for name, value := range map[string]map[string]any{"package.json": manifest, "package-lock.json": lock} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestLoadReplaysExactPathsAndOptionalPlatformSelection(t *testing.T) {
	manifest, lock := smallFixture()
	directory := writeFixture(t, manifest, lock)
	plan, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packages) != 3 || len(plan.Skipped) != 1 || len(plan.Direct) != 3 {
		t.Fatalf("wrong partition: %#v", plan)
	}
	if plan.Platform.OS != "linux" || plan.Platform.Arch != "x64" || plan.Platform.Libc != "glibc" {
		t.Fatalf("platform not normalized: %#v", plan.Platform)
	}
	if plan.Packages[0].Path != "node_modules/@scope/tool" || plan.Packages[2].Path != "node_modules/alpha/node_modules/nested" {
		t.Fatal("recorded tree placement lost")
	}
	if plan.Packages[0].Bin["tool"] != "bin/tool.js" || !plan.Skipped[0].HasInstallScript {
		t.Fatal("bin or skipped script metadata lost")
	}
	if strings.Join(plan.NodeConstraints, ",") != ">=18,>=22" {
		t.Fatalf("engine constraints: %v", plan.NodeConstraints)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	other, err := Load(directory, Platform{OS: "linux", Arch: "x64", Libc: "glibc"})
	if err != nil || plan.ID != other.ID {
		t.Fatalf("equivalent target changed identity: %v", err)
	}
}

func TestPlanIdentityBindsSourcesPlatformAndContents(t *testing.T) {
	manifest, lock := smallFixture()
	directory := writeFixture(t, manifest, lock)
	plan, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	plan.Packages[0].URL = "https://registry.npmjs.org/changed.tgz"
	if err := plan.Validate(); err == nil {
		t.Fatal("mutated in-memory plan accepted")
	}
	original, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "package.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if original.ID == changed.ID || original.ManifestSHA256 == changed.ManifestSHA256 {
		t.Fatal("source byte change did not freeze a new plan")
	}
	musl, err := Load(directory, Platform{OS: "linux", Arch: "x64", Libc: "musl"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == musl.ID {
		t.Fatal("target platform was not bound")
	}
}

func TestRootDeclarationsMustMatchAndExist(t *testing.T) {
	for _, mutation := range []string{"constraint", "missing", "kind", "protocol", "workspace", "null"} {
		t.Run(mutation, func(t *testing.T) {
			manifest, lock := smallFixture()
			entries := lock["packages"].(map[string]any)
			// Detach lock root so root manifest mutations do not silently change both.
			root := map[string]any{}
			for key, value := range manifest {
				root[key] = value
			}
			entries[""] = root
			switch mutation {
			case "constraint":
				root["dependencies"] = map[string]any{"alpha": "^2.0.0"}
			case "missing":
				delete(entries, "node_modules/alpha")
			case "kind":
				root["dependencies"] = []any{}
			case "protocol":
				manifest["dependencies"] = map[string]any{"alpha": "file:../alpha"}
				root["dependencies"] = manifest["dependencies"]
			case "workspace":
				manifest["workspaces"] = []any{"packages/*"}
			case "null":
				manifest["dependencies"] = nil
				root["dependencies"] = nil
			}
			if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
				t.Fatal("invalid root declaration accepted")
			}
		})
	}
}

func TestRejectsUnsafeEntriesAndUnsupportedLifecycle(t *testing.T) {
	for _, mutation := range []string{"path", "alias", "link", "version", "url", "credentials", "integrity", "bin", "script", "required-platform", "platform-shape", "bundle"} {
		t.Run(mutation, func(t *testing.T) {
			manifest, lock := smallFixture()
			entries := lock["packages"].(map[string]any)
			entry := entries["node_modules/alpha"].(map[string]any)
			switch mutation {
			case "path":
				entries["node_modules/../escape"] = entry
			case "alias":
				entry["name"] = "different"
			case "link":
				entry["link"] = true
			case "version":
				entry["version"] = "github:owner/repo"
			case "url":
				entry["resolved"] = "http://example.com/package.tgz"
			case "credentials":
				entry["resolved"] = "https://token@example.com/package.tgz"
			case "integrity":
				entry["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 63))
			case "bin":
				entry["bin"] = map[string]any{"alpha": "../escape"}
			case "script":
				entry["hasInstallScript"] = true
			case "required-platform":
				entry["os"] = []any{"darwin"}
			case "platform-shape":
				entry["os"] = "linux"
			case "bundle":
				entry["inBundle"] = true
			}
			if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
				t.Fatal("unsafe or unsupported entry accepted")
			}
		})
	}
}

func TestPlatformLibcAndNegativeRules(t *testing.T) {
	manifest, lock := smallFixture()
	entries := lock["packages"].(map[string]any)
	entry := entries["node_modules/alpha"].(map[string]any)
	delete(entries, "node_modules/alpha/node_modules/nested")
	delete(manifest["dependencies"].(map[string]any), "alpha")
	manifest["optionalDependencies"].(map[string]any)["alpha"] = "^1.0.0"
	entry["optional"] = true
	entry["os"] = []any{"linux", "!darwin"}
	entry["cpu"] = []any{"x64"}
	entry["libc"] = []any{"musl"}
	directory := writeFixture(t, manifest, lock)
	glibc, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(glibc.Skipped) != 2 {
		t.Fatalf("glibc partition: %v", glibc.Skipped)
	}
	musl, err := Load(directory, Platform{OS: "linux", Arch: "amd64", Libc: "musl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(musl.Skipped) != 1 {
		t.Fatalf("musl partition: %v", musl.Skipped)
	}
	if _, err := Load(directory, Platform{OS: "linux", Arch: "x64"}); err == nil {
		t.Fatal("unknown Linux libc accepted")
	}
	// An early OS mismatch must not hide malformed metadata in later axes.
	entry["os"] = []any{"darwin"}
	entry["cpu"] = "x64"
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
		t.Fatal("skipped package's malformed CPU predicate accepted")
	}
}

func TestRootRequiredAndNestedParentMustRemainSelected(t *testing.T) {
	manifest, lock := smallFixture()
	entries := lock["packages"].(map[string]any)
	entry := entries["node_modules/alpha"].(map[string]any)
	entry["optional"] = true
	entry["os"] = []any{"darwin"}
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
		t.Fatal("required root dependency hidden by lock optional flag")
	}
	delete(manifest["dependencies"].(map[string]any), "alpha")
	manifest["optionalDependencies"].(map[string]any)["alpha"] = "^1.0.0"
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
		t.Fatal("orphan nested package selected beneath skipped parent")
	}
	nested := entries["node_modules/alpha/node_modules/nested"].(map[string]any)
	nested["optional"] = true
	nested["os"] = []any{"darwin"}
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err != nil {
		t.Fatalf("fully excluded optional subtree rejected: %v", err)
	}
}

func TestJSONLimitsAndAmbiguity(t *testing.T) {
	for _, input := range []string{`[]`, `{} {}`, `{"a":1,"a":2}`, `{"packages":{"x":1,"x":2}}`, `{"n":` + strings.Repeat("[", 130) + strings.Repeat("]", 130) + `}`} {
		if _, err := decodeObject([]byte(input)); err == nil {
			t.Fatalf("ambiguous JSON accepted: %.60s", input)
		}
	}
	manifest, lock := smallFixture()
	directory := writeFixture(t, manifest, lock)
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(strings.Repeat(" ", maxManifestBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directory, linuxTarget); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	if err := os.WriteFile(filepath.Join(directory, "package-lock.json"), []byte(strings.Repeat(" ", maxLockBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(directory, "package.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directory, linuxTarget); err == nil {
		t.Fatal("oversized lock accepted")
	}
}

func TestLoopbackFixtureURLAndInvalidPaths(t *testing.T) {
	if err := validateURL("http://127.0.0.1:1234/pkg.tgz"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"file:///pkg.tgz", "git+https://example.test/pkg", "http://localhost/pkg.tgz", "https://example.test/pkg#fragment", "https://:443/package.tgz"} {
		if err := validateURL(value); err == nil {
			t.Fatalf("invalid source URL %s", value)
		}
	}
	for _, value := range []string{"node_modules/alpha/extra", "node_modules/@scope", "node_modules/.bin/tool", "node_modules/alpha/../beta", "/node_modules/alpha", "node_modules\\alpha"} {
		if _, err := packagePathName(value); err == nil {
			t.Fatalf("invalid placement %s", value)
		}
	}
}

func TestRequiredGraphUsesRecordedAncestorLookup(t *testing.T) {
	manifest, lock := smallFixture()
	entries := lock["packages"].(map[string]any)
	entries["node_modules/alpha"].(map[string]any)["dependencies"] = map[string]any{"nested": "^3.0.0"}
	entries["node_modules/alpha/node_modules/nested"].(map[string]any)["dependencies"] = map[string]any{"@scope/tool": "^2.0.0"}
	entries["node_modules/alpha/node_modules/nested"].(map[string]any)["peerDependencies"] = map[string]any{"alpha": "^1.0.0"}
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err != nil {
		t.Fatalf("valid nested/ancestor graph rejected: %v", err)
	}
	delete(entries, "node_modules/alpha/node_modules/nested")
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
		t.Fatal("missing required transitive dependency accepted")
	}
}

func TestPeersAndOptionalEdgesAreAccountedFor(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run("peer-optional="+map[bool]string{false: "false", true: "true"}[optional], func(t *testing.T) {
			manifest, lock := smallFixture()
			entry := lock["packages"].(map[string]any)["node_modules/alpha"].(map[string]any)
			entry["peerDependencies"] = map[string]any{"absent-peer": "^1"}
			entry["peerDependenciesMeta"] = map[string]any{"absent-peer": map[string]any{"optional": optional}}
			_, err := Load(writeFixture(t, manifest, lock), linuxTarget)
			if (err == nil) != optional {
				t.Fatalf("peer optional=%v: %v", optional, err)
			}
		})
	}
	manifest, lock := smallFixture()
	entry := lock["packages"].(map[string]any)["node_modules/alpha"].(map[string]any)
	entry["dependencies"] = map[string]any{"absent-optional": "^1"}
	entry["optionalDependencies"] = map[string]any{"absent-optional": "^1"}
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err != nil {
		t.Fatalf("explicit optional edge rejected: %v", err)
	}
	entry["dependencies"] = map[string]any{"absent-optional": "workspace:*"}
	if _, err := Load(writeFixture(t, manifest, lock), linuxTarget); err == nil {
		t.Fatal("unsupported transitive protocol accepted")
	}
}

func TestShrinkwrapPrecedenceFailsExplicitly(t *testing.T) {
	manifest, lock := smallFixture()
	directory := writeFixture(t, manifest, lock)
	if err := os.WriteFile(filepath.Join(directory, "npm-shrinkwrap.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directory, linuxTarget); err == nil || !strings.Contains(err.Error(), "precedence") {
		t.Fatalf("shrinkwrap precedence ignored: %v", err)
	}
}

// This optional read-only gate uses the actual four-file audit source; it never
// reads installed dependencies or private application environment files.
func TestActualZeframLockFixture(t *testing.T) {
	directory := os.Getenv("ARCHER_NPM_FIXTURE")
	if directory == "" {
		t.Skip("set ARCHER_NPM_FIXTURE to the frozen Zefram source checkout")
	}
	plan, err := Load(directory, linuxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ManifestSHA256 != "1a7228fe4c395ecdefdd86f8ccb205d57f1edd292e97c7a33aaed8bc73207fb2" || plan.LockSHA256 != "7a57ced24fa15c20f4ee7e749d82d7271847301305395f844ee0cc07fb835013" {
		t.Fatal("actual fixture source bytes differ from audited commit")
	}
	if len(plan.Packages) != 112 || len(plan.Skipped) != 46 || len(plan.Direct) != 11 {
		t.Fatalf("actual fixture partition %d selected %d skipped %d direct", len(plan.Packages), len(plan.Skipped), len(plan.Direct))
	}
}
