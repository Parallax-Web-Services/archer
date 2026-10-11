package composer

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPHPDeclarationScanner(t *testing.T) {
	source := `outside class Misleading {}
<?php
// class Comment {}
/* namespace Wrong; interface AlsoWrong {} */
namespace First;
$s = "class StringClass {}";
$s = 'namespace NotReal;';
$s = <<<TEXT
namespace Never;
class HeredocClass {}
TEXT;
$s = <<<'NOWDOC'
trait NotTrait {}
NOWDOC;
class Actual {}
interface Contract {}
trait Reusable {}
enum Direction { case North; }
$x = new class extends Actual {};
$y = Actual::class;
namespace Grouped\Part { final class Nested {} }
namespace { CLASS GlobalClass {} }
?> class Html {}
<?php namespace Last; #[Some(Actual::class)] class LastClass {}
`
	want := []string{"First\\Actual", "First\\Contract", "First\\Reusable", "First\\Direction", "Grouped\\Part\\Nested", "GlobalClass", "Last\\LastClass"}
	if got := phpDeclarations([]byte(source)); !reflect.DeepEqual(got, want) {
		t.Fatalf("declarations = %#v, want %#v", got, want)
	}
}

func TestGeneratedAutoloadPHP(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("PHP interpreter unavailable")
	}
	project := t.TempDir()
	vendor := filepath.Join(project, "vendor")
	writeAutoloadFixture(t, project, "app/Widget.php", "<?php namespace App; class Widget { public static function value() { return 'root'; } }")
	writeAutoloadFixture(t, project, "tests/Spec.php", "<?php namespace Tests; class Spec {}")
	writeAutoloadFixture(t, project, "root.php", "<?php $GLOBALS['order'][] = 'root';")
	writeAutoloadFixture(t, vendor, "a/library/src/Thing.php", "<?php namespace Library; class Thing { public static function value() { return 'library'; } }")
	writeAutoloadFixture(t, vendor, "a/library/legacy/Old/Style.php", "<?php class Old_Style {}")
	writeAutoloadFixture(t, vendor, "a/library/classmap/Multi.php", "<?php namespace Mapped; class First {} interface Second {} trait Third {}")
	writeAutoloadFixture(t, vendor, "a/library/classmap/Tests/Excluded.php", "<?php class Excluded {}")
	writeAutoloadFixture(t, vendor, "a/library/files.php", "<?php $GLOBALS['order'][] = 'dependency';")
	writeAutoloadFixture(t, vendor, "z/client/files.php", "<?php if (end($GLOBALS['order']) !== 'dependency') { throw new Exception('dependency order'); } $GLOBALS['order'][] = 'client';")
	writeAutoloadFixture(t, vendor, "z/client/run.php", "#!/usr/bin/env php\n<?php $loader = require $_composer_autoload_path; echo basename($_composer_bin_dir) . ':' . Library\\Thing::value();")
	plan := &Plan{Name: "root/app", Version: "1.0.0", Autoload: Autoload{PSR4: map[string][]string{"App\\": {"app"}}, Files: []string{"root.php"}}, AutoloadDev: Autoload{PSR4: map[string][]string{"Tests\\": {"tests"}}}, Packages: []Package{
		{Name: "z/client", Version: "2.3.4", Type: "library", Dev: true, Reference: strings.Repeat("a", 40), Require: map[string]string{"a/library": "^1.0"}, Autoload: Autoload{Files: []string{"files.php"}}, Bin: []string{"run.php"}, Extra: map[string]any{"laravel": map[string]any{"providers": []any{"Client\\Provider"}}}},
		{Name: "a/library", Version: "v1.2.3", Type: "library", Autoload: Autoload{PSR4: map[string][]string{"Library\\": {"src"}}, PSR0: map[string][]string{"Old_": {"legacy"}}, Classmap: []string{"classmap"}, ExcludeFromClassmap: []string{"/classmap/Tests/"}, Files: []string{"files.php"}}, Provide: map[string]string{"virtual/library": "self.version"}},
	}}
	if err := GenerateAutoload(project, vendor, plan); err != nil {
		t.Fatal(err)
	}
	probe := `<?php
$loader = require __DIR__ . '/vendor/autoload.php';
if ($loader !== (require __DIR__ . '/vendor/autoload.php')) { throw new Exception('not idempotent'); }
foreach (array('App\\Widget','Tests\\Spec','Library\\Thing','Old_Style','Mapped\\First','Mapped\\Second','Mapped\\Third') as $class) {
    if (!(class_exists($class) || interface_exists($class) || trait_exists($class))) { throw new Exception('missing ' . $class); }
}
if (class_exists('Excluded')) { throw new Exception('excluded class loaded'); }
if (App\Widget::value() !== 'root') { throw new Exception('root PSR-4'); }
if (Composer\InstalledVersions::getVersion('a/library') !== '1.2.3.0') { throw new Exception('normalized version'); }
if (Composer\InstalledVersions::isInstalled('z/client', false)) { throw new Exception('dev flag'); }
if (!Composer\InstalledVersions::isInstalled('virtual/library')) { throw new Exception('virtual package'); }
if (Composer\InstalledVersions::getVersionRanges('virtual/library') !== 'v1.2.3') { throw new Exception('provided version'); }
if (realpath(Composer\InstalledVersions::getInstallPath('a/library')) !== realpath(__DIR__ . '/vendor/a/library')) { throw new Exception('install path'); }
if (Composer\InstalledVersions::getRootPackage()['name'] !== 'root/app') { throw new Exception('root metadata'); }
if (count(Composer\Autoload\ClassLoader::getRegisteredLoaders()) !== 1) { throw new Exception('registration'); }
echo json_encode($GLOBALS['order']);
`
	writeAutoloadFixture(t, project, "probe.php", probe)
	output, err := exec.Command(php, filepath.Join(project, "probe.php")).CombinedOutput()
	if err != nil {
		t.Fatalf("PHP loader: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != `["dependency","client","root"]` {
		t.Fatalf("autoload output %s", output)
	}
	output, err = exec.Command(php, filepath.Join(vendor, "bin", "run.php")).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "bin:library" {
		t.Fatalf("binary proxy: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(vendor, "composer", "installed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["dev"].(bool) != true || len(metadata["dev-package-names"].([]any)) != 1 {
		t.Fatalf("invalid installed.json %s", data)
	}
	// Regeneration uses the eventual vendor directory, never the private stage.
	generated, err := AutoloadFiles(project, vendor, plan)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range generated {
		actual, err := os.ReadFile(filepath.Join(vendor, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(actual, expected.Data) || bytes.Contains(expected.Data, []byte(project)) {
			t.Fatalf("nondeterministic/absolute generated file %s: %v", name, err)
		}
	}
}

func TestAutoloadRejectsUnsafeInputs(t *testing.T) {
	for _, bad := range []string{"../escape", "/absolute", "C:/absolute", "a\x00b", "a/../../b"} {
		t.Run(strings.ReplaceAll(bad, "/", "_"), func(t *testing.T) {
			plan := &Plan{Name: "root/app", Autoload: Autoload{PSR4: map[string][]string{"App\\": {bad}}}}
			if _, err := AutoloadFiles(t.TempDir(), t.TempDir(), plan); err == nil {
				t.Fatalf("accepted unsafe path %q", bad)
			}
		})
	}
	project, vendor := t.TempDir(), t.TempDir()
	plan := &Plan{Name: "root/app", Packages: []Package{{Name: "test/one", Bin: []string{"run"}}}}
	writeAutoloadFixture(t, vendor, "test/one/run", "#!/bin/sh\necho unsafe")
	if _, err := AutoloadFiles(project, vendor, plan); err == nil {
		t.Fatal("accepted non-PHP executable")
	}
}

func TestAutoloadDependencyCyclesAreDeterministic(t *testing.T) {
	p := []Package{{Name: "z/b", Require: map[string]string{"a/a": "*"}}, {Name: "a/a", Require: map[string]string{"z/b": "*"}}}
	a := autoloadPackageOrder(p)
	p[0], p[1] = p[1], p[0]
	b := autoloadPackageOrder(p)
	if !reflect.DeepEqual(a, b) || len(a) != 2 {
		t.Fatalf("cycle ordering differs: %#v / %#v", a, b)
	}
}

func TestAutoloadRecursiveClassmapAndRootBoundary(t *testing.T) {
	project, vendor := t.TempDir(), t.TempDir()
	writeAutoloadFixture(t, vendor, "test/library/src/First.php", "<?php class FirstLevel {}")
	writeAutoloadFixture(t, vendor, "test/library/src/deep/Second.php", "<?php class SecondLevel {}")
	writeAutoloadFixture(t, vendor, "test/library/src/Tests/Ignore.php", "<?php class IgnoreFirstLevel {}")
	writeAutoloadFixture(t, vendor, "test/library/src/deep/Tests/Ignore.php", "<?php class IgnoreSecondLevel {}")
	plan := &Plan{Name: "root/app", Packages: []Package{{Name: "test/library", Autoload: Autoload{Classmap: []string{"src/**/*.php"}, ExcludeFromClassmap: []string{"/src/**/Tests/"}}}}}
	generated, err := AutoloadFiles(project, vendor, plan)
	if err != nil {
		t.Fatal(err)
	}
	data := generated["composer/autoload_classmap.php"].Data
	if !bytes.Contains(data, []byte("'FirstLevel'")) || !bytes.Contains(data, []byte("'SecondLevel'")) || bytes.Contains(data, []byte("'Ignore")) {
		t.Fatalf("incorrect recursive classmap %s", data)
	}
	for _, broad := range []string{".", "vendor", "**/*.php", ".archer-composer-stage-anywhere"} {
		plan.Autoload.Classmap = []string{broad}
		if _, err := AutoloadFiles(project, vendor, plan); err == nil {
			t.Fatalf("accepted unstable root classmap %q", broad)
		}
	}
}

func TestAutoloadRejectsIntermediateSymlink(t *testing.T) {
	project, vendor, external := t.TempDir(), t.TempDir(), t.TempDir()
	writeAutoloadFixture(t, external, "nested/Unsafe.php", "<?php class ExternalClass {}")
	if err := os.Symlink(external, filepath.Join(project, "linked")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	for _, autoload := range []Autoload{
		{PSR4: map[string][]string{"External\\": {"linked/nested"}}},
		{Classmap: []string{"linked/nested"}},
		{Classmap: []string{"linked/nested/*.php"}},
		{Files: []string{"linked/nested/Unsafe.php"}},
	} {
		if _, err := AutoloadFiles(project, vendor, &Plan{Name: "root/app", Autoload: autoload}); err == nil {
			t.Fatalf("accepted intermediate symlink %#v", autoload)
		}
	}
}

func TestAutoloadOrdersVirtualDependencyProviderBeforeClient(t *testing.T) {
	packages := []Package{
		{Name: "a/client", Require: map[string]string{"virtual/api": "^1.0"}},
		{Name: "z/provider", Provide: map[string]string{"virtual/api": "1.0"}},
	}
	ordered := autoloadPackageOrder(packages)
	if len(ordered) != 2 || ordered[0].Name != "z/provider" || ordered[1].Name != "a/client" {
		t.Fatalf("virtual dependency order %#v", ordered)
	}
}

func writeAutoloadFixture(t *testing.T, base, name, contents string) {
	t.Helper()
	filename := filepath.Join(base, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}
