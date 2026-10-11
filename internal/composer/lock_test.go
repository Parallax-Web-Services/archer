package composer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lockTestReference = "0123456789abcdef0123456789abcdef01234567"

func lockTestFixture(t *testing.T) (string, map[string]any, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{"name": "acme/application", "require": map[string]any{"php": "^8.3", "acme/library": "^1.2"}, "require-dev": map[string]any{"acme/test-library": "^2"}, "autoload": map[string]any{"psr-4": map[string]any{"App\\": "app/"}}, "autoload-dev": map[string]any{"psr-4": map[string]any{"Tests\\": "tests/"}}, "scripts": map[string]any{"post-autoload-dump": []any{"@php artisan package:discover"}}}
	pkg := func(name, version string) map[string]any {
		return map[string]any{"name": name, "version": version, "type": "library", "source": map[string]any{"type": "git", "reference": lockTestReference, "url": "https://github.com/acme/library.git"}, "dist": map[string]any{"type": "zip", "url": "https://api.github.com/repos/acme/library/zipball/" + lockTestReference, "reference": lockTestReference, "shasum": ""}, "autoload": map[string]any{"psr-4": map[string]any{"Acme\\": "src/"}}}
	}
	production := pkg("acme/library", "v1.2.3")
	production["require"] = map[string]any{"php": ">=8.4.1", "ext-json": "*", "virtual/contract": "^2", "composer-runtime-api": "^2.2"}
	production["replace"] = map[string]any{"legacy/library": "self.version"}
	development := pkg("acme/test-library", "2.1.0")
	development["provide"] = map[string]any{"virtual/contract": "1.0 || 2.0"}
	development["bin"] = []any{"bin/test"}
	development["require"] = map[string]any{"legacy/library": "^1.2"}
	lock := map[string]any{"packages": []any{production}, "packages-dev": []any{development}, "platform": map[string]any{"php": "^8.3"}, "platform-dev": map[string]any{}, "aliases": []any{}, "stability-flags": map[string]any{}}
	lockWriteFixture(t, dir, manifest, lock)
	return dir, manifest, lock
}

func lockWriteFixture(t *testing.T, dir string, manifest, lock map[string]any) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ContentHash(data)
	if err != nil {
		t.Fatal(err)
	}
	lock["content-hash"] = hash
	locked, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"composer.json": data, "composer.lock": locked} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func lockTestPolicy() SourcePolicy {
	return SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"codeload.github.com", "api.github.com"}}
}

func TestLoadFrozenGraphAndRuntime(t *testing.T) {
	dir, _, _ := lockTestFixture(t)
	plan, err := Load(dir, lockTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packages) != 2 || plan.Packages[0].Path != "vendor/acme/library" || !plan.Packages[1].Dev || plan.Direct["php"] != "^8.3" || plan.RequireDev["acme/test-library"] != "^2" || len(plan.PlatformRequirements) != 3 || len(plan.Scripts) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := CheckRuntime(plan, RuntimeInfo{PHPVersion: "8.5.8", Extensions: map[string]string{"json": "8.5.8"}}); err != nil {
		t.Fatal(err)
	}
	for _, info := range []RuntimeInfo{{PHPVersion: "8.4.0", Extensions: map[string]string{"json": "8.4.0"}}, {PHPVersion: "9.0.0", Extensions: map[string]string{"json": "9.0.0"}}, {PHPVersion: "8.5.8", Extensions: map[string]string{}}} {
		if err := CheckRuntime(plan, info); err == nil {
			t.Fatalf("accepted incompatible runtime %+v", info)
		}
	}
	plan.Packages[0].URL = strings.Replace(plan.Packages[0].URL, "acme/library", "attacker/archive", 1)
	if err := ValidatePlan(plan); err == nil {
		t.Fatal("accepted changed plan")
	}
}

