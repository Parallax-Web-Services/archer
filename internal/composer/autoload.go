package composer

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// GeneratedFile is a reproducible file relative to the vendor directory.
type GeneratedFile struct {
	Data       []byte
	Executable bool
}

// GenerateAutoload writes Archer's own Composer-compatible loader and metadata.
// Package scripts are never evaluated while constructing these files.
func GenerateAutoload(projectDir, vendorStage string, plan *Plan) error {
	files, err := AutoloadFiles(projectDir, vendorStage, plan)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		file := files[name]
		target := filepath.Join(vendorStage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		mode := fs.FileMode(0644)
		if file.Executable {
			mode = 0755
		}
		if err := os.WriteFile(target, file.Data, mode); err != nil {
			return err
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
	}
	return nil
}

// AutoloadFiles reads class declarations without executing PHP. All emitted
// paths are relative, allowing the private installation stage to be renamed.
func AutoloadFiles(projectDir, vendorSource string, plan *Plan) (map[string]GeneratedFile, error) {
	if plan == nil {
		return nil, fmt.Errorf("autoload requires a Composer plan")
	}
	psr4, psr0 := map[string][]string{}, map[string][]string{}
	classmap := map[string]string{"Composer\\InstalledVersions": "composer/InstalledVersions.php"}
	fileMap := map[string]string{}
	fileOrder := []string{}
	files := map[string]GeneratedFile{}
	add := func(base, location string, a Autoload, name string) error {
		for _, pair := range []struct {
			src map[string][]string
			dst map[string][]string
		}{{a.PSR4, psr4}, {a.PSR0, psr0}} {
			for _, prefix := range sortedKeys(pair.src) {
				for _, dir := range pair.src[prefix] {
					clean, err := autoloadPath(dir, true)
					if err != nil {
						return fmt.Errorf("%s autoload: %w", name, err)
					}
					if err := autoloadSafeAncestors(base, clean, true); err != nil {
						return fmt.Errorf("%s autoload: %w", name, err)
					}
					pair.dst[prefix] = append(pair.dst[prefix], path.Join(location, clean))
				}
			}
		}
		excludes := make([]*regexp.Regexp, 0, len(a.ExcludeFromClassmap))
		for _, pattern := range a.ExcludeFromClassmap {
			r, err := excludePattern(pattern)
			if err != nil {
				return fmt.Errorf("%s classmap exclusion: %w", name, err)
			}
			excludes = append(excludes, r)
		}
		for _, pattern := range a.Classmap {
			clean, err := autoloadPath(pattern, true)
			if err != nil {
				return fmt.Errorf("%s classmap: %w", name, err)
			}
			if location == ".." && (clean == "." || strings.ContainsAny(strings.Split(clean, "/")[0], "*?[") || strings.Split(clean, "/")[0] == "vendor" || strings.HasPrefix(strings.Split(clean, "/")[0], ".archer-composer-stage-")) {
				return fmt.Errorf("root classmap must name a source directory or file outside vendor and Archer staging")
			}
			matches, err := autoloadMatches(base, clean)
			if err != nil {
				return err
			}
			for _, filename := range matches {
				rel, err := filepath.Rel(base, filename)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				if excludedClassmap(rel, excludes) {
					continue
				}
				data, err := readBoundedFile(filename, 16<<20)
				if err != nil {
					return err
				}
				for _, class := range phpDeclarations(data) {
					if _, exists := classmap[class]; !exists {
						classmap[class] = path.Join(location, rel)
					}
				}
			}
		}
		for _, filename := range a.Files {
			clean, err := autoloadPath(filename, false)
			if err != nil {
				return fmt.Errorf("%s autoload file: %w", name, err)
			}
			if err := autoloadSafeAncestors(base, clean, false); err != nil {
				return fmt.Errorf("%s autoload file: %w", name, err)
			}
			info, err := os.Lstat(filepath.Join(base, filepath.FromSlash(clean)))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("missing or unsafe autoload file %s/%s", name, clean)
			}
			id := md5.Sum([]byte(name + ":" + filename))
			key := hex.EncodeToString(id[:])
			if _, exists := fileMap[key]; !exists {
				fileMap[key] = path.Join(location, clean)
				fileOrder = append(fileOrder, key)
			}
		}
		return nil
	}

	// Root definitions win ambiguous class declarations and prefix precedence.
	root := mergeAutoload(plan.Autoload, plan.AutoloadDev)
	if err := add(projectDir, "..", root, plan.Name); err != nil {
		return nil, err
	}
	rootFiles := append([]string(nil), fileOrder...)
	fileOrder = nil
	ordered := autoloadPackageOrder(plan.Packages)
	for _, pkg := range ordered {
		if !packagePattern.MatchString(pkg.Name) {
			return nil, fmt.Errorf("unsafe Composer package name %q", pkg.Name)
		}
		if err := autoloadSafeAncestors(vendorSource, pkg.Name, false); err != nil {
			return nil, fmt.Errorf("%s autoload package: %w", pkg.Name, err)
		}
		if err := add(filepath.Join(vendorSource, filepath.FromSlash(pkg.Name)), pkg.Name, pkg.Autoload, pkg.Name); err != nil {
			return nil, err
		}
		for _, binary := range pkg.Bin {
			clean, err := autoloadPath(binary, false)
			if err != nil {
				return nil, fmt.Errorf("%s binary: %w", pkg.Name, err)
			}
			name := path.Base(clean)
			key := "bin/" + name
			if _, exists := files[key]; exists {
				return nil, fmt.Errorf("duplicate Composer binary %s", name)
			}
			binaryPath := filepath.Join(vendorSource, filepath.FromSlash(path.Join(pkg.Name, clean)))
			if err := autoloadSafeAncestors(vendorSource, path.Join(pkg.Name, clean), false); err != nil {
				return nil, fmt.Errorf("%s binary: %w", pkg.Name, err)
			}
			info, err := os.Lstat(binaryPath)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
				return nil, fmt.Errorf("missing or unsafe Composer binary %s", binaryPath)
			}
			// This initial engine supports PHP executables; shell/native binaries
			// need a separate proxy and cannot be silently interpreted as PHP.
			data, err := readBoundedFile(binaryPath, 16<<20)
			if err != nil {
				return nil, err
			}
			if !bytes.Contains(data[:min(len(data), 4096)], []byte("<?php")) {
				return nil, fmt.Errorf("non-PHP Composer binary unsupported: %s", binaryPath)
			}
			proxy := "#!/usr/bin/env php\n<?php\n// Generated by Archer.\n$_composer_autoload_path = dirname(__DIR__) . '/autoload.php';\n$_composer_bin_dir = __DIR__;\n$GLOBALS['_composer_autoload_path'] = $_composer_autoload_path;\n$GLOBALS['_composer_bin_dir'] = $_composer_bin_dir;\nreturn require dirname(__DIR__) . " + phpQuote("/"+path.Join(pkg.Name, clean)) + ";\n"
			files[key] = GeneratedFile{[]byte(proxy), true}
		}
	}
	fileOrder = append(fileOrder, rootFiles...)
	files["composer/autoload_psr4.php"] = generatedPHPPathLists(psr4)
	files["composer/autoload_namespaces.php"] = generatedPHPPathLists(psr0)
	files["composer/autoload_classmap.php"] = generatedPHPPaths(classmap, sortedKeys(classmap))
	files["composer/autoload_files.php"] = generatedPHPPaths(fileMap, fileOrder)
	files["composer/ClassLoader.php"] = GeneratedFile{Data: []byte(classLoaderPHP)}
	files["composer/InstalledVersions.php"] = GeneratedFile{Data: []byte(installedVersionsPHP)}
	files["autoload.php"] = GeneratedFile{Data: []byte(autoloadPHP)}
	installed, installedJSON := installedMetadata(plan)
	files["composer/installed.php"] = GeneratedFile{Data: []byte("<?php\n// Generated by Archer.\nreturn " + phpValue(installed) + ";\n")}
	data, err := json.MarshalIndent(installedJSON, "", "    ")
	if err != nil {
		return nil, err
	}
	files["composer/installed.json"] = GeneratedFile{Data: append(data, '\n')}
	return files, nil
}

