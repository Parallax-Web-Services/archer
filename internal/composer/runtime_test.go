package composer

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestRuntimeEvidenceRejectsMalformedAssertions(t *testing.T) {
	for _, input := range []string{
		`{"php_version":"8.5.8","extensions":[]} `,
		`{"php_version":"8.5.8","extensions":{"PDO":"8.5.8"}}`,
		`{"php_version":"8.5.8","extensions":{"pdo":true}}`,
		`{"php_version":"eight","extensions":{}}`,
		`{"php_version":"8.1.0","php_version":"8.5.8","extensions":{}}`,
		`{"php_version":"8.5.8","extensions":{}} {}`,
	} {
		if _, err := decodeRuntime([]byte(input)); err == nil {
			t.Fatalf("accepted malformed evidence %s", input)
		}
	}
	info, err := decodeRuntime([]byte(`{"php_version":"8.5.8","extensions":{"pdo":"8.5.8","zend-opcache":"8.5.8","custom":""}}`))
	if err != nil || info.PHPVersion != "8.5.8" || len(info.Extensions) != 3 {
		t.Fatal(info, err)
	}
}

func TestProbeRuntimeUsesRealInterpreterAndHonorsCancellation(t *testing.T) {
	binary := os.Getenv("ARCHER_TEST_PHP")
	if binary == "" {
		binary, _ = exec.LookPath("php")
	}
	if binary == "" {
		t.Skip("PHP interpreter not available")
	}
	info, err := ProbeRuntime(context.Background(), binary)
	if err != nil || info.PHPVersion == "" {
		t.Fatal(info, err)
	}
	if _, ok := info.Extensions["json"]; !ok {
		t.Fatal("JSON extension missing from evidence")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProbeRuntime(ctx, binary); err == nil {
		t.Fatal("cancelled probe executed")
	}
}

func TestProbeRuntimeRejectsUnavailableInterpreter(t *testing.T) {
	if _, err := ProbeRuntime(context.Background(), "archer-php-runtime-that-does-not-exist"); err == nil {
		t.Fatal("missing runtime accepted")
	}
}
