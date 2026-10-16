package models

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

var lastConfigRepository string

func setupDiscovery(t *testing.T, config map[string]any, listing func(string, map[string]map[string]json.RawMessage) listingResult) string {
	return setupDiscoveryWithRouteOrder(t, config, routeNames(config), listing)
}

func setupDiscoveryWithRouteOrder(t *testing.T, config map[string]any, routeOrder []string, listing func(string, map[string]map[string]json.RawMessage) listingResult) string {
	t.Helper()
	if config != nil && len(routeOrder) > 0 && !configUsesOpenCode(config) {
		configured := make(map[string]any, len(config)+1)
		for key, value := range config {
			configured[key] = value
		}
		configured["mechanical_opencode_enabled"] = false
		config = configured
	}
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	oldConfig, oldCatalog, oldListing := loadConfig, loadCatalog, listModels
	loadConfig = func(repository string) (loadedConfig, error) {
		lastConfigRepository = repository
		return loadedConfig{Values: config, RouteOrder: routeOrder}, nil
	}
	loadCatalog = func() (catalog, error) { return catalog{Providers: map[string]map[string]json.RawMessage{}}, nil }
	listModels = listing
	t.Cleanup(func() { loadConfig, loadCatalog, listModels = oldConfig, oldCatalog, oldListing })
	return state
}

func configUsesOpenCode(config map[string]any) bool {
	if agent, _ := config["coder_agent"].(string); agent == "opencode" {
		return true
	}
	if agent, _ := config["reviewer1_agent"].(string); agent == "opencode" {
		return true
	}
	if fallback, _ := config["fallback"].(map[string]any); fallback != nil {
		if agent, _ := fallback["agent"].(string); agent == "opencode" {
			return true
		}
	}
	return false
}

func routeNames(config map[string]any) []string {
	routes, _ := config["routes"].(map[string]any)
	order := make([]string, 0, len(routes))
	for name := range routes {
		order = append(order, name)
	}
	return order
}

func writeCheckedState(t *testing.T, root, date string, report *noticeReport) {
	t.Helper()
	if err := writeNoticeState(filepath.Join(root, "model-version-notice.json"), noticeState{
		CheckedOn: &date, Report: report,
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
	writeCheckedState(t, root, today, nil)
	if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatal("unexpected discovery")
	}
}

func TestResolveLatestCachesWithoutProcess(t *testing.T) {
	root := setupDiscovery(t, nil, func(string, map[string]map[string]json.RawMessage) listingResult {
		t.Fatal("dispatch launched a listing process")
		return listingResult{}
	})
	t.Setenv("CODEX_HOME", root)
	oldRun := runListingCommand
	runListingCommand = func(*exec.Cmd) ([]byte, error) { t.Fatal("dispatch launched a process"); return nil, nil }
	t.Cleanup(func() { runListingCommand = oldRun })
	if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6.2-sol","visibility":"list"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLatest("codex", "gpt-6.1-sol"); got != "gpt-6.2-sol" {
		t.Fatalf("resolved %q", got)
	}
	cache := resolutionCache{CheckedOn: nowDate(), Agents: map[string][]string{"agy": {"gemini-3.9-flash-high"}, "opencode": {"google/gemini-3.9-flash-high"}}}
	if err := atomicJSON(filepath.Join(root, "model-resolution.json"), cache); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"agy", "opencode"} {
		prefix := ""
		if agent == "opencode" {
			prefix = "google/"
		}
		if got := ResolveLatest(agent, prefix+"gemini-3.8-flash-high"); got != prefix+"gemini-3.9-flash-high" {
			t.Fatalf("%s resolved %q", agent, got)
		}
	}
	if got := ResolveLatest("claude", "opus"); got != "opus" {
		t.Fatalf("Claude alias resolved %q", got)
	}
}

func TestResolveLatestUnavailableCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("PURSER_STATE_DIR", root)
	for _, content := range []string{"", "{broken"} {
		if content != "" {
			for _, name := range []string{"models_cache.json", "model-resolution.json"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, agent := range []string{"codex", "agy", "opencode"} {
			if got := ResolveLatest(agent, "gpt-6.1-sol"); got != "gpt-6.1-sol" {
				t.Fatalf("%s resolved %q", agent, got)
			}
		}
	}
	if err := atomicJSON(filepath.Join(root, "model-resolution.json"), resolutionCache{CheckedOn: "2000-01-01", Agents: map[string][]string{"agy": {"gemini-3.9-flash-high"}}}); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLatest("agy", "gemini-3.8-flash-high"); got != "gemini-3.8-flash-high" {
		t.Fatalf("stale cache resolved %q", got)
	}
}

func TestDiscoveryCachesAllAgentsBeforeNotice(t *testing.T) {
	for _, agent := range []string{"agy", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			floor, latest := "gemini-3.8-flash-high", "gemini-3.9-flash-high"
			if agent == "opencode" {
				floor, latest = "google/"+floor, "google/"+latest
			}
			calls := map[string]int{}
			root := setupDiscovery(t, map[string]any{"coder_agent": agent, "routes": map[string]any{"normal": floor}, "reviewer1_model_requested": "gemini-3.8-flash-high"}, func(a string, _ map[string]map[string]json.RawMessage) listingResult {
				calls[a]++
				output := latest + "\n"
				if a == "agy" {
					output = "gemini-3.9-flash-high\tFlash\n"
				}
				ids, err := parseListing(a, output)
				if err != nil {
					t.Fatal(err)
				}
				return listingResult{available: true, ids: ids}
			})
			DiscoverModelVersion(root)
			if got := ResolveLatest(agent, floor); got != latest {
				t.Fatalf("resolved %q", got)
			}
			if got := ResolveLatest("agy", "gemini-3.8-flash-high"); got != "gemini-3.9-flash-high" {
				t.Fatalf("reviewer resolved %q", got)
			}
			DiscoverModelVersion(root)
			for a, count := range calls {
				if count != 1 {
					t.Fatalf("%s listed %d times", a, count)
				}
			}
		})
	}
}

func TestDiscoverNewerWritesReportAndDailyCheck(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"coder_agent": "codex", "routes": map[string]any{
		"normal": map[string]any{"model": "gpt-6-luna"}, "mechanical": "gpt-6-luna",
	}}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent != "codex" {
			t.Fatalf("agent = %q", agent)
		}
		return listingResult{available: true, ids: []string{"gpt-6.1-luna"}}
	})
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-luna","visibility":"list"}]}`)
	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gpt-6-luna" || newer != "gpt-6.1-luna" {
		t.Fatalf("got %q %q %v", current, newer, ok)
	}
	state := readStateJSON(t, root)
	want := map[string]any{"checked_on": nowDate(),
		"report": map[string]any{"current": "gpt-6-luna", "new": "gpt-6.1-luna", "delivered": false}}
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
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-luna","visibility":"list"},{"slug":"gpt-6.1-sol","visibility":"list"},{"slug":"gpt-6.1-astra","visibility":"list"}]}`)

	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "gpt-6-luna" || newer != "gpt-6.1-luna" {
		t.Fatalf("first route checked = %q -> %q, available = %v; want zeta route gpt-6-luna", current, newer, ok)
	}
	if got := readStateJSON(t, root)["report"].(map[string]any)["current"]; got != current {
		t.Fatalf("report current = %v, want %q", got, current)
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
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-sol","visibility":"list"}]}`)

	repository := t.TempDir()
	current, newer, ok := DiscoverModelVersion(repository)
	if !ok || current != "gpt-6-sol" || newer != "gpt-6.1-sol" {
		t.Fatalf("got %q -> %q, available = %v; want overlay route", current, newer, ok)
	}
	if lastConfigRepository != repository {
		t.Fatalf("config repository = %q, want %q", lastConfigRepository, repository)
	}
	if got := readStateJSON(t, root)["report"].(map[string]any)["current"]; got != current {
		t.Fatalf("report current = %v, want %q", got, current)
	}
}

func TestDiscoverReportsEachChangeOnce(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		return listingResult{available: true, ids: []string{"gpt-6.1-luna"}}
	})
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-luna","visibility":"list"}]}`)
	if current, newer, ok := DiscoverModelVersion(t.TempDir()); !ok || current != "gpt-6-luna" || newer != "gpt-6.1-luna" {
		t.Fatalf("first report = %q -> %q, available = %v", current, newer, ok)
	}
	state := readNoticeState(filepath.Join(root, "model-version-notice.json"))
	if state.Report == nil || state.Report.New != ResolveLatest("codex", "gpt-6-luna") {
		t.Fatalf("report does not match resolver: %#v", state.Report)
	}
	state.Report.Delivered = true
	yesterday := "2000-01-01"
	state.CheckedOn = &yesterday
	if err := writeNoticeState(filepath.Join(root, "model-version-notice.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatal("same change was reported again after delivery")
	}
}