func mergeAutoload(a, b Autoload) Autoload {
	out := Autoload{PSR4: map[string][]string{}, PSR0: map[string][]string{}}
	for _, source := range []Autoload{a, b} {
		for k, v := range source.PSR4 {
			out.PSR4[k] = append(out.PSR4[k], v...)
		}
		for k, v := range source.PSR0 {
			out.PSR0[k] = append(out.PSR0[k], v...)
		}
		out.Classmap = append(out.Classmap, source.Classmap...)
		out.Files = append(out.Files, source.Files...)
		out.ExcludeFromClassmap = append(out.ExcludeFromClassmap, source.ExcludeFromClassmap...)
	}
	return out
}

func autoloadPath(s string, directory bool) (string, error) {
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.ContainsAny(s, "\x00\r\n") || strings.HasPrefix(s, "/") || strings.Contains(s, ":") {
		return "", fmt.Errorf("unsafe relative path %q", s)
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes package: %q", s)
		}
	}
	clean := path.Clean(s)
	if !directory && (clean == "." || strings.ContainsAny(clean, "*?[]")) {
		return "", fmt.Errorf("expected file path: %q", s)
	}
	return clean, nil
}

func autoloadMatches(base, pattern string) ([]string, error) {
	var starts []string
	literal := []string{}
	for _, part := range strings.Split(pattern, "/") {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		literal = append(literal, part)
	}
	if err := autoloadSafeAncestors(base, strings.Join(literal, "/"), false); err != nil {
		return nil, err
	}
	var recursive *regexp.Regexp
	if strings.Contains(pattern, "**") {
		expression, err := autoloadGlobRegex(pattern)
		if err != nil {
			return nil, err
		}
		recursive, err = regexp.Compile("^" + expression + "$")
		if err != nil {
			return nil, err
		}
		starts = []string{filepath.Join(base, filepath.FromSlash(strings.Join(literal, "/")))}
	} else if strings.ContainsAny(pattern, "*?[") {
		var err error
		starts, err = filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, err
		}
	} else {
		starts = []string{filepath.Join(base, filepath.FromSlash(pattern))}
	}
	unique := map[string]bool{}
	for _, start := range starts {
		rel, err := filepath.Rel(base, start)
		if err != nil {
			return nil, err
		}
		if err := autoloadSafeAncestors(base, filepath.ToSlash(rel), false); err != nil {
			return nil, err
		}
		info, err := os.Lstat(start)
		if err != nil {
			return nil, fmt.Errorf("missing classmap path %s: %w", start, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink classmap path %s", start)
		}
		if err := filepath.WalkDir(start, func(filename string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in classmap: %s", filename)
			}
			if !d.IsDir() && (strings.EqualFold(filepath.Ext(filename), ".php") || strings.EqualFold(filepath.Ext(filename), ".inc")) {
				if recursive != nil {
					rel, err := filepath.Rel(base, filename)
					if err != nil {
						return err
					}
					candidate := filepath.ToSlash(rel)
					matched := false
					for candidate != "." {
						if recursive.MatchString(candidate) {
							matched = true
							break
						}
						candidate = path.Dir(candidate)
					}
					if !matched {
						return nil
					}
				}
				unique[filename] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return sortedKeys(unique), nil
}

func autoloadSafeAncestors(base, relative string, allowMissing bool) error {
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe autoload base %s", base)
	}
	current := base
	for _, component := range strings.Split(relative, "/") {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if allowMissing && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in autoload path: %s", current)
		}
	}
	return nil
}

func excludePattern(s string) (*regexp.Regexp, error) {
	s = strings.ReplaceAll(s, "\\", "/")
	anchored := strings.HasPrefix(s, "/")
	s = strings.Trim(s, "/")
	if _, err := autoloadPath(s, true); err != nil {
		return nil, err
	}
	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("(?:^|/)")
	}
	expression, err := autoloadGlobRegex(s)
	if err != nil {
		return nil, err
	}
	b.WriteString(expression)
	b.WriteString("(?:/|$)")
	return regexp.Compile(b.String())
}

