package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sum(data string) string {
	value := sha256.Sum256([]byte(data))
	return hex.EncodeToString(value[:])
}
func fixture(t *testing.T, data string) Artifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return Artifact{URL: path, SHA256: sum(data)}
}
func ensure(t *testing.T, s *Store, a Artifact) string {
	t.Helper()
	path, _, err := s.Ensure(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWarmCacheAvoidsDownloadAndRejectsTamper(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fmt.Fprint(w, "verified binary") }))
	defer server.Close()
	s := New(filepath.Join(t.TempDir(), "cache"))
	a := Artifact{URL: server.URL, SHA256: sum("verified binary")}
	path, cached, err := s.Ensure(context.Background(), a)
	if err != nil || cached {
		t.Fatalf("first download: cached=%v err=%v", cached, err)
	}
	second, cached, err := New(s.root).Ensure(context.Background(), a)
	if err != nil || !cached || second != path || requests.Load() != 1 {
		t.Fatalf("cache reuse: path=%q cached=%v requests=%d err=%v", second, cached, requests.Load(), err)
	}
	if err := os.WriteFile(path, []byte("tampered binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Has(a.SHA256); err == nil || ok {
		t.Fatal("tampered object was accepted")
	}
	if _, _, err := s.Ensure(context.Background(), a); err == nil {
		t.Fatal("tampered cache was silently reused or repaired")
	}
	if requests.Load() != 1 {
		t.Fatal("tampered cache unexpectedly triggered a download")
	}
}

func TestConcurrentStoresDownloadOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(40 * time.Millisecond)
		fmt.Fprint(w, "concurrent payload")
	}))
	defer server.Close()
	root := filepath.Join(t.TempDir(), "cache")
	a := Artifact{URL: server.URL, SHA256: sum("concurrent payload")}
	start := make(chan struct{})
	results := make(chan error, 12)
	var workers sync.WaitGroup
	var misses atomic.Int32
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			path, cached, err := New(root).Ensure(ctx, a)
			if err == nil && path == "" {
				err = errors.New("empty object path")
			}
			if err == nil && !cached {
				misses.Add(1)
			}
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 || misses.Load() != 1 {
		t.Fatalf("downloads=%d misses=%d", requests.Load(), misses.Load())
	}
}

func TestBadChecksumLeavesNoObjectOrTemporaryFile(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "cache"))
	a := fixture(t, "wrong bytes")
	a.SHA256 = sum("expected bytes")
	if _, _, err := s.Ensure(context.Background(), a); err == nil {
		t.Fatal("invalid checksum accepted")
	}
	if _, ok, err := s.Has(a.SHA256); err != nil || ok {
		t.Fatalf("partial object retained: ok=%v err=%v", ok, err)
	}
	objects, err := filepath.Glob(filepath.Join(s.root, "sha256", a.SHA256[:2], "*"))
	if err != nil || len(objects) != 0 {
		t.Fatalf("temporary files remain: %v %v", objects, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "locks", a.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned lock not released: %v", err)
	}
}

