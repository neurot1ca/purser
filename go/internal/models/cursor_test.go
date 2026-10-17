package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/policy"
)

const cursorListingSample = "Available models\n" +
	"gpt-5.3-codex - GPT Codex\n" +
	"gpt-5.3-codex-high-fast - GPT Codex High Fast\n" +
	"gpt-5.6-sol-high - GPT Sol\n" +
	"claude-opus-5-5-high - Claude Opus\n" +
	"claude-opus-5-thinking-high - Claude Thinking\n" +
	"claude-4.6-opus-high - Claude Opus Legacy\n" +
	"claude-sonnet-5-5-medium - Claude Sonnet\n" +
	"gemini-3.8-flash-high - Gemini Flash\n" +
	"gemini-3.1-pro - Gemini Pro\n" +
	"grok-4.7-high - Grok\n" +
	"cursor-grok-4.6-high-fast - Cursor Grok\n" +
	"composer-2.5 - Composer\n" +
	"kimi-k3-high - Kimi\n" +
	"muse-spark-1.3-low - Muse\n" +
	"auto - Automatic\n"

func cursorConfigTOML(model, reviewer, reviewerModel string) string {
	routes := ""
	for _, name := range []string{"mechanical", "operation", "subtle", "normal", "consequential", "plan_standard", "escalated"} {
		routes += "[coding.routes." + name + "]\nmodel = \"" + model + "\"\neffort = \"none\"\n"
	}
	return "[roles]\ncoder = \"cursor\"\nreviewer = \"" + reviewer + "\"\n[coding]\ncoder_agent = \"cursor\"\nreviewer1_agent = \"" + reviewer + "\"\nreviewer1_model_requested = \"" + reviewerModel + "\"\nreviewer1_effort = \"none\"\n" + routes
}

func TestCursorRealListingIDsAcceptedByConfig(t *testing.T) {
	root := t.TempDir()
	userPath := filepath.Join(root, "roles.toml")
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	for _, model := range []string{"gpt-5.3-codex", "gpt-5.6-sol-high", "claude-opus-5-5-high", "claude-4.6-opus-high", "gemini-3.8-flash-high", "grok-4.7-high", "cursor-grok-4.6-high-fast", "composer-2.5", "kimi-k3-high", "muse-spark-1.3-low"} {
		if err := os.WriteFile(userPath, []byte(cursorConfigTOML(model, "cursor", "claude-opus-5-5-high")), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := policy.LoadCoderConfig(configPath, userPath); err != nil {
			t.Fatalf("real Cursor model %q rejected: %v", model, err)
		}
	}
}

func TestCursorAutoRejectedByConfig(t *testing.T) {
	root := t.TempDir()
	userPath := filepath.Join(root, "roles.toml")
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	if err := os.WriteFile(userPath, []byte(cursorConfigTOML("auto", "cursor", "claude-opus-5-5-high")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.LoadCoderConfig(configPath, userPath); err == nil {
		t.Fatal("Cursor auto model was accepted")
	}
}

func TestCursorLatestFamilyResolutionUsesNumericVersions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", filepath.Join(root, "state"))
	ids, err := parseListing("cursor", cursorListingSample)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(stateRoot(), "model-resolution.json"), resolutionCache{Agents: map[string][]string{"cursor": ids}}); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLatest("cursor", "latest:claude-opus-high"); got != "claude-opus-5-5-high" {
		t.Fatalf("Claude latest = %q", got)
	}
	if got := ResolveLatest("cursor", "latest:grok-high"); got != "grok-4.7-high" {
		t.Fatalf("Grok latest = %q", got)
	}
}

func TestCursorLatestPrefixVersionWinsInEitherListingOrder(t *testing.T) {
	for _, ids := range [][]string{
		{"claude-opus-5-high", "claude-opus-5-5-high"},
		{"claude-opus-5-5-high", "claude-opus-5-high"},
	} {
		t.Run(strings.Join(ids, "_"), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PURSER_STATE_DIR", filepath.Join(root, "state"))
			if err := atomicJSON(filepath.Join(stateRoot(), "model-resolution.json"), resolutionCache{Agents: map[string][]string{"cursor": ids}}); err != nil {
				t.Fatal(err)
			}
			if got := ResolveLatest("cursor", "latest:claude-opus-high"); got != "claude-opus-5-5-high" {
				t.Fatalf("latest Claude model = %q for listing %v", got, ids)
			}
		})
	}
}