func autoloadGlobRegex(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*':
			if i+1 < len(s) && s[i+1] == '*' {
				if i+2 < len(s) && s[i+2] == '/' {
					b.WriteString("(?:[^/]+/)*")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(s[i+1:], ']')
			if end < 0 {
				return "", fmt.Errorf("unclosed glob character class %q", s)
			}
			value := s[i+1 : i+1+end]
			if strings.HasPrefix(value, "!") {
				value = "^" + value[1:]
			}
			b.WriteString("[" + value + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(s[i : i+1]))
		}
	}
	return b.String(), nil
}

func excludedClassmap(name string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(name) {
			return true
		}
	}
	return false
}

// Dependencies precede dependents; unresolved cycles have stable name order.
func autoloadPackageOrder(packages []Package) []Package {
	byName := map[string]Package{}
	providers := map[string][]string{}
	for _, pkg := range packages {
		byName[pkg.Name] = pkg
		for name := range pkg.Provide {
			providers[name] = append(providers[name], pkg.Name)
		}
		for name := range pkg.Replace {
			providers[name] = append(providers[name], pkg.Name)
		}
	}
	for name := range providers {
		sort.Strings(providers[name])
	}
	state := map[string]int{}
	var out []Package
	var visit func(string)
	visit = func(name string) {
		if state[name] != 0 {
			return
		}
		pkg, ok := byName[name]
		if !ok {
			return
		}
		state[name] = 1
		for _, dependency := range sortedKeys(pkg.Require) {
			if _, exists := byName[dependency]; exists {
				visit(dependency)
			} else {
				for _, provider := range providers[dependency] {
					visit(provider)
				}
			}
		}
		state[name] = 2
		out = append(out, pkg)
	}
	for _, name := range sortedKeys(byName) {
		visit(name)
	}
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func phpQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}

