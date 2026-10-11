package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSupportedFormats(t *testing.T) {
	tests := []struct {
		name, body, id string
		provides       []string
		stale          bool
	}{
		{"native", `{"schema":"archer.catalog/v1","services":[{"id":"relay","provides":["social-publishing"],"endpoint":"https://relay.example/","health_url":"https://relay.example/health","token_env":"PRIVATE_ENV","token":"never-cache"}]}`, "relay", []string{"social-publishing"}, false},
		{"products", `{"products":[{"slug":"relay","name":"RELAY","capabilities":["social-publishing"],"actions":["publish"],"metadata":{"endpoint":"https://relay.example/","health_url":"https://relay.example/health"}}]}`, "relay", []string{"social-publishing"}, false},
		{"descriptors", `{"capabilities":[{"provider":"relay","capability":"social-publishing","provides":["publish","social-publishing"],"metadata":{"endpoint":"https://relay.example/","health_url":"https://relay.example/health","token_env":"PRIVATE_ENV"}}],"providers":{"parallax":{"available":true,"stale":true}}}`, "relay:social-publishing", []string{"publish", "social-publishing"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parse([]byte(tt.body), "https://catalog.example/index")
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Services) != 1 || s.Services[0].ID != tt.id || !reflect.DeepEqual(s.Services[0].Provides, tt.provides) || s.Stale != tt.stale {
				t.Fatalf("unexpected snapshot: %#v", s)
			}
			if s.Services[0].TokenEnv != "" || s.Services[0].Source != s.Source {
				t.Fatal("remote credentials or provenance were retained")
			}
		})
	}
}

func TestEndpointlessDescriptorsStayCandidates(t *testing.T) {
	for _, body := range []string{
		`{"products":[{"slug":"relay","capabilities":["social-publishing"]}]}`,
		`{"capabilities":[{"provider":"relay","capability":"social-publishing","provides":[]}]}`,
	} {
		snapshot, err := parse([]byte(body), "https://catalog.example/index")
		if err != nil || len(snapshot.Services) != 1 {
			t.Fatalf("candidate was lost: %v", err)
		}
		if _, err := Verify(context.Background(), snapshot.Services[0]); err == nil {
			t.Fatal("advertisement was falsely verified")
		}
	}
}

func TestUnavailableProviderCannotAuthorizeFallback(t *testing.T) {
	snapshot, err := parse([]byte(`{"capabilities":[],"providers":{"parallax":{"available":false,"stale":false}}}`), "https://catalog.example")
	if err != nil || !snapshot.Stale || snapshot.Error == "" || snapshot.State != StateUnavailable {
		t.Fatalf("unavailable discovery was represented as complete: %#v %v", snapshot, err)
	}
}

func TestInvalidCatalogsFailClosed(t *testing.T) {
	for _, body := range []string{
		`[]`, `null`, `{}`, `{"schema":"archer.catalog/v2","services":[]}`,
		`{"schema":"archer.catalog/v1","services":null}`,
		`{"products":[{"slug":"relay","capabilities":[]}]}`,
		`{"capabilities":[{"capability":"x"}]}`,
		`{"schema":"archer.catalog/v1","services":[{"id":"x","provides":["x"],"endpoint":"http://external.example/"}]}`,
		`{"schema":"archer.catalog/v1","services":[{"id":"x","provides":["x"]},{"id":"x","provides":["y"]}]}`,
	} {
		if _, err := parse([]byte(body), "https://catalog.example"); err == nil {
			t.Fatalf("accepted invalid input %s", body)
		}
	}
}