func TestSourcePolicyIsExplicit(t *testing.T) {
	dir, manifest, lock := lockTestFixture(t)
	if _, err := Load(dir, SourcePolicy{}); !errors.Is(err, ErrSourcePolicy) {
		t.Fatalf("missing checksum gate: %v", err)
	}
	if _, err := Load(dir, SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"other.example"}}); !errors.Is(err, ErrSourcePolicy) {
		t.Fatalf("host gate: %v", err)
	}
	for _, field := range []string{"packages", "packages-dev"} {
		for _, v := range lock[field].([]any) {
			v.(map[string]any)["dist"].(map[string]any)["shasum"] = strings.Repeat("a", 40)
		}
	}
	lockWriteFixture(t, dir, manifest, lock)
	if plan, err := Load(dir, SourcePolicy{}); err != nil || plan.Policy.Mode != "checksum" {
		t.Fatalf("checksummed profile: %v", err)
	}
}

func TestContentHashComposerGolden(t *testing.T) {
	// Independently computed with PHP json_decode(..., true), top-level ksort,
	// and json_encode(..., 0), following Composer Package/Locker::getContentHash.
	manifest := []byte(`{"require":{"z/z":"^2","a/a":"^1"},"name":"acme/app","extra":{"unicode":"\u00e9\ud83d\ude00","empty":{}}}`)
	hash, err := ContentHash(manifest)
	if err != nil || hash != "582111fa9b57fa7132b2ae8552231a54" {
		t.Fatalf("content hash %s: %v", hash, err)
	}
	changed := []byte(`{"name":"acme/app","require":{"a/a":"^1","z/z":"^2"},"extra":{"unicode":"\u00e9\ud83d\ude00","empty":{}},"description":"ignored"}`)
	other, err := ContentHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if other == hash {
		t.Fatal("nested insertion order must affect Composer compatibility hash")
	}
	for _, invalid := range []string{`{"name":"acme/app","name":"different/app"}`, `{} {}`, `{"extra":{"number":1.25}}`} {
		if _, err := ContentHash([]byte(invalid)); err == nil {
			t.Fatalf("accepted ambiguous/unsupported hash input %s", invalid)
		}
	}
}

func TestGitHubArchiveURLProfile(t *testing.T) {
	policy, _ := normalizePolicy(lockTestPolicy())
	for _, raw := range []string{"https://api.github.com/repos/acme/library/zipball/" + lockTestReference, "https://codeload.github.com/acme/library/zip/" + lockTestReference, "https://codeload.github.com/acme/library/legacy.zip/" + lockTestReference} {
		if err := validateArchiveURL(raw, lockTestReference, policy); err != nil {
			t.Errorf("valid archive %s: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://other.example/repos/acme/library/zipball/" + lockTestReference, "https://api.github.com:444/repos/acme/library/zipball/" + lockTestReference, "http://api.github.com/repos/acme/library/zipball/" + lockTestReference, "https://user:secret@api.github.com/repos/acme/library/zipball/" + lockTestReference, "https://codeload.github.com/acme/library/zipball/" + lockTestReference, "https://api.github.com/repos/acme/library/zipball/" + lockTestReference + "?token=secret", "https://api.github.com/repos/acme/library/zipball/" + lockTestReference + "/../main"} {
		if err := validateArchiveURL(raw, lockTestReference, policy); err == nil {
			t.Errorf("accepted unsafe archive URL %s", raw)
		}
	}
	fixturePolicy, _ := normalizePolicy(SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"127.0.0.1"}})
	if err := validateArchiveURL("http://127.0.0.1:1234/repos/acme/library/zipball/"+lockTestReference, lockTestReference, fixturePolicy); err != nil {
		t.Fatal(err)
	}
	original := SourcePolicy{Mode: "source-reference", TrustedHosts: []string{"codeload.github.com", "api.github.com"}}
	if _, err := normalizePolicy(original); err != nil {
		t.Fatal(err)
	}
	if original.TrustedHosts[0] != "codeload.github.com" {
		t.Fatal("normalization mutated caller's policy slice")
	}
}