func phpPath(s string) string {
	return "__DIR__ . " + phpQuote("/../"+s)
}

func generatedPHPPaths(values map[string]string, keys []string) GeneratedFile {
	var b strings.Builder
	b.WriteString("<?php\n// Generated by Archer.\nreturn array(\n")
	for _, k := range keys {
		b.WriteString("    " + phpQuote(k) + " => " + phpPath(values[k]) + ",\n")
	}
	b.WriteString(");\n")
	return GeneratedFile{Data: []byte(b.String())}
}

func generatedPHPPathLists(values map[string][]string) GeneratedFile {
	var b strings.Builder
	b.WriteString("<?php\n// Generated by Archer.\nreturn array(\n")
	for _, k := range sortedKeys(values) {
		var paths []string
		for _, p := range values[k] {
			paths = append(paths, phpPath(p))
		}
		b.WriteString("    " + phpQuote(k) + " => array(" + strings.Join(paths, ", ") + "),\n")
	}
	b.WriteString(");\n")
	return GeneratedFile{Data: []byte(b.String())}
}

type phpExpression string

func phpValue(value any) string {
	switch v := value.(type) {
	case phpExpression:
		return string(v)
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case string:
		return phpQuote(v)
	case []string:
		x := make([]string, len(v))
		for i, s := range v {
			x[i] = phpQuote(s)
		}
		return "array(" + strings.Join(x, ", ") + ")"
	case map[string]any:
		x := make([]string, 0, len(v))
		for _, k := range sortedKeys(v) {
			x = append(x, phpQuote(k)+" => "+phpValue(v[k]))
		}
		return "array(" + strings.Join(x, ", ") + ")"
	default:
		panic(fmt.Sprintf("unsupported PHP value %T", value))
	}
}

