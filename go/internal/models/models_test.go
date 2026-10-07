package models

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func setupDiscovery(t *testing.T, config map[string]any, listing func(string, map[string]map[string]json.RawMessage) listingResult) string {
	return setupDiscoveryWithRouteOrder(t, config, routeNames(config), listing)
}

func setupDiscoveryWithRouteOrder(t *testing.T, config map[string]any, routeOrder []string, listing func(string, map[string]map[string]json.RawMessage) listingResult) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	oldConfig, oldCatalog, oldListing := loadConfig, loadCatalog, listModels
	loadConfig = func() (loadedConfig, error) { return loadedConfig{Values: config, RouteOrder: routeOrder}, nil }
	loadCatalog = func() (catalog, error) { return catalog{Providers: map[string]map[string]json.RawMessage{}}, nil }
	listModels = listing
	t.Cleanup(func() { loadConfig, loadCatalog, listModels = oldConfig, oldCatalog, oldListing })
	return state
}

func routeNames(config map[string]any) []string {
	routes, _ := config["routes"].(map[string]any)
	order := make([]string, 0, len(routes))
	for name := range routes {
		order = append(order, name)
	}
	return order
}

func writeCheckedState(t *testing.T, root, date string, asked, declined []string) {
	t.Helper()
	if err := writeNoticeState(filepath.Join(root, "model-version-notice.json"), noticeState{
		CheckedOn: &date, Asked: asked, Declined: declined,
	}); err != nil {
		t.Fatal(err)
	}
}

func readStateJSON(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "model-version-notice.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDiscoverModelVersionNotDueDoesNotList(t *testing.T) {
	today := nowDate()
	root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		t.Fatal("listing called when daily check is already recorded")
		return listingResult{}
	})
	writeCheckedState(t, root, today, nil, nil)
	if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatal("unexpected discovery")
	}
}

func TestDiscoverNewerWritesPendingAndDailyCheck(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"coder_agent": "codex", "routes": map[string]any{
		"normal": map[string]any{"model": "gpt-6-luna"}, "mechanical": "gpt-6-luna",
	}}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent != "codex" {
			t.Fatalf("agent = %q", agent)
		}
		return listingResult{available: true, ids: []string{"gpt-6.1-luna"}}
	})
	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gpt-6-luna" || newer != "gpt-6.1-luna" {
		t.Fatalf("got %q %q %v", current, newer, ok)
	}
	state := readStateJSON(t, root)
	want := map[string]any{"checked_on": nowDate(), "asked": []any{}, "declined": []any{},
		"pending": map[string]any{"current": "gpt-6-luna", "new": "gpt-6.1-luna", "blocked": false, "delivered": false}}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("state mismatch\n got: %#v\nwant: %#v", state, want)
	}
}

func TestDiscoverChecksRoutesInConfigOrder(t *testing.T) {
	config := map[string]any{"coder_agent": "codex", "routes": map[string]any{
		"zeta": "gpt-6-luna", "middle": "gpt-6-sol", "alpha": "gpt-6-astra",
	}}
	routeOrder, err := routeKeyOrder([]byte(`{"routes":{"zeta":"gpt-6-luna","alpha":"gpt-6-astra","middle":"gpt-6-sol"}}`))
	if err != nil {
		t.Fatal(err)
	}
	root := setupDiscoveryWithRouteOrder(t, config, routeOrder, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent != "codex" {
			t.Fatalf("agent = %q", agent)
		}
		return listingResult{available: true, ids: []string{"gpt-6.1-luna", "gpt-6.1-sol", "gpt-6.1-astra"}}
	})

	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gpt-6-luna" || newer != "gpt-6.1-luna" {
		t.Fatalf("first route checked = %q -> %q, available = %v; want zeta route gpt-6-luna", current, newer, ok)
	}
	if got := readStateJSON(t, root)["pending"].(map[string]any)["current"]; got != current {
		t.Fatalf("pending current = %v, want %q", got, current)
	}
}

func TestDiscoverChecksOverlayOnlyRoute(t *testing.T) {
	config := map[string]any{"coder_agent": "codex", "routes": map[string]any{
		"normal": "gpt-6-luna", "overlay": "gpt-6-sol",
	}}
	root := setupDiscoveryWithRouteOrder(t, config, []string{"normal"}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent != "codex" {
			t.Fatalf("agent = %q", agent)
		}
		return listingResult{available: true, ids: []string{"gpt-6-luna", "gpt-6.1-sol"}}
	})

	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gpt-6-sol" || newer != "gpt-6.1-sol" {
		t.Fatalf("got %q -> %q, available = %v; want overlay route", current, newer, ok)
	}
	if got := readStateJSON(t, root)["pending"].(map[string]any)["current"]; got != current {
		t.Fatalf("pending current = %v, want %q", got, current)
	}
}

