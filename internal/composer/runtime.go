package composer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Parallax-Web-Services/archer/internal/parallax"
)

const runtimeProbePHP = `$versions = []; foreach (get_loaded_extensions() as $extension) { $name = strtolower(str_replace(' ', '-', $extension)); $version = phpversion($extension); $versions[$name] = $version === false ? '' : $version; } echo json_encode(['php_version' => PHP_VERSION, 'extensions' => $versions], JSON_THROW_ON_ERROR);`

// ProbeRuntime executes only a fixed introspection program in an explicitly
// selected PHP interpreter. It does not load project or package code.
func ProbeRuntime(ctx context.Context, binary string) (RuntimeInfo, error) {
	return ProbeRuntimeWithEnv(ctx, binary, nil)
}

// ProbeRuntimeWithEnv uses the selected interpreter's environment for the same
// configuration that Archer passes to an application command. A nil environment
// preserves the invoking process's configuration for legacy local interpreters.
func ProbeRuntimeWithEnv(ctx context.Context, binary string, environment []string) (RuntimeInfo, error) {
	if binary == "" {
		binary = "php"
	}
	if strings.ContainsRune(binary, '\x00') {
		return RuntimeInfo{}, fmt.Errorf("invalid PHP executable")
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return RuntimeInfo{}, fmt.Errorf("PHP runtime unavailable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return RuntimeInfo{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return RuntimeInfo{}, fmt.Errorf("PHP runtime must be a regular executable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, resolved, "-d", "display_errors=stderr", "-d", "log_errors=0", "-r", runtimeProbePHP)
	if environment != nil {
		command.Env = environment
	}
	var output, diagnostics probeBuffer
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return RuntimeInfo{}, fmt.Errorf("PHP runtime probe failed: %w: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	if output.overflow {
		return RuntimeInfo{}, fmt.Errorf("PHP runtime probe exceeded 16 KiB")
	}
	return decodeRuntime(output.Bytes())
}

var runtimeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)
var extensionName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func decodeRuntime(data []byte) (RuntimeInfo, error) {
	object, err := parallax.Decode(bytes.NewReader(data))
	if err != nil {
		return RuntimeInfo{}, fmt.Errorf("invalid PHP runtime evidence: %w", err)
	}
	version, ok := object["php_version"].(string)
	if !ok || !runtimeVersion.MatchString(version) {
		return RuntimeInfo{}, fmt.Errorf("invalid PHP version evidence")
	}
	extensions, ok := object["extensions"].(map[string]any)
	if !ok {
		return RuntimeInfo{}, fmt.Errorf("PHP extension evidence must be an object")
	}
	result := RuntimeInfo{PHPVersion: version, Extensions: map[string]string{}}
	for name, value := range extensions {
		version, ok := value.(string)
		if !extensionName.MatchString(name) || !ok || len(version) > 128 || strings.ContainsAny(version, "\x00\r\n") {
			return RuntimeInfo{}, fmt.Errorf("invalid PHP extension evidence for %q", name)
		}
		result.Extensions[name] = version
	}
	return result, nil
}

type probeBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *probeBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := 16384 - b.Len()
	if len(p) > left {
		b.overflow = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