func normalizedVersion(version string) string {
	v := strings.TrimPrefix(version, "v")
	r := regexp.MustCompile(`^(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:\.(\d+))?(.*)$`)
	m := r.FindStringSubmatch(v)
	if m == nil {
		return v
	}
	for i := 1; i <= 4; i++ {
		if m[i] == "" {
			m[i] = "0"
		}
	}
	return strings.Join(m[1:5], ".") + m[5]
}

func installedMetadata(plan *Plan) (map[string]any, map[string]any) {
	root := map[string]any{"name": plan.Name, "pretty_version": plan.Version, "version": normalizedVersion(plan.Version), "reference": nil, "type": "project", "install_path": phpExpression("__DIR__ . '/../..'"), "aliases": []string{}, "dev": true}
	versions := map[string]any{plan.Name: root}
	packages := make([]map[string]any, 0, len(plan.Packages))
	devNames := []string{}
	for _, pkg := range plan.Packages {
		versions[pkg.Name] = map[string]any{"pretty_version": pkg.Version, "version": normalizedVersion(pkg.Version), "reference": pkg.Reference, "type": pkg.Type, "install_path": phpExpression("__DIR__ . " + phpQuote("/../"+pkg.Name)), "aliases": []string{}, "dev_requirement": pkg.Dev}
		p := map[string]any{"name": pkg.Name, "version": pkg.Version, "version_normalized": normalizedVersion(pkg.Version), "type": pkg.Type, "install-path": "../" + pkg.Name, "installation-source": "dist", "require": pkg.Require, "extra": pkg.Extra}
		packages = append(packages, p)
		if pkg.Dev {
			devNames = append(devNames, pkg.Name)
		}
	}
	for _, pkg := range plan.Packages {
		for field, links := range map[string]map[string]string{"provided": pkg.Provide, "replaced": pkg.Replace} {
			for _, name := range sortedKeys(links) {
				value, exists := versions[name]
				if !exists {
					value = map[string]any{"dev_requirement": pkg.Dev}
					versions[name] = value
				}
				entry := value.(map[string]any)
				constraint := links[name]
				if constraint == "self.version" {
					constraint = pkg.Version
				}
				old, _ := entry[field].([]string)
				entry[field] = append(old, constraint)
			}
		}
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i]["name"].(string) < packages[j]["name"].(string) })
	sort.Strings(devNames)
	return map[string]any{"root": root, "versions": versions}, map[string]any{"packages": packages, "dev": true, "dev-package-names": devNames}
}

// phpDeclarations tokenizes syntax only. Strings, comments, heredocs, anonymous
// classes and Foo::class constants cannot create spurious map entries.
func phpDeclarations(source []byte) []string {
	tokens := phpTokens(source)
	namespace := ""
	depth, namespaceDepth := 0, -1
	var declarations []string
	for i := 0; i < len(tokens); i++ {
		token := strings.ToLower(tokens[i])
		if token == "}" {
			depth--
			if depth == namespaceDepth {
				namespace, namespaceDepth = "", -1
			}
			continue
		}
		if token == "{" {
			depth++
			continue
		}
		if token == "namespace" && (i+1 >= len(tokens) || tokens[i+1] != "\\") && (i == 0 || tokens[i-1] == ";" || tokens[i-1] == "{" || tokens[i-1] == "}") {
			j := i + 1
			var parts []string
			for j < len(tokens) && tokens[j] != ";" && tokens[j] != "{" {
				parts = append(parts, tokens[j])
				j++
			}
			if j < len(tokens) {
				namespace = strings.Trim(strings.Join(parts, ""), "\\")
				if tokens[j] == "{" {
					namespaceDepth = depth
					depth++
				}
				i = j
			}
			continue
		}
		if token != "class" && token != "interface" && token != "trait" && token != "enum" {
			continue
		}
		if i > 0 && (tokens[i-1] == "::" || strings.EqualFold(tokens[i-1], "new")) {
			continue
		}
		if i+1 < len(tokens) && phpIdentifier(tokens[i+1]) && !strings.EqualFold(tokens[i+1], "extends") && !strings.EqualFold(tokens[i+1], "implements") {
			name := tokens[i+1]
			if namespace != "" {
				name = namespace + "\\" + name
			}
			declarations = append(declarations, name)
		}
	}
	return declarations
}

func phpIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(unicode.IsLetter(r) || r == '_' || r >= 128 || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

func phpTokens(source []byte) []string {
	s := string(source)
	var tokens []string
	inPHP := false
	for i := 0; i < len(s); {
		if !inPHP {
			j := strings.Index(s[i:], "<?")
			if j < 0 {
				break
			}
			i += j + 2
			if strings.HasPrefix(strings.ToLower(s[i:]), "php") {
				i += 3
			}
			inPHP = true
			continue
		}
		if strings.HasPrefix(s[i:], "?>") {
			i += 2
			inPHP = false
			continue
		}
		if strings.HasPrefix(s[i:], "//") || s[i] == '#' && !strings.HasPrefix(s[i:], "#[") {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break
			}
			i += j + 4
			continue
		}
		if s[i] == '\'' || s[i] == '"' || s[i] == '`' {
			quote := s[i]
			i++
			for i < len(s) {
				if s[i] == '\\' {
					i += min(2, len(s)-i)
				} else if s[i] == quote {
					i++
					break
				} else {
					i++
				}
			}
			continue
		}
		if strings.HasPrefix(s[i:], "<<<") {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			label := strings.Trim(strings.TrimSpace(s[i+3:i+j]), "'\"")
			i += j + 1
			for i < len(s) {
				end := strings.IndexByte(s[i:], '\n')
				if end < 0 {
					end = len(s) - i
				}
				line := strings.TrimSpace(s[i : i+end])
				i += end
				if i < len(s) {
					i++
				}
				if strings.HasPrefix(line, label) && (len(line) == len(label) || !phpIdentifier(line[len(label):len(label)+1])) {
					break
				}
			}
			continue
		}
		if unicode.IsSpace(rune(s[i])) {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "::") {
			tokens = append(tokens, "::")
			i += 2
			continue
		}
		if s[i] == '_' || s[i] >= 128 || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' {
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] >= 128 || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			tokens = append(tokens, s[i:j])
			i = j
			continue
		}
		tokens = append(tokens, s[i:i+1])
		i++
	}
	return tokens
}

const autoloadPHP = `<?php
// Generated by Archer. Loader initialization is idempotent.
if (isset($GLOBALS['__archer_composer_loaders'][__DIR__]) && $GLOBALS['__archer_composer_loaders'][__DIR__] instanceof \Composer\Autoload\ClassLoader) {
    return $GLOBALS['__archer_composer_loaders'][__DIR__];
}
require_once __DIR__ . '/composer/ClassLoader.php';
$loader = new \Composer\Autoload\ClassLoader(__DIR__);
foreach (require __DIR__ . '/composer/autoload_psr4.php' as $prefix => $paths) { $loader->setPsr4($prefix, $paths); }
foreach (require __DIR__ . '/composer/autoload_namespaces.php' as $prefix => $paths) { $loader->set($prefix, $paths); }
$loader->addClassMap(require __DIR__ . '/composer/autoload_classmap.php');
$loader->register(true);
$GLOBALS['__archer_composer_loaders'][__DIR__] = $loader;
foreach (require __DIR__ . '/composer/autoload_files.php' as $id => $file) {
    if (empty($GLOBALS['__composer_autoload_files'][$id])) {
        $GLOBALS['__composer_autoload_files'][$id] = true;
        (static function ($path) { require $path; })($file);
    }
}
return $loader;
`