func TestDiscoverAskedAndDeclinedVersionsAreSuppressed(t *testing.T) {
	for _, decision := range []string{"asked", "declined"} {
		t.Run(decision, func(t *testing.T) {
			root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
				return listingResult{available: true, ids: []string{"gpt-6.1-luna"}}
			})
			asked, declined := []string{}, []string{}
			if decision == "asked" {
				asked = []string{"gpt-6.1-luna"}
			} else {
				declined = []string{"gpt-6.1-luna"}
			}
			writeCheckedState(t, root, "2000-01-01", asked, declined)
			if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
				t.Fatal("suppressed version was offered")
			}
			state := readStateJSON(t, root)
			if state["pending"] != nil || state["checked_on"] != nowDate() {
				t.Fatalf("unexpected state: %#v", state)
			}
		})
	}
}

func TestDiscoverUnavailableListingRecordsCheckWithoutPending(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		return listingResult{}
	})
	if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatal("unavailable listing discovered a model")
	}
	state := readStateJSON(t, root)
	if state["pending"] != nil || state["checked_on"] != nowDate() {
		t.Fatalf("unexpected state: %#v", state)
	}
}

func writeCodexCache(t *testing.T, raw string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCachedCodexVersionQueuesVisibleUpgrade(t *testing.T) {
	config := map[string]any{"routes": map[string]any{
		"normal": "gpt-6-luna", "mechanical": "gpt-6-luna", "subtle": "gpt-6-sol",
	}}
	root := setupDiscoveryWithRouteOrder(t, config, []string{"normal", "mechanical", "subtle"}, nil)
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-sol","visibility":"list"},{"slug":"gpt-7-luna","visibility":"hide"}]}`)
	current, newer, ok := DiscoverCachedCodexVersion()
	if !ok || current != "gpt-6-sol" || newer != "gpt-6.1-sol" {
		t.Fatalf("got %q -> %q, available = %v", current, newer, ok)
	}
	state := readStateJSON(t, root)
	if got := state["pending"].(map[string]any); got["current"] != current || got["new"] != newer {
		t.Fatalf("pending = %#v", got)
	}
}

func TestDiscoverCachedCodexVersionRespectsNoticeState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		asked    []string
		declined []string
		pending  *noticePending
	}{
		{name: "asked", asked: []string{"gpt-6.1-luna"}},
		{name: "declined", declined: []string{"gpt-6.1-luna"}},
		{name: "pending", pending: &noticePending{Current: "gpt-6-sol", New: "gpt-6.1-sol", Blocked: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, nil)
			writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-luna","visibility":"list"}]}`)
			if err := writeNoticeState(filepath.Join(root, "model-version-notice.json"), noticeState{
				Asked: tc.asked, Declined: tc.declined, Pending: tc.pending,
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, ok := DiscoverCachedCodexVersion(); ok {
				t.Fatal("unexpected model notice")
			}
			state := readNoticeState(filepath.Join(root, "model-version-notice.json"))
			if !reflect.DeepEqual(state.Pending, tc.pending) {
				t.Fatalf("pending changed: got %#v want %#v", state.Pending, tc.pending)
			}
		})
	}
}

