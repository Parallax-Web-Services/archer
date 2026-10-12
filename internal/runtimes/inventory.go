package runtimes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type receipt struct {
	Schema         string  `json:"schema"`
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Version        string  `json:"version"`
	ArtifactSHA256 string  `json:"artifact_sha256"`
	Inventory      []entry `json:"inventory"`
}

func withPHPIni(p Plan, inventory []entry) ([]entry, error) {
	if p.Kind != "php" {
		return inventory, nil
	}
	if p.Spec.PHPIni == "" {
		h := sha256.Sum256(nil)
		result := append([]entry(nil), inventory...)
		result = append(result, entry{Path: emptyIni, Kind: "file", Mode: 0644, SHA256: hex.EncodeToString(h[:])})
		sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
		return result, nil
	}
	if _, err := safeRelative(p.Spec.PHPIni); err != nil {
		return nil, fmt.Errorf("managed PHP ini must be a safe bundled relative file: %w", err)
	}
	for _, e := range inventory {
		if e.Path == p.Spec.PHPIni {
			if e.Kind != "file" {
				return nil, errors.New("managed PHP ini must be a regular file")
			}
			return inventory, nil
		}
	}
	return nil, errors.New("declared PHP ini is absent from runtime bundle")
}
func sameInventory(a, b []entry) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
func expectedReceipt(p Plan, b Bundle, inventory []entry) receipt {
	return receipt{Schema: "archer.runtime.receipt.v1", ID: p.ID, Kind: p.Kind, Version: p.Version, ArtifactSHA256: strings.ToLower(b.Artifact.SHA256), Inventory: inventory}
}
func writeReceipt(root string, p Plan, b Bundle, inventory []entry) error {
	data, err := json.MarshalIndent(expectedReceipt(p, b, inventory), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, receiptName), append(data, '\n'), 0644)
}
func verifyManaged(root string, p Plan, b Bundle, expected []entry) error {
	if err := checkDirectories(root, false); err != nil {
		return err
	}
	if info, err := os.Lstat(root); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
		return errors.New("runtime root permissions changed; tree preserved")
	}
	file := filepath.Join(root, receiptName)
	if _, err := hashRegular(file, 16<<20); err != nil {
		return fmt.Errorf("runtime ownership receipt unavailable: %w", err)
	}
	if info, err := os.Lstat(file); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0644) {
		return errors.New("runtime receipt permissions changed; tree preserved")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var got receipt
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return fmt.Errorf("invalid runtime receipt: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("runtime receipt has trailing content")
	}
	actualJSON, _ := json.Marshal(got)
	expectedJSON, _ := json.Marshal(expectedReceipt(p, b, expected))
	if !bytes.Equal(actualJSON, expectedJSON) {
		return errors.New("runtime receipt does not match hash-verified archive inventory; tree preserved")
	}
	wanted := map[string]entry{}
	for _, e := range expected {
		wanted[e.Path] = e
	}
	seen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if name == receiptName {
			if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
				return errors.New("unsafe runtime receipt")
			}
			return nil
		}
		expected, ok := wanted[name]
		if !ok {
			return fmt.Errorf("runtime tree contains unexpected path %s; tree preserved", name)
		}
		seen++
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch expected.Kind {
		case "dir":
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("runtime directory changed: %s", name)
			}
		case "link":
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("runtime symlink changed: %s", name)
			}
			target, err := os.Readlink(path)
			if err != nil || filepath.ToSlash(target) != expected.Target {
				return fmt.Errorf("runtime symlink target changed: %s", name)
			}
		case "file":
			if !info.Mode().IsRegular() || info.Size() != expected.Size {
				return fmt.Errorf("runtime file changed: %s", name)
			}
			hash, err := hashRegular(path, maxTreeBytes)
			if err != nil || hash != expected.SHA256 {
				return fmt.Errorf("runtime file checksum changed: %s; tree preserved", name)
			}
		default:
			return errors.New("unknown runtime inventory entry type")
		}
		if runtime.GOOS != "windows" && expected.Kind != "link" && uint32(info.Mode().Perm()) != expected.Mode {
			return fmt.Errorf("runtime file permissions changed: %s", name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(expected) {
		return errors.New("runtime tree is missing archive paths; tree preserved")
	}
	return nil
}
