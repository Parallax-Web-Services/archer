package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cacheTTL = 300 * time.Second
	maxBody  = 2 * 1024 * 1024

	StateFresh       = "fresh"
	StateStale       = "stale"
	StateUnavailable = "unavailable"
)

type Service struct {
	ID        string   `json:"id"`
	Provides  []string `json:"provides"`
	Endpoint  string   `json:"endpoint"`
	HealthURL string   `json:"health_url"`
	TokenEnv  string   `json:"token_env,omitempty"`
	Source    string   `json:"source,omitempty"`
	Version   string   `json:"version,omitempty"`
}

type Snapshot struct {
	Services []Service `json:"services"`
	Stale    bool      `json:"stale"`
	State    string    `json:"state"`
	Source   string    `json:"source"`
	Error    string    `json:"error,omitempty"`
}

type cacheEntry struct {
	FetchedAt time.Time `json:"fetched_at"`
	Snapshot  Snapshot  `json:"snapshot"`
}

// Fetch discovers candidates, not verified or authorized service bindings.
// Failed refreshes may return a stale snapshot with Error populated and no error.
// Remote catalog data can never choose an environment variable for credentials.
func Fetch(ctx context.Context, source, cacheRoot string) (Snapshot, error) {
	unavailable := func(err error) (Snapshot, error) {
		return Snapshot{Services: []Service{}, Source: source, Stale: true, State: StateUnavailable, Error: err.Error()}, err
	}
	u, err := checkedURL(source)
	if err != nil {
		return unavailable(fmt.Errorf("catalog URL: %w", err))
	}
	token, team := catalogCredentials(u)
	if err := checkToken(token); err != nil {
		return unavailable(err)
	}
	if strings.ContainsAny(team, "\r\n") {
		return unavailable(errors.New("configured team contains invalid characters"))
	}
	cachePath := ""
	if cacheRoot != "" {
		// Credentials and team scope affect cache identity. Only a hash reaches
		// the filename; neither credential is persisted in the file contents.
		digest := sha256.Sum256([]byte(source + "\x00" + token + "\x00" + team))
		cachePath = filepath.Join(cacheRoot, "catalog", hex.EncodeToString(digest[:])+".json")
	}
	cached, cacheOK := readCache(cachePath, source)
	if cacheOK && !cached.Snapshot.Stale {
		age := time.Since(cached.FetchedAt)
		if age >= 0 && age < cacheTTL {
			return cached.Snapshot, nil
		}
	}
	fallback := func(reason error) (Snapshot, error) {
		if cacheOK {
			snapshot := cached.Snapshot
			snapshot.Stale = true
			snapshot.State = StateStale
			snapshot.Error = reason.Error()
			return snapshot, nil
		}
		return unavailable(reason)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fallback(errors.New("cannot construct catalog request"))
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if team != "" {
		req.Header.Set("X-Slipstream-Team", team)
	}
	resp, err := clientFor(u).Do(req)
	if err != nil {
		return fallback(transportError("catalog", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallback(fmt.Errorf("catalog returned HTTP %d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fallback(errors.New("cannot read catalog response"))
	}
	if len(data) > maxBody {
		return fallback(errors.New("catalog exceeds 2 MiB limit"))
	}
	snapshot, err := parse(data, source)
	if err != nil {
		return fallback(fmt.Errorf("invalid catalog: %w", err))
	}
	if cachePath != "" {
		if err := writeCache(cachePath, cacheEntry{FetchedAt: time.Now().UTC(), Snapshot: snapshot}); err != nil {
			snapshot.Error = "catalog fetched, but its cache could not be updated"
		}
	}
	return snapshot, nil
}

// Verify checks an explicit service health endpoint. TokenEnv is intended only
// for locally configured bindings; Fetch strips it from every remote descriptor.
func Verify(ctx context.Context, service Service) (string, error) {
	if service.Endpoint == "" || service.HealthURL == "" {
		return "", errors.New("service has no explicit endpoint and health URL; it remains a candidate")
	}
	endpoint, err := checkedURL(service.Endpoint)
	if err != nil {
		return "", fmt.Errorf("service endpoint: %w", err)
	}
	health, err := checkedURL(service.HealthURL)
	if err != nil {
		return "", fmt.Errorf("service health URL: %w", err)
	}
	if origin(endpoint) != origin(health) {
		return "", errors.New("service health URL must have the same origin as its endpoint")
	}
	token := ""
	if service.TokenEnv != "" {
		token = strings.TrimSpace(os.Getenv(service.TokenEnv))
		if token == "" {
			return "", errors.New("configured service credential is unavailable")
		}
		if err := checkToken(token); err != nil {
			return "", err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		return "", errors.New("cannot construct service health request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := clientFor(health).Do(req)
	if err != nil {
		return "", transportError("service health", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("service health returned HTTP %d", resp.StatusCode)
	}
	return fmt.Sprintf("service %s: explicit health endpoint returned HTTP 200 at %s", service.ID, time.Now().UTC().Format(time.RFC3339)), nil
}

func checkedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.Fragment != "" {
		return nil, errors.New("expected an absolute HTTP(S) URL without a fragment")
	}
	if u.User != nil {
		return nil, errors.New("credentials must be supplied through local environment configuration")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
		return nil, errors.New("HTTPS is required outside loopback hosts")
	}
	for key := range u.Query() {
		switch strings.ToLower(key) {
		case "token", "access_token", "api_key", "apikey", "password", "secret", "authorization":
			return nil, errors.New("credentials cannot appear in URL query parameters")
		}
	}
	return u, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func clientFor(initial *url.URL) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("redirect limit reached")
			}
			u, err := checkedURL(req.URL.String())
			if err != nil || origin(u) != origin(initial) {
				return errors.New("redirect to another origin is forbidden")
			}
			return nil
		},
	}
}

func catalogCredentials(u *url.URL) (string, string) {
	configured, err := checkedURL(os.Getenv("ARCHER_CATALOG_ORIGIN"))
	if err != nil || origin(configured) != origin(u) {
		return "", ""
	}
	return strings.TrimSpace(os.Getenv("ARCHER_CATALOG_TOKEN")), strings.TrimSpace(os.Getenv("SLIPSTREAM_TEAM"))
}

func checkToken(token string) error {
	if strings.ContainsAny(token, "\r\n") {
		return errors.New("configured credential contains invalid characters")
	}
	return nil
}

func transportError(action string, err error) error {
	// Do not echo request URLs, bodies, or headers into diagnostics.
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s request canceled", action)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s request timed out", action)
	}
	return fmt.Errorf("%s request failed (network or redirect policy)", action)
}

func readCache(path, source string) (cacheEntry, bool) {
	var entry cacheEntry
	if path == "" {
		return entry, false
	}
	f, err := os.Open(path)
	if err != nil {
		return entry, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBody+1))
	if err != nil || len(data) > maxBody || json.Unmarshal(data, &entry) != nil || entry.FetchedAt.IsZero() || entry.Snapshot.Source != source {
		return cacheEntry{}, false
	}
	for i := range entry.Snapshot.Services {
		entry.Snapshot.Services[i].TokenEnv = ""
	}
	// Upgrade caches written before the explicit state field was introduced.
	if entry.Snapshot.State == "" {
		entry.Snapshot.State = StateFresh
		if entry.Snapshot.Stale {
			entry.Snapshot.State = StateStale
		}
	}
	return entry, true
}

func writeCache(path string, entry cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type metadata struct {
	Endpoint  string `json:"endpoint"`
	HealthURL string `json:"health_url"`
	Version   string `json:"version"`
}

func parse(data []byte, source string) (Snapshot, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return Snapshot{}, errors.New("expected a JSON object")
	}
	snapshot := Snapshot{Services: []Service{}, Source: source, State: StateFresh}
	if raw, ok := root["stale"]; ok {
		if err := json.Unmarshal(raw, &snapshot.Stale); err != nil {
			return Snapshot{}, errors.New("stale must be boolean")
		}
	}
	if raw, ok := root["providers"]; ok {
		var providers map[string]struct {
			Stale     bool  `json:"stale"`
			Available *bool `json:"available"`
		}
		if err := json.Unmarshal(raw, &providers); err != nil {
			return Snapshot{}, errors.New("invalid provider status")
		}
		for _, provider := range providers {
			snapshot.Stale = snapshot.Stale || provider.Stale
			if provider.Available != nil && !*provider.Available {
				// Unavailable discovery cannot establish that no service exists.
				// Preserve candidates, but use the same conservative execution
				// gate as a failed refresh rather than licensing installation/AI.
				snapshot.Stale = true
				snapshot.State = StateUnavailable
				snapshot.Error = "catalog reports an unavailable provider"
			}
		}
	}
	if snapshot.Stale && snapshot.State != StateUnavailable {
		snapshot.State = StateStale
	}
	if raw, ok := root["schema"]; ok {
		var schema string
		if json.Unmarshal(raw, &schema) != nil || schema != "archer.catalog/v1" {
			return Snapshot{}, errors.New("unsupported catalog schema")
		}
		services, ok := root["services"]
		if !ok || string(services) == "null" || json.Unmarshal(services, &snapshot.Services) != nil {
			return Snapshot{}, errors.New("services must be an array")
		}
	} else if raw, ok := root["products"]; ok {
		var products []struct {
			Slug         string   `json:"slug"`
			Capabilities []string `json:"capabilities"`
			Endpoint     string   `json:"endpoint"`
			HealthURL    string   `json:"health_url"`
			Version      string   `json:"version"`
			Metadata     metadata `json:"metadata"`
		}
		if string(raw) == "null" || json.Unmarshal(raw, &products) != nil {
			return Snapshot{}, errors.New("products must be an array")
		}
		for _, product := range products {
			snapshot.Services = append(snapshot.Services, Service{ID: product.Slug, Provides: product.Capabilities, Endpoint: first(product.Endpoint, product.Metadata.Endpoint), HealthURL: first(product.HealthURL, product.Metadata.HealthURL), Version: first(product.Version, product.Metadata.Version)})
		}
	} else if raw, ok := root["capabilities"]; ok {
		var descriptors []struct {
			Provider   string   `json:"provider"`
			Capability string   `json:"capability"`
			Provides   []string `json:"provides"`
			Metadata   metadata `json:"metadata"`
		}
		if string(raw) == "null" || json.Unmarshal(raw, &descriptors) != nil {
			return Snapshot{}, errors.New("capabilities must be a descriptor array")
		}
		for _, descriptor := range descriptors {
			if descriptor.Provider == "" || descriptor.Capability == "" {
				return Snapshot{}, errors.New("descriptor needs provider and capability")
			}
			snapshot.Services = append(snapshot.Services, Service{ID: descriptor.Provider + ":" + descriptor.Capability, Provides: append(descriptor.Provides, descriptor.Capability), Endpoint: descriptor.Metadata.Endpoint, HealthURL: descriptor.Metadata.HealthURL, Version: descriptor.Metadata.Version})
		}
	} else {
		return Snapshot{}, errors.New("expected services, products, or capability descriptors")
	}
	seen := map[string]bool{}
	for i := range snapshot.Services {
		service := &snapshot.Services[i]
		if strings.TrimSpace(service.ID) == "" || len(service.Provides) == 0 {
			return Snapshot{}, errors.New("service needs id and provided capabilities")
		}
		if seen[service.ID] {
			return Snapshot{}, errors.New("duplicate service id")
		}
		seen[service.ID] = true
		service.TokenEnv = ""
		service.Source = source
		service.Provides = unique(service.Provides)
		for _, capability := range service.Provides {
			if strings.TrimSpace(capability) == "" {
				return Snapshot{}, errors.New("empty provided capability")
			}
		}
		for _, endpoint := range []string{service.Endpoint, service.HealthURL} {
			if endpoint != "" {
				if _, err := checkedURL(endpoint); err != nil {
					return Snapshot{}, fmt.Errorf("unsafe service URL: %w", err)
				}
			}
		}
	}
	sort.Slice(snapshot.Services, func(i, j int) bool { return snapshot.Services[i].ID < snapshot.Services[j].ID })
	return snapshot, nil
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func unique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