func TestFetchCacheFreshnessAndStaleFallback(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "offline", 503)
			return
		}
		w.Write([]byte(`{"schema":"archer.catalog/v1","services":[{"id":"relay","provides":["publish"]}]}`))
	}))
	defer server.Close()
	cache := t.TempDir()
	first, err := Fetch(context.Background(), server.URL, cache)
	if err != nil || first.Stale || first.State != StateFresh || len(first.Services) != 1 {
		t.Fatalf("initial fetch: %#v %v", first, err)
	}
	failing.Store(true)
	fresh, err := Fetch(context.Background(), server.URL, cache)
	if err != nil || fresh.Stale || fresh.State != StateFresh || calls.Load() != 1 {
		t.Fatal("fresh cache did not avoid the network")
	}
	expireCache(t, cache)
	stale, err := Fetch(context.Background(), server.URL, cache)
	if err != nil || !stale.Stale || stale.State != StateStale || stale.Error == "" || len(stale.Services) != 1 || calls.Load() != 2 {
		t.Fatalf("stale fallback: %#v %v", stale, err)
	}
	if unavailable, err := Fetch(context.Background(), server.URL, t.TempDir()); err == nil || unavailable.State != StateUnavailable || unavailable.Source != server.URL || !unavailable.Stale {
		t.Fatal("fetch failure without cache must be an error")
	}
}

func TestFreshEmptyCatalogIsDistinctFromUnavailableDiscovery(t *testing.T) {
	for _, test := range []struct {
		body, wantState string
		stale           bool
	}{
		{`{"schema":"archer.catalog/v1","services":[]}`, StateFresh, false},
		{`{"schema":"archer.catalog/v1","services":[],"stale":true}`, StateStale, true},
		{`{"capabilities":[],"providers":{"parallax":{"available":false}}}`, StateUnavailable, true},
	} {
		snapshot, err := parse([]byte(test.body), "https://catalog.example")
		if err != nil || snapshot.State != test.wantState || snapshot.Stale != test.stale || len(snapshot.Services) != 0 {
			t.Fatalf("empty catalog lost its discovery meaning: %+v %v", snapshot, err)
		}
	}
}

func TestLegacySnapshotCacheReceivesExplicitState(t *testing.T) {
	for _, stale := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "legacy.json")
		entry := cacheEntry{FetchedAt: time.Now(), Snapshot: Snapshot{Services: []Service{}, Source: "https://catalog.example", Stale: stale}}
		if err := writeCache(path, entry); err != nil {
			t.Fatal(err)
		}
		cached, ok := readCache(path, entry.Snapshot.Source)
		want := StateFresh
		if stale {
			want = StateStale
		}
		if !ok || cached.Snapshot.State != want || cached.Snapshot.Stale != stale {
			t.Fatalf("legacy cache was not upgraded safely: %+v", cached)
		}
	}
}

func expireCache(t *testing.T, root string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "catalog", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatal("cache file missing")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var entry cacheEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			t.Fatal(err)
		}
		entry.FetchedAt = time.Now().Add(-cacheTTL - time.Second)
		if err := writeCache(path, entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogCredentialsAreOriginScopedAndNeverCached(t *testing.T) {
	t.Setenv("ARCHER_CATALOG_TOKEN", "very-private-token")
	t.Setenv("SLIPSTREAM_TEAM", "team-42")
	var observed atomic.Value
	observed.Store([2]string{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.Store([2]string{r.Header.Get("Authorization"), r.Header.Get("X-Slipstream-Team")})
		w.Write([]byte(`{"schema":"archer.catalog/v1","services":[{"id":"relay","provides":["publish"],"token_env":"AWS_SECRET_ACCESS_KEY","token":"response-secret"}]}`))
	}))
	defer server.Close()
	t.Setenv("ARCHER_CATALOG_ORIGIN", server.URL)
	cache := t.TempDir()
	snapshot, err := Fetch(context.Background(), server.URL+"/index", cache)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Load().([2]string) != [2]string{"Bearer very-private-token", "team-42"} || snapshot.Services[0].TokenEnv != "" {
		t.Fatal("credential scoping failed")
	}
	paths, _ := filepath.Glob(filepath.Join(cache, "catalog", "*.json"))
	if len(paths) != 1 {
		t.Fatal("cache missing")
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"very-private-token", "response-secret", "AWS_SECRET_ACCESS_KEY", "team-42"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("cache contains %s", secret)
		}
	}
	t.Setenv("ARCHER_CATALOG_ORIGIN", "https://another-origin.example")
	if _, err := Fetch(context.Background(), server.URL+"/index", cache); err != nil {
		t.Fatal(err)
	}
	if observed.Load().([2]string) != [2]string{} {
		t.Fatal("credentials crossed configured origin")
	}
	t.Setenv("ARCHER_CATALOG_ORIGIN", server.URL)
	t.Setenv("SLIPSTREAM_TEAM", "different-team")
	if _, err := Fetch(context.Background(), server.URL+"/index", cache); err != nil {
		t.Fatal(err)
	}
	if observed.Load().([2]string)[1] != "different-team" {
		t.Fatal("team change incorrectly reused old scoped cache")
	}
}