func TestInvalidDigestCannotEscapeStore(t *testing.T) {
	s := New(t.TempDir())
	for _, value := range []string{"", "../outside", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, _, err := s.Ensure(context.Background(), Artifact{URL: "https://unused.example", SHA256: value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
		if _, _, err := s.Has(value); err == nil {
			t.Fatalf("Has accepted %q", value)
		}
	}
}

func TestFileURLAndAbsoluteFile(t *testing.T) {
	a := fixture(t, "offline bytes")
	s := New(filepath.Join(t.TempDir(), "cache"))
	filePath := filepath.ToSlash(a.URL)
	if runtime.GOOS == "windows" {
		filePath = "/" + filePath
	}
	a.URL = (&url.URL{Scheme: "file", Path: filePath}).String()
	path := ensure(t, s, a)
	if _, ok, err := s.Has(strings.ToUpper(a.SHA256)); err != nil || !ok {
		t.Fatalf("normalized digest: %v %v", ok, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "offline bytes" {
		t.Fatalf("offline file: %q %v", data, err)
	}
}

func TestPublishPreservesExistingFiles(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "cache"))
	path := ensure(t, s, fixture(t, "executable bytes"))
	target := filepath.Join(t.TempDir(), "bin", "tool")
	if reused, err := s.Publish(path, target); err != nil || reused {
		t.Fatalf("publish: reused=%v err=%v", reused, err)
	}
	if reused, err := s.Publish(path, target); err != nil || !reused {
		t.Fatalf("reuse: reused=%v err=%v", reused, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0600); err != nil {
			t.Fatal(err)
		}
		if reused, err := s.Publish(path, target); err != nil || !reused {
			t.Fatalf("matching mode repair: %v %v", reused, err)
		}
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("mode=%v err=%v", info, err)
		}
	}
	if err := os.WriteFile(target, []byte("user's existing tool"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(path, target); err == nil {
		t.Fatal("different executable overwritten")
	}
	if data, _ := os.ReadFile(target); string(data) != "user's existing tool" {
		t.Fatal("existing bytes changed")
	}
}

func TestPublishRejectsSymlink(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "cache"))
	path := ensure(t, s, fixture(t, "executable bytes"))
	target := filepath.Join(t.TempDir(), "user-tool")
	if err := os.WriteFile(target, []byte("user's tool"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(target), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.Publish(path, link); err == nil {
		t.Fatal("target symlink followed")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("target symlink changed")
	}
}

func TestConcurrentPublishDoesNotOverwrite(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "cache"))
	path := ensure(t, s, fixture(t, "executable bytes"))
	target := filepath.Join(t.TempDir(), "tool")
	var workers sync.WaitGroup
	results := make(chan error, 8)
	var created atomic.Int32
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			reused, err := s.Publish(path, target)
			if err == nil && !reused {
				created.Add(1)
			}
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d executable targets", created.Load())
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "executable bytes" {
		t.Fatalf("target=%q err=%v", data, err)
	}
}

func TestPublishRejectsTamperedSource(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "cache"))
	path := ensure(t, s, fixture(t, "original"))
	if err := os.WriteFile(path, []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "tool")
	if _, err := s.Publish(path, target); err == nil {
		t.Fatal("tampered source published")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target exists after failed verification")
	}
}

func TestRedirectOriginPolicy(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationRequests.Add(1); fmt.Fprint(w, "bytes") }))
	defer destination.Close()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cross":
			http.Redirect(w, r, destination.URL, http.StatusFound)
		case "/same":
			http.Redirect(w, r, server.URL+"/bytes", http.StatusFound)
		default:
			fmt.Fprint(w, "bytes")
		}
	}))
	defer server.Close()
	if _, _, err := New(t.TempDir()).Ensure(context.Background(), Artifact{URL: server.URL + "/cross", SHA256: sum("bytes")}); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	if destinationRequests.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
	ensure(t, New(t.TempDir()), Artifact{URL: server.URL + "/same", SHA256: sum("bytes")})
	if _, _, err := New(t.TempDir()).Ensure(context.Background(), Artifact{URL: "http://192.0.2.1/artifact", SHA256: sum("bytes")}); err == nil {
		t.Fatal("non-loopback HTTP accepted")
	}
}

func TestOversizedResponseRejectedBeforeReading(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(MaxArtifactBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s := New(t.TempDir())
	a := Artifact{URL: server.URL, SHA256: sum("bytes")}
	if _, _, err := s.Ensure(context.Background(), a); err == nil {
		t.Fatal("oversized response accepted")
	}
	if _, ok, err := s.Has(a.SHA256); err != nil || ok {
		t.Fatalf("oversized object retained: %v %v", ok, err)
	}
}

func TestContextCancellationAndAbandonedLockPreserved(t *testing.T) {
	s := New(t.TempDir())
	a := fixture(t, "bytes")
	lock := filepath.Join(s.root, "locks", a.SHA256)
	if err := os.MkdirAll(lock, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := s.Ensure(ctx, a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation: %v", err)
	}
	if info, err := os.Lstat(lock); err != nil || !info.IsDir() {
		t.Fatal("abandoned lock was deleted")
	}
}

func TestCanceledDownloadLeavesNoObject(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer server.Close()
	s := New(t.TempDir())
	a := Artifact{URL: server.URL, SHA256: sum("bytes")}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, _, err := s.Ensure(ctx, a); err == nil {
		t.Fatal("canceled download accepted")
	}
	select {
	case <-started:
	default:
		t.Fatal("request never began")
	}
	if _, ok, err := s.Has(a.SHA256); err != nil || ok {
		t.Fatalf("canceled object retained: %v %v", ok, err)
	}
}
