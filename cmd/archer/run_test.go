package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunParserPreservesCommandArgumentsAfterSeparator(t *testing.T) {
	argv := []string{"node", "--eval", "process.exit(19)", "--project", "child-owned", "$HOME; & $(literal)"}
	opts, err := parse(append([]string{"run", "--offline", "--project", "project with spaces", "--"}, argv...))
	if err != nil || !opts.offline || opts.project != "project with spaces" || strings.Join(opts.argv, "\x00") != strings.Join(argv, "\x00") || len(opts.requirements) != 0 {
		t.Fatal(opts, err)
	}
	for _, args := range [][]string{{"run"}, {"run", "node"}, {"run", "--"}, {"run", "--json", "--", "node"}, {"plan", "--", "node"}} {
		var out, diagnostics bytes.Buffer
		if code := runWithInput(args, nil, &out, &diagnostics); code != 1 {
			t.Fatal("invalid run syntax accepted", args, code, out.String())
		}
	}
}

func TestRunChildFixture(t *testing.T) {
	if os.Getenv("ARCHER_CLI_RUN_FIXTURE") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	input, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[separator:], "input": string(input), "cwd": cwd})
	fmt.Fprintln(os.Stderr, "fixture diagnostics")
	os.Exit(19)
}

func TestRunCLIPassesStdioCwdAndChildExit(t *testing.T) {
	t.Setenv("ARCHER_CLI_RUN_FIXTURE", "1")
	project, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "archer.json"), []byte(`{"schema":"archer.project/v1","scope":"cli-run","requirements":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--project", project, "--home", home, "--", binary, "-test.run=^TestRunChildFixture$", "--", "with spaces", "$HOME; & $(literal)", "--project", "child-owned"}
	var out, diagnostics bytes.Buffer
	code := runWithInput(args, strings.NewReader("child input\n"), &out, &diagnostics)
	if code != 19 {
		t.Fatal(code, diagnostics.String(), out.String())
	}
	var result struct {
		Args       []string
		Input, Cwd string
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err, out.String())
	}
	if strings.Join(result.Args, "\x00") != strings.Join(args[len(args)-4:], "\x00") || result.Input != "child input\n" || diagnostics.String() != "fixture diagnostics\n" {
		t.Fatal(result, diagnostics.String())
	}
	actual, actualErr := os.Stat(result.Cwd)
	expected, expectedErr := os.Stat(project)
	if actualErr != nil || expectedErr != nil || !actual.IsDir() || !os.SameFile(actual, expected) {
		t.Fatalf("child working directory %q does not identify project %q: %v %v", result.Cwd, project, actualErr, expectedErr)
	}
}
