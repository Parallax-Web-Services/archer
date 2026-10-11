// Package store keeps verified artifacts in a shared content-addressed store.
// Reusable artifact bytes are never evidence of current installation state.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const MaxArtifactBytes int64 = 256 << 20
const lockWait = 30 * time.Second

type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type Store struct{ root string }

func New(root string) *Store { return &Store{root: root} }

func digest(value string) (string, error) {
	if len(value) != 64 {
		return "", errors.New("SHA256 requires exactly 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("SHA256 requires exactly 64 hexadecimal characters")
	}
	return strings.ToLower(value), nil
}
func (s *Store) objectPath(hash string) (string, error) {
	if s.root == "" {
		return "", errors.New("artifact store root is empty")
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sha256", hash[:2], hash), nil
}

// Has rehashes every hit and rejects corrupt or non-regular objects.
func (s *Store) Has(value string) (path string, ok bool, err error) {
	hash, err := digest(value)
	if err != nil {
		return "", false, err
	}
	path, err = s.objectPath(hash)
	if err != nil {
		return "", false, err
	}
	if err := directory(filepath.Dir(path), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	actual, _, err := hashFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if actual != hash {
		return "", false, fmt.Errorf("cached artifact checksum mismatch: expected %s, got %s", hash, actual)
	}
	return path, true, nil
}

// Ensure serializes downloads by digest. Abandoned locks cause bounded errors;
// they are never deleted automatically. Failed downloads leave no object.
func (s *Store) Ensure(ctx context.Context, a Artifact) (path string, cached bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	hash, err := digest(a.SHA256)
	if err != nil {
		return "", false, err
	}
	if path, cached, err = s.Has(hash); err != nil || cached {
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return "", false, err
		}
		return path, cached, err
	}
	path, err = s.objectPath(hash)
	if err != nil {
		return "", false, err
	}
	if err := directory(filepath.Dir(path), true); err != nil {
		return "", false, err
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	locks := filepath.Join(root, "locks")
	if err := directory(locks, true); err != nil {
		return "", false, err
	}
	unlock, err := acquire(ctx, filepath.Join(locks, hash))
	if err != nil {
		return "", false, err
	}
	defer unlock()
	if path, cached, err = s.Has(hash); err != nil || cached {
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return "", false, err
		}
		return path, cached, err
	}
	path, _ = s.objectPath(hash)
	reader, err := artifactReader(ctx, a.URL)
	if err != nil {
		return "", false, err
	}
	defer reader.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".incoming-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(&contextReader{ctx, reader}, MaxArtifactBytes+1))
	if copyErr == nil && n > MaxArtifactBytes {
		copyErr = errors.New("artifact exceeds 256 MiB limit")
	}
	if copyErr == nil && hex.EncodeToString(hasher.Sum(nil)) != hash {
		copyErr = fmt.Errorf("downloaded artifact checksum mismatch for %s", hash)
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", false, copyErr
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	// Link atomically creates the object without replacing an existing path.
	if err := os.Link(tmp.Name(), path); err != nil {
		if existing, ok, checkErr := s.Has(hash); checkErr != nil || ok {
			return existing, ok, checkErr
		}
		return "", false, fmt.Errorf("publish artifact object: %w", err)
	}
	return path, false, nil
}

// Publish creates an executable from a verified object returned by Ensure/Has.
// It requires an absolute target, never overwrites an existing file or symlink,
// and reuses matching executable files. The source is checked again while copying.
func (s *Store) Publish(path, target string) (reused bool, err error) {
	hash, err := digest(filepath.Base(path))
	if err != nil {
		return false, errors.New("publish source must be a SHA256 store object")
	}
	verified, ok, err := s.Has(hash)
	if err != nil {
		return false, err
	}
	if !ok || filepath.Clean(verified) != filepath.Clean(path) {
		return false, errors.New("publish source is not a verified object in this store")
	}
	if !filepath.IsAbs(target) {
		return false, errors.New("executable target must be an absolute path")
	}
	if err := directory(filepath.Dir(target), true); err != nil {
		return false, err
	}
	if reused, err := matchingTarget(target, hash); err != nil || reused {
		return reused, err
	}
	source, info, err := openRegular(path)
	if err != nil {
		return false, err
	}
	defer source.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".archer-executable-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(source, MaxArtifactBytes+1))
	if copyErr == nil && (n > MaxArtifactBytes || hex.EncodeToString(hasher.Sum(nil)) != hash) {
		copyErr = errors.New("artifact changed while publishing executable")
	}
	if copyErr == nil {
		copyErr = unchanged(path, info)
	}
	if copyErr == nil {
		copyErr = tmp.Chmod(0755)
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return false, copyErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err := os.Link(tmp.Name(), target); err != nil {
		if reused, checkErr := matchingTarget(target, hash); checkErr != nil || reused {
			return reused, checkErr
		}
		return false, fmt.Errorf("publish executable: %w", err)
	}
	return false, nil
}
func matchingTarget(path, hash string) (bool, error) {
	actual, info, err := hashFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if actual != hash {
		return false, fmt.Errorf("refusing to overwrite different executable at %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		f, opened, err := openRegular(path)
		if err != nil {
			return false, err
		}
		defer f.Close()
		if !os.SameFile(info, opened) {
			return false, errors.New("executable target changed during verification")
		}
		if err := unchanged(path, info); err != nil {
			return false, err
		}
		if err := f.Chmod(0755); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Walk parents without following symlinks. No existing entries are deleted.
func directory(path string, create bool) error {
	path = filepath.Clean(path)
	if parent := filepath.Dir(path); parent != path {
		if err := directory(parent, create); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe directory at %s", path)
	}
	return nil
}
func openRegular(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("refusing non-regular file at %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		f.Close()
		return nil, nil, fmt.Errorf("file changed while opening %s", path)
	}
	return f, opened, nil
}
func unchanged(path string, before os.FileInfo) error {
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("file changed while reading %s", path)
	}
	return nil
}
func hashFile(path string) (string, os.FileInfo, error) {
	f, info, err := openRegular(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if info.Size() > MaxArtifactBytes {
		return "", nil, errors.New("stored file exceeds artifact size limit")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxArtifactBytes+1))
	if err != nil {
		return "", nil, err
	}
	if n > MaxArtifactBytes {
		return "", nil, errors.New("stored file exceeds artifact size limit")
	}
	if err := unchanged(path, info); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), info, nil
}
func acquire(ctx context.Context, path string) (func(), error) {
	deadline := time.NewTimer(lockWait)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := os.Mkdir(path, 0700)
		if err == nil {
			owned, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			return func() {
				current, err := os.Lstat(path)
				if err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(owned, current) {
					_ = os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe artifact lock at %s", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for artifact lock %s; existing lock was preserved", path)
		case <-tick.C:
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func artifactReader(ctx context.Context, source string) (io.ReadCloser, error) {
	if filepath.IsAbs(source) {
		f, _, err := openRegular(source)
		return f, err
	}
	u, err := url.Parse(source)
	if err != nil || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid artifact URL")
	}
	if u.Scheme == "file" {
		if (u.Host != "" && u.Host != "localhost") || u.RawQuery != "" {
			return nil, errors.New("file artifact URL must address a local absolute path")
		}
		path := filepath.FromSlash(u.Path)
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '\\' && path[2] == ':' {
			path = path[1:]
		}
		if !filepath.IsAbs(path) {
			return nil, errors.New("file artifact URL must address a local absolute path")
		}
		f, _, err := openRegular(path)
		return f, err
	}
	if u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("artifact URL requires HTTPS, loopback HTTP, or an absolute local file")
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return nil, errors.New("HTTP artifact downloads are allowed only on loopback")
	}
	origin := urlOrigin(u)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "http" {
		// Do not let proxy settings or a hosts-file override send loopback HTTP
		// elsewhere. localhost is dialed directly as a loopback IP.
		transport.Proxy = nil
		transport.DialContext = dialLoopback
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.User != nil || urlOrigin(req.URL) != origin {
			return errors.New("artifact redirect changed origin or exceeded redirect limit")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxArtifactBytes {
		response.Body.Close()
		return nil, fmt.Errorf("artifact response refused: status %d, length %d", response.StatusCode, response.ContentLength)
	}
	return response.Body, nil
}
func dialLoopback(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses := []string{host}
	if strings.EqualFold(host, "localhost") {
		addresses = []string{"127.0.0.1", "::1"}
	}
	var last error
	for _, host := range addresses {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("HTTP connection must use a loopback address")
		}
		connection, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(host, port))
		if err == nil {
			return connection, nil
		}
		last = err
	}
	return nil, last
}
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
func urlOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else if u.Scheme == "http" {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(net.JoinHostPort(u.Hostname(), port))
}