func TestDiscoverCatalogModelInUnconfiguredFamily(t *testing.T) {
	setupDiscovery(t, map[string]any{"coder_agent": "codex", "routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		return listingResult{available: true, ids: []string{"gpt-6-luna"}}
	})
	loadCatalog = func() (catalog, error) {
		return catalog{Providers: map[string]map[string]json.RawMessage{
			"anthropic": {"claude-5-5-haiku": json.RawMessage(`{"name":"Claude Haiku 5.5","reasoning":true,"family":"claude-haiku","release_date":"2026-10-07"}`)},
		}}, nil
	}

	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != "Claude Haiku" || newer != "claude-5-5-haiku" {
		t.Fatalf("got %q -> %q, available = %v", current, newer, ok)
	}
}

func TestDiscoverCatalogModelAlreadyAnnouncedDoesNotRepeat(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"coder_agent": "codex", "routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		return listingResult{available: true, ids: []string{"gpt-6-luna"}}
	})
	loadCatalog = func() (catalog, error) {
		return catalog{Providers: map[string]map[string]json.RawMessage{
			"anthropic": {"claude-5-5-haiku": json.RawMessage(`{"name":"Claude Haiku 5.5","reasoning":true,"family":"claude-haiku","release_date":"2026-10-07"}`)},
		}}, nil
	}
	yesterday := "2000-01-01"
	writeCheckedState(t, root, yesterday, &noticeReport{Current: "Claude Haiku", New: "claude-5-5-haiku", Delivered: true})

	if current, newer, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatalf("already announced model repeated as %q -> %q", current, newer)
	}
}

func TestDiscoverUnavailableListingRecordsCheckWithoutReport(t *testing.T) {
	root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
		return listingResult{}
	})
	if _, _, ok := DiscoverModelVersion(t.TempDir()); ok {
		t.Fatal("unavailable listing discovered a model")
	}
	state := readStateJSON(t, root)
	if state["report"] != nil || state["checked_on"] != nowDate() {
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

func TestDiscoverCachedCodexVersionReportsVisibleUpgrade(t *testing.T) {
	config := map[string]any{"routes": map[string]any{
		"normal": "gpt-6-luna", "mechanical": "gpt-6-luna", "subtle": "gpt-6-sol",
	}}
	root := setupDiscoveryWithRouteOrder(t, config, []string{"normal", "mechanical", "subtle"}, nil)
	writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-sol","visibility":"list"},{"slug":"gpt-7-luna","visibility":"hide"}]}`)
	current, newer, ok := DiscoverCachedCodexVersion()
	if !ok || current != "gpt-6-sol" || newer != "gpt-6.1-sol" {
		t.Fatalf("got %q -> %q, available = %v", current, newer, ok)
	}
	if resolved := ResolveLatest("codex", current); resolved != newer {
		t.Fatalf("reported model %q differs from resolver %q", newer, resolved)
	}
	state := readStateJSON(t, root)
	if got := state["report"].(map[string]any); got["current"] != current || got["new"] != newer {
		t.Fatalf("report = %#v", got)
	}
}

func TestDiscoverCachedCodexVersionRespectsNoticeState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *noticeReport
	}{
		{name: "delivered report", report: &noticeReport{Current: "gpt-6-luna", New: "gpt-6.1-luna", Delivered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := setupDiscovery(t, map[string]any{"routes": map[string]any{"normal": "gpt-6-luna"}}, nil)
			writeCodexCache(t, `{"models":[{"slug":"gpt-6.1-luna","visibility":"list"}]}`)
			if err := writeNoticeState(filepath.Join(root, "model-version-notice.json"), noticeState{Report: tc.report}); err != nil {
				t.Fatal(err)
			}
			if _, _, ok := DiscoverCachedCodexVersion(); ok {
				t.Fatal("unexpected model notice")
			}
			state := readNoticeState(filepath.Join(root, "model-version-notice.json"))
			if !reflect.DeepEqual(state.Report, tc.report) {
				t.Fatalf("report changed: got %#v want %#v", state.Report, tc.report)
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

func TestDiscoverCodexCLIVersionReportsNewerClientVersionOnce(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	writeCodexCache(t, `{"client_version":"0.161.0","models":[]}`)
	writeCodexCLIState(t, root, "0.160.0", nil)
	for attempt := 0; attempt < 2; attempt++ {
		installed, available, ok := DiscoverCodexCLIVersion()
		if attempt == 0 && (!ok || installed != "0.160.0" || available != "0.161.0") {
			t.Fatalf("got %q -> %q, available = %v", installed, available, ok)
		}
		if attempt == 1 && ok {
			t.Fatal("repeated Codex CLI notice")
		}
	}
}

func TestDiscoverCodexCLIVersionIgnoresEqualOrOlderClientVersion(t *testing.T) {
	for _, clientVersion := range []string{"0.160.0", "0.159.0"} {
		t.Run(clientVersion, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PURSER_STATE_DIR", root)
			writeCodexCache(t, `{"client_version":"`+clientVersion+`","models":[]}`)
			writeCodexCLIState(t, root, "0.160.0", nil)
			if _, _, ok := DiscoverCodexCLIVersion(); ok {
				t.Fatal("unexpected Codex CLI notice")
			}
		})
	}
}

func writeCodexCLIState(t *testing.T, root, installed string, asked []string) {
	t.Helper()
	today := time.Now().UTC().Format("2006-01-02")
	data, err := json.Marshal(map[string]any{"checked_on": today, "installed_version": installed, "asked": asked})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "codex-cli-version.json"), data, 0600); err != nil {
		t.Fatal(err)
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
	if got := readStateJSON(t, root)["report"].(map[string]any)["current"]; got != current {
		t.Fatalf("report current = %v", got)
	}
}

func TestDiscoverListsOpenCodeForEveryRole(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
	}{
		{name: "race default", config: map[string]any{}},
		{name: "coder", config: map[string]any{"coder_agent": "opencode", "routes": map[string]any{"normal": "openai/gpt-6-luna"}}},
		{name: "reviewer1", config: map[string]any{"reviewer1_agent": "opencode", "reviewer1_model_requested": "openai/gpt-6-luna"}},
		{name: "fallback", config: map[string]any{"fallback": map[string]any{"agent": "opencode", "model": "openai/gpt-6-luna"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			setupDiscovery(t, tc.config, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
				if agent == "opencode" {
					calls++
					return listingResult{available: true, ids: []string{"openai/gpt-6.1-luna"}}
				}
				return listingResult{}
			})
			DiscoverModelVersion(t.TempDir())
			if calls != 1 {
				t.Fatalf("OpenCode listing calls = %d, want 1", calls)
			}
		})
	}
}

func TestDiscoverExplicitlyDisablesDefaultOpenCodeRace(t *testing.T) {
	calls := 0
	setupDiscovery(t, map[string]any{"mechanical_opencode_enabled": false}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent == "opencode" {
			calls++
		}
		return listingResult{}
	})
	DiscoverModelVersion(t.TempDir())
	if calls != 0 {
		t.Fatalf("OpenCode listing calls = %d, want none when explicitly disabled", calls)
	}
}

