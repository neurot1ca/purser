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

const catalogFixture = `{"openai":{"name":"ignored provider metadata","models":{"sample-model":{"name":"Sample Model","family":"sample-family","reasoning":true,"cost":{"input":0,"output":0,"cache_read":99},"limit":{"context":12000,"output":2000},"unknown":"ignored"},"bad-model":{"name":"Bad Model","reasoning":false,"limit":{"context":"unknown"}}}}}`

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
	writeCatalogCache(t, root, now.Add(-time.Minute))
	if err := RefreshCatalog(context.Background(), root, now); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("fresh cache caused %d requests", requests)
	}
}

func TestRefreshCatalogStaleCacheWritesCheckedEnvelope(t *testing.T) {
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
	if providers["openai"]["sample-model"] == nil {
		t.Fatalf("written cache was not readable: providers=%v", providers)
	}
	fetchedAt, dropped, err := readCatalogCache(path)
	if err != nil || !fetchedAt.Equal(now) {
		t.Fatalf("written cache timestamp was not readable: fetched_at=%v err=%v", fetchedAt, err)
	}
	if dropped != 1 {
		t.Fatalf("dropped_count = %d, want 1", dropped)
	}
	var normalized map[string]struct {
		Models map[string]map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(envelope["providers"], &normalized); err != nil {
		t.Fatal(err)
	}
	model := normalized["openai"].Models["sample-model"]
	if _, ok := model["limits"]; !ok {
		t.Fatalf("API limit was not normalized to limits: %v", model)
	}
	if _, ok := model["limit"]; ok {
		t.Fatalf("API limit key was retained: %v", model)
	}
	var got map[string]any
	encoded, err := json.Marshal(normalized["openai"].Models["sample-model"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": "sample-model", "name": "Sample Model", "family": "sample-family", "reasoning": true,
		"cost":   map[string]any{"input": float64(0), "output": float64(0)},
		"limits": map[string]any{"context": float64(12000), "output": float64(2000)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized cached model = %#v, want %#v", got, want)
	}
}

func TestRefreshCatalogTimeoutPreservesOldCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	setCatalogURL(t, server.URL)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	before := writeCatalogCache(t, root, now.Add(-25*time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := RefreshCatalog(ctx, root, now); err == nil {
		t.Fatal("RefreshCatalog() succeeded after request timeout")
	}
	got, err := os.ReadFile(filepath.Join(root, "model-catalog", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatal("timed out refresh changed the old cache")
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

func TestNormalizeCatalogKeepsValidFieldsAndIgnoresHugeCosts(t *testing.T) {
	payload := []byte(`{"openai":{"name":"ignored provider metadata","models":{"sample-model":{"name":"Sample Model","family":"sample-family","reasoning":true,"cost":{"input":0,"output":0,"cache_read":99},"limit":{"context":12000,"output":2000},"unknown":"ignored"},"bad-model":{"name":"Bad Model","reasoning":false,"limit":{"context":"unknown"}},"huge-cost":{"name":"Huge Cost","reasoning":false,"cost":{"input":` + strings.Repeat("9", 401) + `,"output":1}}}}}`)
	providers, dropped, err := normalizeCatalog(payload)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	var got map[string]any
	encoded, err := json.Marshal(providers["openai"]["sample-model"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": "sample-model", "name": "Sample Model", "family": "sample-family", "reasoning": true,
		"cost":   map[string]any{"input": float64(0), "output": float64(0)},
		"limits": map[string]any{"context": float64(12000), "output": float64(2000)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized model = %#v, want %#v", got, want)
	}
	var huge map[string]any
	encoded, err = json.Marshal(providers["openai"]["huge-cost"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &huge); err != nil {
		t.Fatal(err)
	}
	if _, exists := huge["cost"]; exists {
		t.Fatalf("huge cost was retained: %#v", huge["cost"])
	}
}
