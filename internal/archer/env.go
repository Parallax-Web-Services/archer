package archer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Parallax-Web-Services/archer/internal/composer"
)

const maxEnvBytes = 1 << 20

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// Environment follows Laravel's convention: real process variables take
// precedence over .env, and .env.example supplies committed defaults.
type Environment map[string]string

func LoadEnvironment(dir string) (Environment, error) {
	env := Environment{}
	for _, name := range []string{".env.example", ".env"} {
		values, err := readEnvFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for key, value := range values {
			env[key] = value
		}
	}
	return env, nil
}

func (env Environment) Get(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return env[key]
}

func (env Environment) List(key string) []string {
	out := []string{}
	for _, item := range strings.Split(env.Get(key), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func readEnvFile(path string) (map[string]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxEnvBytes {
		return nil, fmt.Errorf("must be a regular file of at most %d bytes", maxEnvBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxEnvBytes)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, raw, found := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !found || !envKey.MatchString(key) {
			return nil, fmt.Errorf("line %d is not KEY=value", line)
		}
		value, err := envValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		values[key] = value
	}
	return values, scanner.Err()
}

// envValue accepts quoted and unquoted dotenv values. Variable interpolation
// is left literal; Archer settings are explicit and never expand other keys.
func envValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single-quoted value")
		}
		return raw[1 : end+1], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			switch raw[i] {
			case '"':
				return b.String(), nil
			case '\\':
				if i+1 < len(raw) {
					i++
					switch raw[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(raw[i])
					}
					continue
				}
			}
			b.WriteByte(raw[i])
		}
		return "", errors.New("unterminated double-quoted value")
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw), nil
}

// ProjectFromEnvironment describes a project without archer.json: package
// requirements come from its Composer and npm manifests, Archer policy from env.
func ProjectFromEnvironment(dir string, env Environment) (Project, error) {
	requirements, err := ManifestRequirements(dir)
	if err != nil {
		return Project{}, err
	}
	scope := env.Get("ARCHER_SCOPE")
	if scope == "" {
		scope = filepath.Base(dir)
	}
	p := Project{Schema: "archer.project/v1", Scope: scope, Requirements: requirements}
	hosts := env.List("ARCHER_COMPOSER_TRUSTED_HOSTS")
	mode := env.Get("ARCHER_COMPOSER_SOURCE_POLICY")
	php := env.Get("ARCHER_PHP")
	if mode != "" || len(hosts) != 0 || php != "" {
		p.Composer = &ComposerConfig{PHP: php, SourcePolicy: composer.SourcePolicy{Mode: mode, TrustedHosts: hosts}}
	}
	return p, nil
}

// ManifestRequirements lists direct Composer and npm declarations as explicit
// package requirements, retaining each declared constraint.
func ManifestRequirements(dir string) ([]Requirement, error) {
	requirements := []Requirement{}
	seen := map[string]bool{}
	for _, source := range []struct {
		file, prefix string
		keys         []string
	}{
		{"composer.json", "composer:", []string{"require", "require-dev"}},
		{"package.json", "npm:", []string{"dependencies", "devDependencies", "optionalDependencies"}},
	} {
		b, err := os.ReadFile(filepath.Join(dir, source.file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(b, &manifest); err != nil {
			return nil, fmt.Errorf("%s: %w", source.file, err)
		}
		for _, key := range source.keys {
			var packages map[string]string
			if manifest[key] == nil {
				continue
			}
			if err := json.Unmarshal(manifest[key], &packages); err != nil {
				return nil, fmt.Errorf("%s %s: %w", source.file, key, err)
			}
			names := []string{}
			for name := range packages {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				capability := source.prefix + name
				if seen[capability] {
					continue
				}
				seen[capability] = true
				requirements = append(requirements, Requirement{Capability: capability, Delivery: "package", Constraint: packages[name]})
			}
		}
	}
	return requirements, nil
}
