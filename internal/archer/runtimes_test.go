package archer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Parallax-Web-Services/archer/internal/composer"
	"github.com/Parallax-Web-Services/archer/internal/runtimes"
	"github.com/Parallax-Web-Services/archer/internal/store"
)

// The copied native test binary supplies a fixed Node version probe and a child
// argv/stdio fixture. End-user runtime selection never invokes a compiler.
func TestMain(m *testing.M) {
	if os.Getenv("ARCHER_ENGINE_RUNTIME_HELPER") == "1" {
		if len(os.Args) > 1 && os.Args[1] == "-n" {
			fmt.Print("8.5.8")
			os.Exit(0)
		}
		if len(os.Args) > 1 && os.Args[1] == "-d" {
			extensions := map[string]string{}
			if os.Getenv("PHPRC") != "" {
				extensions["json"] = "8.5.8"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"php_version": "8.5.8", "extensions": extensions})
			os.Exit(0)
		}
		if len(os.Args) == 2 && os.Args[1] == "--version" {
			if path := os.Getenv("ARCHER_ENGINE_MUTATE_SOURCE"); path != "" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					panic(err)
				}
				_, _ = f.WriteString("\n")
				_ = f.Close()
			}
			fmt.Println("v24.20.0")
			os.Exit(0)
		}
		input, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"argv": os.Args[1:], "stdin": string(input), "cwd": cwd, "path": os.Getenv("PATH"), "home": os.Getenv("ARCHER_HOME"), "node_options": os.Getenv("NODE_OPTIONS")})
		fmt.Fprintln(os.Stderr, "child diagnostics")
		code, _ := strconv.Atoi(os.Getenv("ARCHER_ENGINE_RUNTIME_EXIT"))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func runtimeEngineFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	t.Setenv("ARCHER_ENGINE_RUNTIME_HELPER", "1")
	t.Setenv("PATH", t.TempDir())
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "custom-interpreter")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(binary, data, 0755); err != nil {
		t.Fatal(err)
	}
	e := engine(t, Project{Schema: "archer.project/v1", Scope: "runtime-integration", Requirements: []Requirement{}, Runtimes: map[string]runtimes.Spec{"node": {Version: "24.20.0", Binary: binary}}})
	return e, binary
}

