package archer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentFollowsLaravelPrecedence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env.example", "# defaults\nARCHER_SCOPE=example\nARCHER_COMPOSER_SOURCE_POLICY=source-reference\nARCHER_COMPOSER_TRUSTED_HOSTS=\"api.github.com, gitlab.com\"\nVITE_APP_NAME=\"${APP_NAME}\"\n")
	write(".env", "export ARCHER_SCOPE='local app' # kept\nARCHER_PHP=/opt/php # comment\nEMPTY=\n")
	t.Setenv("ARCHER_PHP", "/usr/bin/php")
	env, err := LoadEnvironment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if env.Get("ARCHER_SCOPE") != "local app" || env.Get("ARCHER_PHP") != "/usr/bin/php" || env["ARCHER_PHP"] != "/opt/php" || env.Get("VITE_APP_NAME") != "${APP_NAME}" || env.Get("EMPTY") != "" {
		t.Fatalf("unexpected environment %+v", env)
	}
	if hosts := env.List("ARCHER_COMPOSER_TRUSTED_HOSTS"); len(hosts) != 2 || hosts[1] != "gitlab.com" {
		t.Fatalf("unexpected host list %q", hosts)
	}
	write(".env", "NOT A SETTING\n")
	if _, err := LoadEnvironment(dir); err == nil {
		t.Fatal("accepted malformed .env")
	}
}

func TestLoadDerivesProjectFromManifestsAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"composer.json": `{"require":{"php":"^8.4","acme/cli":"dev-master"},"require-dev":{"phpunit/phpunit":"^12"}}`,
		"package.json":  `{"dependencies":{"react":"^19"},"optionalDependencies":{"@laravel/multiplex":"^0.4"}}`,
		".env.example":  "ARCHER_COMPOSER_SOURCE_POLICY=source-reference\nARCHER_COMPOSER_TRUSTED_HOSTS=api.github.com,codeload.github.com,gitlab.com\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Load(dir, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	p := e.Project
	if p.Scope != filepath.Base(e.ProjectDir) || len(p.Requirements) != 5 || p.Requirements[1] != (Requirement{Capability: "composer:php", Delivery: "package", Constraint: "^8.4"}) {
		t.Fatalf("unexpected derived project %+v", p)
	}
	if p.Composer == nil || p.Composer.SourcePolicy.Mode != "source-reference" || len(p.Composer.SourcePolicy.TrustedHosts) != 3 {
		t.Fatalf("composer policy not read from environment: %+v", p.Composer)
	}
	if err := os.WriteFile(filepath.Join(dir, "archer.json"), []byte(`{"schema":"archer.project/v1","scope":"explicit","requirements":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if e, err = Load(dir, t.TempDir(), true); err != nil || e.Project.Scope != "explicit" || e.Project.Composer != nil {
		t.Fatalf("archer.json must take precedence: %+v %v", e, err)
	}
}