func TestDiscoverCachedCodexVersionIgnoresUnavailableOrHiddenCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache *string
	}{
		{name: "missing"},
		{name: "malformed", cache: stringPointer("{")},
		{name: "hidden only", cache: stringPointer(`{"models":[{"slug":"gpt-6.1-luna","visibility":"hide"}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, nil)
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			if tc.cache != nil {
				if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(*tc.cache), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, ok := DiscoverCachedCodexVersion(); ok {
				t.Fatal("unexpected model notice")
			}
			if _, err := os.Stat(filepath.Join(root, "model-version-notice.json")); !os.IsNotExist(err) {
				t.Fatalf("notice state changed: %v", err)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func TestDiscoverChecksAgyReviewerAfterCoderModels(t *testing.T) {
	calls := []string{}
	root := setupDiscovery(t, map[string]any{
		"coder_agent": "codex", "routes": map[string]any{"normal": "gpt-6-luna"},
		"reviewer1_model_requested": "gemini-3.8-flash-high",
	}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		calls = append(calls, agent)
		if agent == "codex" {
			return listingResult{available: true, ids: []string{"gpt-6-luna"}}
		}
		return listingResult{available: true, ids: []string{"gemini-3.9-flash-high"}}
	})
	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gemini-3.8-flash-high" || newer != "gemini-3.9-flash-high" {
		t.Fatalf("got %q %q %v", current, newer, ok)
	}
	if !reflect.DeepEqual(calls, []string{"codex", "agy"}) {
		t.Fatalf("listing order = %v", calls)
	}
	if got := readStateJSON(t, root)["pending"].(map[string]any)["current"]; got != current {
		t.Fatalf("pending current = %v", got)
	}
}

func TestNewerVersionComparison(t *testing.T) {
	cases := []struct {
		current    string
		candidates []string
		want       string
	}{
		{"gpt-6-luna", []string{"gpt-6.1-luna", "gpt-5-luna", "gpt-6.1-sol"}, "gpt-6.1-luna"},
		{"gemini-3.8-flash-high", []string{"gemini-3.9-pro-high", "gemini-3.9-flash-medium", "gemini-3.9-flash-high"}, "gemini-3.9-flash-high"},
		{"gemini-3.8-flash-high", []string{"gemini-3.9-pro-high", "gemini-3.9-flash-medium"}, ""},
		{"gpt-luna", []string{"gpt-7-luna"}, ""},
	}
	for _, test := range cases {
		if got := newerVersion(test.current, test.candidates); got != test.want {
			t.Errorf("newerVersion(%q) = %q, want %q", test.current, got, test.want)
		}
	}
}

func TestParseMockListings(t *testing.T) {
	cases := []struct {
		agent, output string
		want          []string
	}{
		{"codex", `{"models":[{"slug":"gpt-6.1-luna","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]},{"slug":"hidden","visibility":"hide","supported_reasoning_levels":[]}]}`, []string{"gpt-6.1-luna"}},
		{"opencode", "openai/gpt-6.1-luna\ninvalid\n", []string{"openai/gpt-6.1-luna"}},
		{"agy", "gemini-3.9-flash-high\tFlash\ngemini-3.9-flash-low\tFlash\n", []string{"gemini-3.9-flash"}},
	}
	for _, test := range cases {
		got, err := parseListing(test.agent, test.output)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseListing(%s) = %v, %v", test.agent, got, err)
		}
	}
}

func TestListingUsesExpectedMockExecutableAndTwoSecondDeadline(t *testing.T) {
	oldLookPath, oldRun := lookPath, runListingCommand
	t.Cleanup(func() { lookPath, runListingCommand = oldLookPath, oldRun })
	lookPath = func(name string) (string, error) {
		if name != "codex" {
			t.Fatalf("lookup name = %q", name)
		}
		return filepath.Join(t.TempDir(), "mock-codex"), nil
	}
	runListingCommand = func(command *exec.Cmd) ([]byte, error) {
		if !reflect.DeepEqual(command.Args[1:], []string{"debug", "models"}) {
			t.Fatalf("args = %v", command.Args)
		}
		if command.Cancel == nil {
			t.Fatal("listing command has no timeout cancellation")
		}
		return []byte(`{"models":[{"slug":"gpt-6.1-luna","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]}]}`), nil
	}
	result := listModels("codex", nil)
	if !result.available || !reflect.DeepEqual(result.ids, []string{"gpt-6.1-luna"}) {
		t.Fatalf("listing = %#v", result)
	}
}

func TestCatalogDropsInvalidModelsAndRequiresOneValidModel(t *testing.T) {
	data := []byte(`{"p":{"models":{"valid":{"id":"valid","name":"Valid","reasoning":true,"limit":{"context":10}},"bad":{"id":"bad","name":"","reasoning":true},"wrong-limit":{"id":"wrong-limit","name":"Bad limit","reasoning":false,"limit":{"output":0}}}}}`)
	providers, err := validateProviders(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(providers["p"]); got != 1 {
		t.Fatalf("valid model count = %d", got)
	}
	if _, err := validateProviders([]byte(`{"p":{"models":{"bad":{"id":"bad","name":"","reasoning":true}}}}`)); err == nil {
		t.Fatal("empty valid catalog was accepted")
	}
}

func nowDate() string { return time.Now().UTC().Format("2006-01-02") }