func TestRejectUnsupportedAndIncompatibleLock(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"direct-version", func(m, l map[string]any) { l["packages"].([]any)[0].(map[string]any)["version"] = "2.0.0" }},
		{"missing-provider", func(m, l map[string]any) { delete(l["packages-dev"].([]any)[0].(map[string]any), "provide") }},
		{"incompatible-provider", func(m, l map[string]any) {
			l["packages-dev"].([]any)[0].(map[string]any)["provide"] = map[string]any{"virtual/contract": "^3"}
		}},
		{"wrong-replacer", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["replace"] = map[string]any{"legacy/library": "^2"}
		}},
		{"plugin", func(m, l map[string]any) { l["packages-dev"].([]any)[0].(map[string]any)["type"] = "composer-plugin" }},
		{"metapackage", func(m, l map[string]any) { l["packages-dev"].([]any)[0].(map[string]any)["type"] = "metapackage" }},
		{"mutable-reference", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["dist"].(map[string]any)["reference"] = "main"
		}},
		{"reference-url-drift", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["dist"].(map[string]any)["url"] = "https://api.github.com/repos/acme/library/zipball/main"
		}},
		{"source-reference-drift", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["source"].(map[string]any)["reference"] = strings.Repeat("f", 40)
		}},
		{"source-repository-drift", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["source"].(map[string]any)["url"] = "https://github.com/other/library.git"
		}},
		{"null-type", func(m, l map[string]any) { l["packages"].([]any)[0].(map[string]any)["type"] = nil }},
		{"null-autoload", func(m, l map[string]any) { m["autoload"] = nil }},
		{"null-shasum", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["dist"].(map[string]any)["shasum"] = nil
		}},
		{"path-autoload", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["autoload"] = map[string]any{"files": []any{"../../outside.php"}}
		}},
		{"path-bin", func(m, l map[string]any) {
			l["packages-dev"].([]any)[0].(map[string]any)["bin"] = []any{"/tmp/host-script"}
		}},
		{"source-type", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["source"].(map[string]any)["type"] = "path"
		}},
		{"duplicate-package", func(m, l map[string]any) {
			l["packages-dev"] = append(l["packages-dev"].([]any), l["packages"].([]any)[0])
		}},
		{"platform-override", func(m, l map[string]any) { m["config"] = map[string]any{"platform": map[string]any{"php": "8.5.8"}} }},
		{"lock-platform-drift", func(m, l map[string]any) { l["platform"] = map[string]any{"php": "^9"} }},
		{"aliases", func(m, l map[string]any) { l["aliases"] = []any{map[string]any{"alias": "1.0"}} }},
		{"prerelease", func(m, l map[string]any) { l["packages"].([]any)[0].(map[string]any)["version"] = "1.2.3-beta1" }},
		{"unsupported-range", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["require"].(map[string]any)["php"] = "!=8.0"
		}},
		{"runtime-api", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["require"].(map[string]any)["composer-runtime-api"] = "^3"
		}},
		{"locked-conflict", func(m, l map[string]any) {
			l["packages"].([]any)[0].(map[string]any)["conflict"] = map[string]any{"acme/test-library": "^2"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, m, l := lockTestFixture(t)
			test.mutate(m, l)
			lockWriteFixture(t, dir, m, l)
			if _, err := Load(dir, lockTestPolicy()); err == nil {
				t.Fatal("accepted unsupported or incompatible lock")
			}
		})
	}
}

func TestRejectSourceDriftAndMalformedJSON(t *testing.T) {
	dir, manifest, _ := lockTestFixture(t)
	manifest["require"].(map[string]any)["acme/library"] = "^2"
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "composer.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, lockTestPolicy()); err == nil || !strings.Contains(err.Error(), "content-hash") {
		t.Fatalf("source drift: %v", err)
	}
	for _, input := range []string{`{"packages":[],"packages":[]}`, `{} {}`, `[]`, `{"a":{"x":1,"x":2}}`, strings.Repeat("[", 130) + strings.Repeat("]", 130)} {
		if _, err := decodeObject([]byte(input)); err == nil {
			t.Fatalf("accepted malformed JSON %q", input)
		}
	}
	if _, err := decodeObject([]byte{0xff}); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	f, err := os.Create(filepath.Join(dir, "composer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxLockBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Load(dir, lockTestPolicy()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize source: %v", err)
	}
}

