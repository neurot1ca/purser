package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const catalogFixture = `{"provider":{"models":{"model-a":{"name":"Model A","reasoning":true,"cost":{"input":0.1,"output":0.2},"limit":{"context":1000,"output":100}}}}}`

func setCatalogURL(t *testing.T, value string) {
	t.Helper()
	previous := modelsDevURL
	modelsDevURL = value
	t.Cleanup(func() { modelsDevURL = previous })
}

func writeCatalogCache(t *testing.T, root string, fetchedAt time.Time) []byte {
	t.Helper()
	path := filepath.Join(root, "model-catalog", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf(`{"fetched_at":%q,"dropped_count":0,"providers":{"provider":{"models":{"model-a":{"id":"model-a","name":"Model A","reasoning":true,"limits":{"context":1000,"output":100}}}}}}`, fetchedAt.UTC().Format(time.RFC3339Nano)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRefreshCatalogFreshCacheSkipsRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	setCatalogURL(t, server.URL)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	writeCatalogCache(t, root, now.Add(-23*time.Hour))
	if err := RefreshCatalog(context.Background(), root, now); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("fresh cache caused %d requests", requests)
	}
}

func TestRefreshCatalogStaleCacheWritesPythonCompatibleEnvelope(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("User-Agent"); got != "purser" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	setCatalogURL(t, server.URL)
	now := time.Date(2026, 10, 6, 12, 0, 0, 123000, time.UTC)
	root := t.TempDir()
	writeCatalogCache(t, root, now.Add(-25*time.Hour))
	if err := RefreshCatalog(context.Background(), root, now); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	path := filepath.Join(root, "model-catalog", "catalog.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, 0, len(envelope))
	for key := range envelope {
		gotKeys = append(gotKeys, key)
	}
	wantKeys := []string{"dropped_count", "fetched_at", "providers"}
	// The Python _atomic_write envelope has exactly these three keys.
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("cache envelope keys = %v, want %v", gotKeys, wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("cache envelope missing %q: %v", key, gotKeys)
		}
	}
	providers, err := readCachedProviders(path)
	if err != nil {
		t.Fatalf("Go cache reader rejected written cache: %v", err)
	}
	if providers["provider"]["model-a"] == nil {
		t.Fatalf("written cache was not readable: providers=%v", providers)
	}
	fetchedAt, _, err := readCatalogCache(path)
	if err != nil || !fetchedAt.Equal(now) {
		t.Fatalf("written cache timestamp was not readable: fetched_at=%v err=%v", fetchedAt, err)
	}
	var normalized map[string]struct {
		Models map[string]map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(envelope["providers"], &normalized); err != nil {
		t.Fatal(err)
	}
	model := normalized["provider"].Models["model-a"]
	if _, ok := model["limits"]; !ok {
		t.Fatalf("API limit was not normalized to limits: %v", model)
	}
	if _, ok := model["limit"]; ok {
		t.Fatalf("API limit key was retained: %v", model)
	}
}

func TestRefreshCatalogFailurePreservesOldCache(t *testing.T) {
	for _, test := range []struct {
		name string
		body func(http.ResponseWriter)
	}{
		{name: "oversized", body: func(w http.ResponseWriter) { _, _ = w.Write([]byte(strings.Repeat("x", maxCatalogBytes+1))) }},
		{name: "invalid", body: func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"provider":{"models":{"broken":{"name":"No reasoning"}}}}`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { test.body(w) }))
			defer server.Close()
			setCatalogURL(t, server.URL)
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			root := t.TempDir()
			before := writeCatalogCache(t, root, now.Add(-25*time.Hour))
			if err := RefreshCatalog(context.Background(), root, now); err == nil {
				t.Fatal("RefreshCatalog() succeeded for invalid response")
			}
			got, err := os.ReadFile(filepath.Join(root, "model-catalog", "catalog.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, before) {
				t.Fatal("failed refresh changed the old cache")
			}
		})
	}
}
