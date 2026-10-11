package archer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func readJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > 2<<20 {
		return fmt.Errorf("configuration exceeds 2 MiB")
	}
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("configuration must contain one JSON object")
	}
	return nil
}
func atomicJSON(path string, value any) error {
	if err := checkedDirectories(filepath.Dir(path), true); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink state file %s", path)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archer-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (e *Engine) Context() (Context, error) {
	c := Context{Schema: "archer.context/v1", Scope: e.scopeKey, Bindings: map[string]string{}, Outcomes: []Outcome{}}
	if err := checkedDirectories(filepath.Dir(e.contextPath()), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, err
	}
	if info, err := os.Lstat(e.contextPath()); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return c, fmt.Errorf("symlink context file refused")
	}
	err := readJSON(e.contextPath(), &c)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if c.Schema != "archer.context/v1" || c.Scope != e.scopeKey {
		return c, fmt.Errorf("context scope/schema mismatch")
	}
	if c.Bindings == nil {
		c.Bindings = map[string]string{}
	}
	return c, nil
}
func (e *Engine) contextPath() string { return filepath.Join(e.Home, "contexts", e.scopeKey+".json") }
func (e *Engine) record(plan Plan) error {
	if err := checkedDirectories(filepath.Dir(e.contextPath()), true); err != nil {
		return err
	}
	lock := e.contextPath() + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lock, 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(lock)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe context lock")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("context is locked; inspect abandoned lock %s", lock)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer os.Remove(lock)
	c, err := e.Context()
	if err != nil {
		return err
	}
	changed := false
	for _, d := range plan.Decisions {
		if d.Status != "service" && d.Status != "local" && d.Status != "installed" {
			continue
		}
		c.Bindings[d.Requirement.Capability] = d.Provider
		c.Outcomes = append(c.Outcomes, Outcome{Capability: d.Requirement.Capability, Provider: d.Provider, Status: d.Status, At: time.Now().UTC().Format(time.RFC3339)})
		changed = true
	}
	if !changed {
		return nil
	}
	if len(c.Outcomes) > 100 {
		c.Outcomes = c.Outcomes[len(c.Outcomes)-100:]
	}
	c.Revision++
	return atomicJSON(e.contextPath(), c)
}

func checkedDirectories(path string, create bool) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("unsafe state directory %s", current)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if create {
		return os.MkdirAll(path, 0700)
	}
	_, err := os.Stat(path)
	return err
}
