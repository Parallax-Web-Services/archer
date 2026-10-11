package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Parallax-Web-Services/archer/internal/archer"
)

func TestInitPreservesPackageMeaningAndConstraints(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "composer.json"), []byte(`{"require":{"php":"^8.4","laravel/framework":"^13"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"dependencies":{"react":"^19"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := run([]string{"init", "--project", project}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	b, err := os.ReadFile(filepath.Join(project, "archer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p archer.Project
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Requirements) != 3 {
		t.Fatal(p)
	}
	for _, r := range p.Requirements {
		if r.Delivery != "package" || r.Constraint == "" {
			t.Fatal("package semantics or constraint lost", r)
		}
	}
	if code := run([]string{"init", "--project", project}, &out, &errout); code != 1 {
		t.Fatal("init overwrote existing configuration")
	}
}

func TestUnresolvedJSONUsesExitTwoAndNeverCallsAI(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "archer.json"), []byte(`{"schema":"archer.project/v1","scope":"cli-test","requirements":["unknown"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := run([]string{"i", "--plan", "--json", "--project", project, "--home", home}, &out, &errout); code != 2 {
		t.Fatal(code, errout.String())
	}
	var plan archer.Plan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Remaining) != 1 || plan.AIRequests != 0 {
		t.Fatal(plan)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("offline planning unexpectedly wrote state", entries, err)
	}
}

func TestMalformedConfigFailsInsteadOfAppearingReady(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "archer.json"), []byte(`{"schema":"archer.project/v1","scope":"test","requirements":[]} {"hidden":"second object"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := run([]string{"plan", "--project", project, "--home", t.TempDir()}, &out, &errout); code != 1 {
		t.Fatal("malformed configuration appeared ready")
	}
}