func TestOpenCodeAttemptSharedAcrossConcurrentRepositories(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	setupDiscovery(t, map[string]any{"mechanical_opencode_enabled": true}, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent == "opencode" {
			mu.Lock()
			calls++
			mu.Unlock()
			return listingResult{available: true, ids: []string{"openai/gpt-6.1-luna"}}
		}
		return listingResult{}
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			DiscoverModelVersion(t.TempDir())
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("OpenCode listing calls = %d, want one shared daily attempt", calls)
	}
}

func TestFailedOpenCodeListingPreservesSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result listingResult
	}{
		{name: "failed", result: listingResult{}},
		{name: "invalid empty snapshot", result: listingResult{available: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := setupDiscovery(t, map[string]any{"coder_agent": "opencode", "routes": map[string]any{"normal": "openai/gpt-6-luna"}}, func(string, map[string]map[string]json.RawMessage) listingResult {
				return tc.result
			})
			yesterday := "2000-01-01"
			before := resolutionCache{OpenCodeAttempted: yesterday, OpenCodeSnapshot: yesterday, Agents: map[string][]string{"opencode": {"openai/gpt-6.1-luna"}}}
			if err := atomicJSON(filepath.Join(root, "model-resolution.json"), before); err != nil {
				t.Fatal(err)
			}
			DiscoverModelVersion(t.TempDir())
			after := readResolutionCache()
			if !reflect.DeepEqual(after.Agents["opencode"], before.Agents["opencode"]) || after.OpenCodeSnapshot != yesterday {
				t.Fatalf("failed refresh changed good snapshot: %#v", after)
			}
			if got := ResolveLatest("opencode", "openai/gpt-6-luna"); got != "openai/gpt-6.1-luna" {
				t.Fatalf("prior successful snapshot resolved %q", got)
			}
			if after.OpenCodeAttempted != nowDate() {
				t.Fatalf("attempt date = %q, want %q", after.OpenCodeAttempted, nowDate())
			}
		})
	}
}