// This is Archer's implementation of the public Composer ClassLoader API.
const classLoaderPHP = `<?php
namespace Composer\Autoload;
class ClassLoader {
    private $prefixesPsr4 = array();
    private $prefixes = array();
    private $fallbackPsr4 = array();
    private $fallback = array();
    private $classMap = array();
    private $missing = array();
    private $authoritative = false;
    private $useIncludePath = false;
    private $apcuPrefix = null;
    private $vendorDir;
    private static $registered = array();
    public function __construct($vendorDir = null) { $this->vendorDir = $vendorDir; }
    public function getPrefixes() { return $this->prefixes; }
    public function getPrefixesPsr4() { return $this->prefixesPsr4; }
    public function getFallbackDirs() { return $this->fallback; }
    public function getFallbackDirsPsr4() { return $this->fallbackPsr4; }
    public function getClassMap() { return $this->classMap; }
    public function addClassMap(array $classMap) { $this->classMap = array_merge($this->classMap, $classMap); $this->missing = array(); }
    public function addPsr4($prefix, $paths, $prepend = false) {
        $paths = (array) $paths;
        if ($prefix !== '' && substr($prefix, -1) !== '\\') { throw new \InvalidArgumentException('PSR-4 prefix must end with a namespace separator'); }
        if ($prefix === '') { $this->fallbackPsr4 = $prepend ? array_merge($paths, $this->fallbackPsr4) : array_merge($this->fallbackPsr4, $paths); }
        else { $old = isset($this->prefixesPsr4[$prefix]) ? $this->prefixesPsr4[$prefix] : array(); $this->prefixesPsr4[$prefix] = $prepend ? array_merge($paths, $old) : array_merge($old, $paths); }
        $this->missing = array();
    }
    public function setPsr4($prefix, $paths) {
        if ($prefix !== '' && substr($prefix, -1) !== '\\') { throw new \InvalidArgumentException('PSR-4 prefix must end with a namespace separator'); }
        if ($prefix === '') { $this->fallbackPsr4 = (array) $paths; } else { $this->prefixesPsr4[$prefix] = (array) $paths; }
        $this->missing = array();
    }
    public function add($prefix, $paths, $prepend = false) {
        $paths = (array) $paths;
        if ($prefix === '') { $this->fallback = $prepend ? array_merge($paths, $this->fallback) : array_merge($this->fallback, $paths); }
        else { $old = isset($this->prefixes[$prefix]) ? $this->prefixes[$prefix] : array(); $this->prefixes[$prefix] = $prepend ? array_merge($paths, $old) : array_merge($old, $paths); }
        $this->missing = array();
    }
    public function set($prefix, $paths) { if ($prefix === '') { $this->fallback = (array) $paths; } else { $this->prefixes[$prefix] = (array) $paths; } $this->missing = array(); }
    public function setUseIncludePath($value) { $this->useIncludePath = (bool) $value; }
    public function getUseIncludePath() { return $this->useIncludePath; }
    public function setClassMapAuthoritative($value) { $this->authoritative = (bool) $value; }
    public function isClassMapAuthoritative() { return $this->authoritative; }
    public function setApcuPrefix($prefix) { $this->apcuPrefix = $prefix; }
    public function getApcuPrefix() { return $this->apcuPrefix; }
    public function register($prepend = false) { spl_autoload_register(array($this, 'loadClass'), true, $prepend); if ($this->vendorDir !== null) { self::$registered[$this->vendorDir] = $this; } }
    public function unregister() { spl_autoload_unregister(array($this, 'loadClass')); if ($this->vendorDir !== null) { unset(self::$registered[$this->vendorDir]); } }
    public static function getRegisteredLoaders() { return self::$registered; }
    public function loadClass($class) { if ($file = $this->findFile($class)) { (static function ($path) { include $path; })($file); return true; } return null; }
    public function findFile($class) {
        if (isset($this->classMap[$class])) { return $this->classMap[$class]; }
        if ($this->authoritative || isset($this->missing[$class])) { return false; }
        $logical = str_replace('\\', DIRECTORY_SEPARATOR, $class) . '.php';
        $prefix = $class;
        while (false !== ($position = strrpos($prefix, '\\'))) {
            $prefix = substr($prefix, 0, $position);
            $key = $prefix . '\\';
            if (isset($this->prefixesPsr4[$key])) { $suffix = substr($logical, strlen($key)); foreach ($this->prefixesPsr4[$key] as $directory) { $file = $directory . DIRECTORY_SEPARATOR . $suffix; if (is_file($file)) { return $file; } } }
        }
        foreach ($this->fallbackPsr4 as $directory) { $file = $directory . DIRECTORY_SEPARATOR . $logical; if (is_file($file)) { return $file; } }
        $position = strrpos($class, '\\');
        $legacy = $position === false ? str_replace('_', DIRECTORY_SEPARATOR, $class) : str_replace('\\', DIRECTORY_SEPARATOR, substr($class, 0, $position + 1)) . str_replace('_', DIRECTORY_SEPARATOR, substr($class, $position + 1));
        $legacy .= '.php';
        foreach ($this->prefixes as $key => $directories) { if (strncmp($class, $key, strlen($key)) === 0) { foreach ($directories as $directory) { $file = $directory . DIRECTORY_SEPARATOR . $legacy; if (is_file($file)) { return $file; } } } }
        foreach ($this->fallback as $directory) { $file = $directory . DIRECTORY_SEPARATOR . $legacy; if (is_file($file)) { return $file; } }
        if ($this->useIncludePath && ($file = stream_resolve_include_path($legacy))) { return $file; }
        $this->missing[$class] = true;
        return false;
    }
}
`