func TestRedirectOriginBoundary(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		w.Write([]byte(`{"schema":"archer.catalog/v1","services":[]}`))
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/ok", 302)
			return
		}
		if r.URL.Path == "/ok" {
			w.Write([]byte(`{"schema":"archer.catalog/v1","services":[]}`))
			return
		}
		http.Redirect(w, r, destination.URL, 302)
	}))
	defer server.Close()
	if _, err := Fetch(context.Background(), server.URL+"/cross", ""); err == nil {
		t.Fatal("cross-origin catalog redirect accepted")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("cross-origin endpoint was contacted")
	}
	if _, err := Fetch(context.Background(), server.URL+"/same", ""); err != nil {
		t.Fatalf("same-origin redirect refused: %v", err)
	}
	if _, err := Verify(context.Background(), Service{ID: "relay", Endpoint: server.URL, HealthURL: server.URL + "/cross"}); err == nil {
		t.Fatal("cross-origin health redirect accepted")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("health credential boundary failed")
	}
}

func TestVerifyRequiresHealthyExplicitSameOriginBinding(t *testing.T) {
	t.Setenv("ARCHER_TEST_SERVICE_TOKEN", "private-service-token")
	var observed atomic.Value
	observed.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/down" {
			http.Error(w, "unavailable", 503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	service := Service{ID: "relay", Endpoint: server.URL, HealthURL: server.URL + "/health", TokenEnv: "ARCHER_TEST_SERVICE_TOKEN"}
	evidence, err := Verify(context.Background(), service)
	if err != nil || !strings.Contains(evidence, "HTTP 200") || observed.Load().(string) != "Bearer private-service-token" {
		t.Fatalf("health verification: %s %v", evidence, err)
	}
	if strings.Contains(evidence, "private-service-token") {
		t.Fatal("evidence exposes credentials")
	}
	service.HealthURL = server.URL + "/down"
	if _, err := Verify(context.Background(), service); err == nil {
		t.Fatal("unhealthy service accepted")
	}
	service.HealthURL = "https://another-origin.example/health"
	if _, err := Verify(context.Background(), service); err == nil {
		t.Fatal("cross-origin health configuration accepted")
	}
	service.HealthURL = server.URL + "/health"
	service.TokenEnv = "ARCHER_NONEXISTENT_TEST_TOKEN"
	t.Setenv(service.TokenEnv, "")
	if _, err := Verify(context.Background(), service); err == nil {
		t.Fatal("missing local credential accepted")
	}
}

func TestURLPolicyAndBoundedResponses(t *testing.T) {
	for _, raw := range []string{"http://remote.example/index", "file:///tmp/index", "https://user:password@example.test/index", "https://example.test/index?access_token=secret", "https://example.test/index#fragment"} {
		if _, err := checkedURL(raw); err == nil {
			t.Fatalf("unsafe URL accepted %s", raw)
		}
	}
	for _, raw := range []string{"https://example.test/index", "http://127.0.0.1:8000/index", "http://[::1]:8000/index", "http://localhost:8000/index"} {
		if _, err := checkedURL(raw); err != nil {
			t.Fatalf("safe URL rejected %s: %v", raw, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat(" ", maxBody+1))) }))
	defer server.Close()
	if _, err := Fetch(context.Background(), server.URL, ""); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatalf("response limit not enforced: %v", err)
	}
}