func TestStableComposerConstraints(t *testing.T) {
	for _, test := range []struct {
		version, constraint string
		want                bool
	}{
		{"8.5.8", "^8.3", true}, {"9.0.0", "^8.3", false}, {"8.4.0", ">=8.4.1", false}, {"8.4.1", ">=8.4.1", true},
		{"8.5.99", "8.1 - 8.5", true}, {"8.6.0", "8.1 - 8.5", false}, {"1.2.3", "1.0.0 - 1.2.3", true}, {"1.2.4", "1.0.0 - 1.2.3", false},
		{"0.0.4", "^0.0.3", false}, {"0.4.0", "^0.3.10", false}, {"0.3.11", "^0.3.10", true}, {"0.0.9", "^0.0", true}, {"0.1.0", "^0.0", false},
		{"1.9.0", "~1.2", true}, {"2.0.0", "~1.2", false}, {"1.3.0", "~1.2.3", false}, {"1.2.9", "~1.2.3", true},
		{"1.2.3.4", "~1.2.3.4", true}, {"1.2.3.99", "~1.2.3.4", true}, {"1.2.4.0", "~1.2.3.4", false}, {"1.2.9.0", "~1.2.3.4", false},
		{"0.0.0.1", "^0.0.0.1", true}, {"0.0.0.2", "^0.0.0.1", true}, {"0.0.1.0", "^0.0.0.1", false},
		{"7.4.5", "^7.4|^8.0", true}, {"8.2.1", "^7.4|^8.0", true}, {"9.0", "^7.4|^8.0", false},
		{"8.4.1", ">= 8.4.1, < 9", true}, {"9.0.0", ">=8.4.1 <9", false}, {"1.9.0", "1.*", true}, {"2.0.0", "1.*", false},
		{"v13.35.0", "^11.45.3|^12.41.1|^13.0", true}, {"0.0.0", "*", true},
	} {
		ok, err := Satisfies(test.version, test.constraint)
		if err != nil || ok != test.want {
			t.Errorf("%s %s = %v, %v; want %v", test.version, test.constraint, ok, err, test.want)
		}
	}
	for _, c := range []string{"dev-main", "^8.3@dev", "!=8.1", "^8.3 ||", ">=1 <0 !=3", "1.*.3", "99999999999.0", "1.0-beta"} {
		if _, err := Satisfies("8.5.8", c); err == nil {
			t.Errorf("accepted unsupported constraint %q", c)
		}
	}
}

func TestPlatformConflict(t *testing.T) {
	dir, m, l := lockTestFixture(t)
	l["packages"].([]any)[0].(map[string]any)["conflict"] = map[string]any{"php": "<8.4.2"}
	lockWriteFixture(t, dir, m, l)
	plan, err := Load(dir, lockTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRuntime(plan, RuntimeInfo{PHPVersion: "8.4.1", Extensions: map[string]string{"json": "8.4.1"}}); err == nil {
		t.Fatal("accepted conflicting platform")
	}
}

func TestFrozenZeframComposerContract(t *testing.T) {
	dir := os.Getenv("ARCHER_COMPOSER_FIXTURE")
	if dir == "" {
		t.Skip("set ARCHER_COMPOSER_FIXTURE to the frozen Zefram checkout for application acceptance")
	}
	plan, err := Load(dir, lockTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packages) != 115 || plan.ContentHash != "f2b07acadf1990a313ab2faa5b8302c1" || plan.ManifestSHA256 != "6f6a543a3e092929add9646035e32ac987599fdcf5276ca9e23d759ccdc07158" || plan.LockSHA256 != "f9d6fae6195f8b4abb33534c4be503af39d2b14adcff65e956fc31bb6652879a" {
		t.Fatalf("frozen application identity/count changed: %+v", plan)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatal(err)
	}
	extensions := map[string]string{}
	for _, req := range plan.PlatformRequirements {
		if strings.HasPrefix(req.Name, "ext-") {
			extensions[strings.TrimPrefix(req.Name, "ext-")] = "8.5.8"
		}
	}
	if err := CheckRuntime(plan, RuntimeInfo{PHPVersion: "8.5.8", Extensions: extensions}); err != nil {
		t.Fatal(err)
	}
	if err := CheckRuntime(plan, RuntimeInfo{PHPVersion: "8.4.0", Extensions: extensions}); err == nil {
		t.Fatal("PHP 8.4.0 should fail frozen Symfony requirements")
	}
}