const installedVersionsPHP = `<?php
namespace Composer;
class InstalledVersions {
    private static $data = null;
    private static function data() { if (self::$data === null) { self::$data = require __DIR__ . '/installed.php'; } return self::$data; }
    private static function entry($package) { $data = self::data(); if (!isset($data['versions'][$package])) { throw new \OutOfBoundsException('Package "' . $package . '" is not installed'); } return $data['versions'][$package]; }
    public static function getInstalledPackages() { return array_keys(self::data()['versions']); }
    public static function getInstalledPackagesByType($type) { $out = array(); foreach (self::data()['versions'] as $name => $entry) { if (isset($entry['type']) && $entry['type'] === $type) { $out[] = $name; } } return $out; }
    public static function isInstalled($package, $includeDevRequirements = true) { $data = self::data(); return isset($data['versions'][$package]) && ($includeDevRequirements || empty($data['versions'][$package]['dev_requirement'])); }
    public static function satisfies($parser, $package, $constraint) { return $parser->parseConstraints($constraint)->matches($parser->parseConstraints(self::getVersionRanges($package))); }
    public static function getVersionRanges($package) { $entry = self::entry($package); $ranges = array(); if (isset($entry['pretty_version'])) { $ranges[] = $entry['pretty_version']; } foreach (array('aliases', 'replaced', 'provided') as $key) { if (isset($entry[$key])) { $ranges = array_merge($ranges, $entry[$key]); } } return implode(' || ', $ranges); }
    public static function getVersion($package) { $entry = self::entry($package); return isset($entry['version']) ? $entry['version'] : null; }
    public static function getPrettyVersion($package) { $entry = self::entry($package); return isset($entry['pretty_version']) ? $entry['pretty_version'] : null; }
    public static function getReference($package) { $entry = self::entry($package); return isset($entry['reference']) ? $entry['reference'] : null; }
    public static function getInstallPath($package) { $entry = self::entry($package); return isset($entry['install_path']) ? $entry['install_path'] : null; }
    public static function getRootPackage() { return self::data()['root']; }
    public static function getRawData() { return self::data(); }
    public static function getAllRawData() { return array(self::data()); }
    public static function reload($data) { self::$data = $data; }
}
`