func TestCursorUnknownVendorCoderAllowedReviewerRejected(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	coderPath := filepath.Join(root, "coder.toml")
	if err := os.WriteFile(coderPath, []byte(cursorConfigTOML("muse-spark-1.3-low", "cursor", "claude-opus-5-5-high")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.LoadCoderConfig(configPath, coderPath); err != nil {
		t.Fatalf("unknown-vendor Cursor coder rejected: %v", err)
	}
}

func TestCursorDailyDiscoveryOnlyWhenConfiguredAndFailureKeepsExactModel(t *testing.T) {
	oldConfig, oldCatalog, oldListing := loadConfig, loadCatalog, listModels
	t.Cleanup(func() { loadConfig, loadCatalog, listModels = oldConfig, oldCatalog, oldListing })
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	called := 0
	loadConfig = func(string) (loadedConfig, error) {
		return loadedConfig{Values: map[string]any{"coder_agent": "codex", "routes": map[string]any{"normal": map[string]any{"model": "gpt-6-luna"}}, "mechanical_opencode_enabled": false}}, nil
	}
	loadCatalog = func() (catalog, error) { return catalog{Providers: map[string]map[string]json.RawMessage{}}, nil }
	listModels = func(agent string, _ map[string]map[string]json.RawMessage, _ time.Duration) listingResult {
		if agent == "cursor" {
			called++
		}
		return listingResult{}
	}
	_, _, _ = DiscoverModelVersion(t.TempDir())
	if called != 0 {
		t.Fatalf("unconfigured Cursor listing called %d times", called)
	}

	t.Setenv("PURSER_STATE_DIR", t.TempDir())
	loadConfig = func(string) (loadedConfig, error) {
		return loadedConfig{Values: map[string]any{"coder_agent": "cursor", "routes": map[string]any{"normal": map[string]any{"model": "gpt-5.3-codex", "effort": "none"}}, "mechanical_opencode_enabled": false}}, nil
	}
	listModels = func(agent string, _ map[string]map[string]json.RawMessage, _ time.Duration) listingResult {
		if agent == "cursor" {
			called++
		}
		return listingResult{}
	}
	if got := ResolveLatest("cursor", "gpt-5.3-codex"); got != "gpt-5.3-codex" {
		t.Fatalf("listing failure changed exact model to %q", got)
	}
	_, _, _ = DiscoverModelVersion(t.TempDir())
	if called != 1 {
		t.Fatalf("configured Cursor listing calls = %d, want 1", called)
	}
}

func TestParseCursorListingSampleIgnoresDisplayNames(t *testing.T) {
	got, err := parseListing("cursor", cursorListingSample)
	want := []string{"gpt-5.3-codex", "gpt-5.3-codex-high-fast", "gpt-5.6-sol-high", "claude-opus-5-5-high", "claude-opus-5-thinking-high", "claude-4.6-opus-high", "claude-sonnet-5-5-medium", "gemini-3.8-flash-high", "gemini-3.1-pro", "grok-4.7-high", "cursor-grok-4.6-high-fast", "composer-2.5", "kimi-k3-high", "muse-spark-1.3-low"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parse Cursor listing = %v, %v; want %v", got, err, want)
	}
	if _, err := parseListing("cursor", "Available models\nauto - Automatic\n"); err == nil {
		t.Fatal("auto-only listing accepted")
	}
}