func TestOpenCodeListingOutputIsBounded(t *testing.T) {
	output := &limitedOutput{limit: 3}
	if _, err := output.Write([]byte("four")); err == nil {
		t.Fatal("oversized listing output was accepted")
	}
	if len(output.data) != 3 {
		t.Fatalf("captured %d bytes, want limit 3", len(output.data))
	}
}

func TestOpenCodeBinaryFindsWindowsNpmVariantFromCmdLauncher(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("Windows x64 npm launcher layout")
	}
	root := t.TempDir()
	launcher := filepath.Join(root, "opencode.cmd")
	if err := os.WriteFile(launcher, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "node_modules", "opencode-ai", "node_modules", "opencode-windows-x64", "bin", "opencode.exe")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	oldLookPath := lookPath
	lookPath = func(string) (string, error) { return launcher, nil }
	t.Cleanup(func() { lookPath = oldLookPath })

	got, err := opencodeBinary()
	if err != nil || got != executable {
		t.Fatalf("opencodeBinary() = %q, %v; want %q, nil", got, err, executable)
	}
}

func TestOpenCodeBinaryRejectsWindowsCmdWithoutExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm launcher layout")
	}
	launcher := filepath.Join(t.TempDir(), "opencode.cmd")
	if err := os.WriteFile(launcher, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	oldLookPath := lookPath
	lookPath = func(string) (string, error) { return launcher, nil }
	t.Cleanup(func() { lookPath = oldLookPath })

	got, err := opencodeBinary()
	if err != nil || got != "" {
		t.Fatalf("opencodeBinary() = %q, %v; want empty path, nil", got, err)
	}
}

func TestOpenCodeSameFamilyUpgradeKeepsConfiguredFloor(t *testing.T) {
	floor := "openai/gpt-6-luna"
	config := map[string]any{"coder_agent": "opencode", "routes": map[string]any{"normal": floor}}
	setupDiscovery(t, config, func(agent string, _ map[string]map[string]json.RawMessage) listingResult {
		if agent != "opencode" {
			t.Fatalf("listed %q, want opencode", agent)
		}
		return listingResult{available: true, ids: []string{"openai/gpt-6.1-luna"}}
	})
	current, newer, ok := DiscoverModelVersion(t.TempDir())
	if !ok || current != floor || newer != "openai/gpt-6.1-luna" {
		t.Fatalf("discovery = %q -> %q, %v", current, newer, ok)
	}
	if got := ResolveLatest("opencode", floor); got != newer {
		t.Fatalf("resolved %q, want %q", got, newer)
	}
	if got := routeModel(config["routes"].(map[string]any)["normal"]); got != floor {
		t.Fatalf("configured floor changed to %q, want %q", got, floor)
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

func TestNewerVersionRejectsOtherFamiliesOlderAndUnparsableCandidates(t *testing.T) {
	cases := []struct {
		current    string
		candidates []string
	}{
		{"gpt-6-luna", []string{"gpt-6.1-sol", "gpt-5-luna", "latest-luna", "gpt-luna"}},
		{"gemini-3.8-flash-high", []string{"gemini-3.9-pro-high", "gemini-3.9-flash-medium"}},
	}
	for _, test := range cases {
		if got := newerVersion(test.current, test.candidates); got != "" {
			t.Errorf("newerVersion(%q, %v) = %q, want no candidate", test.current, test.candidates, got)
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
		{"agy", "gemini-3.9-flash-high\tFlash\ngemini-3.9-flash-low\tFlash\n", []string{"gemini-3.9-flash-high", "gemini-3.9-flash", "gemini-3.9-flash-low"}},
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

func TestListingCommandFailureIsUnavailable(t *testing.T) {
	oldLookPath, oldRun := lookPath, runListingCommand
	t.Cleanup(func() { lookPath, runListingCommand = oldLookPath, oldRun })
	lookPath = func(string) (string, error) { return filepath.Join(t.TempDir(), "mock-codex"), nil }
	runListingCommand = func(*exec.Cmd) ([]byte, error) { return nil, errors.New("listing failed") }
	if result := listModels("codex", nil); result.available || len(result.ids) != 0 {
		t.Fatalf("failed listing = %#v, want unavailable", result)
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