func TestRunUsesSelectedCustomInterpreterAndPreservesChildContract(t *testing.T) {
	e, binary := runtimeEngineFixture(t)
	t.Setenv("ARCHER_ENGINE_RUNTIME_EXIT", "23")
	t.Setenv("NODE_OPTIONS", "--invalid-preload-must-not-leak")
	args := []string{"node", "literal $HOME; & $(touch unwanted)", "with spaces", "--version", "Unicode café"}
	var output, diagnostics bytes.Buffer
	code, err := e.Run(context.Background(), args, strings.NewReader("literal input\n"), &output, &diagnostics)
	if err != nil || code != 23 {
		t.Fatal(code, err, diagnostics.String())
	}
	var result struct {
		Argv                   []string `json:"argv"`
		Stdin, Cwd, Path, Home string
		NodeOptions            string `json:"node_options"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err, output.String())
	}
	if strings.Join(result.Argv, "\x00") != strings.Join(args[1:], "\x00") || result.Stdin != "literal input\n" || result.Home != e.Home || result.NodeOptions != "" {
		t.Fatal(result)
	}
	assertRuntimeDirectory(t, result.Cwd, e.ProjectDir)
	paths := filepath.SplitList(result.Path)
	if len(paths) == 0 || diagnostics.String() != "child diagnostics\n" {
		t.Fatal(result, diagnostics.String())
	}
	assertRuntimeDirectory(t, paths[0], filepath.Dir(binary))
	entries, err := os.ReadDir(e.Home)
	if err != nil || len(entries) != 0 {
		t.Fatal("run installed or wrote Archer state", entries, err)
	}
}

// appendExecutable changes an executable that a probe may have just run. It
// replaces the file rather than writing in place: Linux can briefly refuse
// writes to a recently executed image, and Windows scanners can hold it open.
func appendExecutable(t *testing.T, path string, trailer []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".changed"
	if err := os.WriteFile(replacement, append(data, trailer...), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = os.Rename(replacement, path)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeDirectory(t *testing.T, actualPath, expectedPath string) {
	t.Helper()
	actual, actualErr := os.Stat(actualPath)
	expected, expectedErr := os.Stat(expectedPath)
	if actualErr != nil || expectedErr != nil || !actual.IsDir() || !os.SameFile(actual, expected) {
		t.Fatalf("directory %q does not identify %q: %v %v", actualPath, expectedPath, actualErr, expectedErr)
	}
}

func TestRuntimeSelectionsPreserveRequirementOrderAndUnrelatedScope(t *testing.T) {
	e, _ := runtimeEngineFixture(t)
	selected := []Requirement{{Capability: "unknown", Delivery: "local"}, {Capability: "runtime:node", Delivery: "local", Constraint: "24.20.0"}}
	plan, err := e.Plan(context.Background(), selected)
	if err != nil || len(plan.Decisions) != 2 || plan.Decisions[0].Requirement != selected[0] || plan.Decisions[1].Requirement != selected[1] || plan.Decisions[1].Status != "runtime_local" || len(plan.Runtimes) != 1 {
		t.Fatal(plan, err)
	}
	plan, err = e.Plan(context.Background(), selected[:1])
	if err != nil || len(plan.Runtimes) != 0 || len(plan.Decisions) != 1 {
		t.Fatal("unrelated selector prepared a runtime", plan, err)
	}
	for _, invalid := range []Requirement{{Capability: "runtime:node", Delivery: "service"}, {Capability: "runtime:node", Delivery: "local", Constraint: "22.13.0"}} {
		if _, err := e.Install(context.Background(), []Requirement{invalid}, ""); err == nil {
			t.Fatal("conflicting runtime policy accepted", invalid)
		}
	}
}

func TestRunMissingRuntimeDoesNotDownloadOrPrepare(t *testing.T) {
	e, _ := runtimeEngineFixture(t)
	e.Project.Runtimes["node"] = runtimes.Spec{Version: "24.20.0", Binary: "archer-missing-node", Artifacts: []runtimes.Bundle{{Platform: runtime.GOOS + "/" + runtime.GOARCH, Format: "raw", Executable: "bin/node", Artifact: store.Artifact{URL: "https://runtime.invalid/node", SHA256: strings.Repeat("a", 64)}}}}
	code, err := e.Run(context.Background(), []string{"node", "script.js"}, nil, io.Discard, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "run archer i") {
		t.Fatal(code, err)
	}
	entries, err := os.ReadDir(e.Home)
	if err != nil || len(entries) != 0 {
		t.Fatal("run prepared missing runtime", entries, err)
	}
}

func TestRuntimeVerificationRejectsProjectMutationBeforeEffects(t *testing.T) {
	e, _ := runtimeEngineFixture(t)
	source := filepath.Join(e.ProjectDir, "archer.json")
	if err := os.WriteFile(source, []byte(`{"schema":"archer.project/v1","scope":"fixture","requirements":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCHER_ENGINE_MUTATE_SOURCE", source)
	if _, err := e.Install(context.Background(), nil, ""); err == nil || !strings.Contains(err.Error(), "sources changed") {
		t.Fatal("mutation during probe accepted", err)
	}
	if _, err := e.Run(context.Background(), []string{"node", "execute"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "sources changed") {
		t.Fatal("run ignored mutation during probe", err)
	}
	entries, err := os.ReadDir(e.Home)
	if err != nil || len(entries) != 0 {
		t.Fatal("source drift wrote Archer state", entries, err)
	}
}

func TestApprovedRuntimeExecutableDriftBlocksInstallation(t *testing.T) {
	e, binary := runtimeEngineFixture(t)
	approved, err := e.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	appendExecutable(t, binary, []byte("changed executable trailer"))
	if _, err := e.Install(context.Background(), nil, approved.ID); err == nil || !strings.Contains(err.Error(), "plan changed since approval") {
		t.Fatal("local runtime drift did not invalidate approval", err)
	}
}

func TestMissingLocalPHPExtensionSelectsManagedBundleForPreparationAndRun(t *testing.T) {
	_, binary := runtimeEngineFixture(t)
	t.Setenv("PHPRC", "")
	e, calls := composerEngineFixture(t)
	e.Project.Composer.PHP = binary
	manifestPath := filepath.Join(e.ProjectDir, "composer.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["require"].(map[string]any)["ext-json"] = "*"
	manifestBytes, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(e.ProjectDir, "composer.lock")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]any
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatal(err)
	}
	lock["platform"].(map[string]any)["ext-json"] = "*"
	hash, err := composer.ContentHash(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lock["content-hash"] = hash
	lockBytes, _ = json.Marshal(lock)
	if err := os.WriteFile(lockPath, lockBytes, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	executable := "bin/php"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	e.Project.Runtimes = map[string]runtimes.Spec{"php": {Version: "8.5.8", Binary: binary, Artifacts: []runtimes.Bundle{{Platform: runtime.GOOS + "/" + runtime.GOARCH, Format: "raw", Executable: executable, Artifact: store.Artifact{URL: binary, SHA256: hex.EncodeToString(digest[:])}}}}}
	plan, err := e.Plan(context.Background(), nil)
	if err != nil || len(plan.Runtimes) != 1 || plan.Runtimes[0].Status != "install" || plan.Composer == nil || calls.Load() != 0 {
		t.Fatal(plan, err, calls.Load())
	}
	prepared, err := e.prepareRuntime(context.Background(), plan.Runtimes[0])
	if err != nil || prepared.Status != "managed" {
		t.Fatal(prepared, err)
	}
	plan, err = e.Plan(context.Background(), nil)
	if err != nil || plan.Runtimes[0].Status != "managed" || plan.ComposerRuntime == nil || plan.ComposerRuntime.Extensions["json"] == "" {
		t.Fatal(plan, err)
	}
	var output, diagnostics bytes.Buffer
	code, err := e.Run(context.Background(), []string{"php", "application.php"}, nil, &output, &diagnostics)
	if err != nil || code != 0 || !strings.Contains(output.String(), "application.php") {
		t.Fatal(code, err, output.String(), diagnostics.String())
	}
	if calls.Load() != 0 {
		t.Fatal("runtime selection replayed packages", calls.Load())
	}
}
