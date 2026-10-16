package contextio

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeNoticeJSON(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeFamilyNoticeSeedsBaselineAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	repo := updateSessionTestRepo(t)
	if got, want := openCodeFamily("opencode/glm-5.3-flash-free"), "opencode/glm"; got != want {
		t.Fatalf("GLM family = %q, want %q", got, want)
	}
	if got, want := openCodeFamily("deepseek/deepseek-v4.1-flash-free"), "deepseek/deepseek-v"; got != want {
		t.Fatalf("DeepSeek family = %q, want %q", got, want)
	}
	if got, want := openCodeFamily("opencode/unnumbered"), "opencode/unnumbered"; got != want {
		t.Fatalf("unnumbered family = %q, want %q", got, want)
	}
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-16","agents":{"opencode":["opencode/deepseek-v4.1-flash-free"]}}`)
	rolePath := filepath.Join(root, "projects", repositoryProjectKey(repo), "roles.toml")
	writeNoticeJSON(t, rolePath, "schema_version = 1\n[roles]\ncoder = \"opencode\"\n")
	if got := openCodeFamilyContext(repo); got != "" {
		t.Fatalf("baseline emitted a family notice: %q", got)
	}
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-17","agents":{"opencode":["opencode/deepseek-v4.1-flash-free","opencode/glm-5.3-flash-free"]}}`)
	first := openCodeFamilyContext(repo)
	if !strings.Contains(first, "New OpenCode model family") || !strings.Contains(first, "`opencode/glm`") || !strings.Contains(first, "`coder`") {
		t.Fatalf("new family notice = %q", first)
	}
	if repeated := openCodeFamilyContext(repo); repeated != "" {
		t.Fatalf("family notice repeated: %q", repeated)
	}
}

func TestOpenCodeFamilyNoticeUsesSessionNoticePath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())
	repo := updateSessionTestRepo(t)
	writeNoticeJSON(t, filepath.Join(root, "projects", repositoryProjectKey(repo), "telemetry", "compaction_advisor.json"), `{}`)
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-16","agents":{"opencode":["alpha/one"]}}`)
	writeNoticeJSON(t, filepath.Join(root, "projects", repositoryProjectKey(repo), "roles.toml"), "schema_version = 1\n[roles]\ncoder = \"opencode\"\n")
	if got := sessionUpdateContext("session-a", repo, nil); strings.Contains(got, "New OpenCode model family") {
		t.Fatalf("uninitialized session notice state claimed family: %q", got)
	}
	if got := sessionUpdateContext("session-a", repo, map[string]any{}); strings.Contains(got, "New OpenCode model family") {
		t.Fatalf("first successful listing was not treated as baseline: %q", got)
	}
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-17","agents":{"opencode":["alpha/one","beta/two"]}}`)
	got := sessionUpdateContext("session-a", repo, map[string]any{})
	if !strings.Contains(got, "New OpenCode model family") {
		t.Fatalf("session notice path omitted family notice: %q", got)
	}
	if repeated := sessionUpdateContext("session-b", repo, map[string]any{}); strings.Contains(repeated, "New OpenCode model family") {
		t.Fatalf("family notice repeated in another session: %q", repeated)
	}
}

func TestOpenCodeFamilyNoticeConcurrentDurableClaim(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode", "candidates.json")
	writeNoticeJSON(t, path, `{"race":{"started":true,"finished":false,"started_at":"race-1","notice_claim_ids":[]}}`)
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimInventoryNotice(root, "race-start:race-1", "race") {
				mu.Lock()
				claims++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("durable claim count = %d, want 1", claims)
	}
}

func TestMechanicalRaceStartNoticeVariants(t *testing.T) {
	for _, tc := range []struct {
		name, inventory, want string
	}{
		{"single", `{"candidates":[],"race":{"started":true,"finished":false,"started_at":"r1","entrants":["new/model","old/model"],"stage":"newcomer_vs_leader","notice_claim_ids":[]}}`, "OpenCode added `new/model`; it is racing `old/model`"},
		{"several", `{"candidates":[{"id":"old/model","status":"measured"}],"race":{"started":true,"finished":false,"started_at":"r2","entrants":["new/a","new/b"],"stage":"newcomer_bracket","notice_claim_ids":[]}}`, "they race each other first, then the winner races `old/model`"},
		{"bootstrap", `{"bootstrap":{"status":"started","started_at":"b1","entrants":["a/model","b/model"]}}`, "racing all 2 of them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PURSER_STATE_DIR", root)
			writeNoticeJSON(t, filepath.Join(root, "opencode", "candidates.json"), tc.inventory)
			if got := mechanicalRaceContext(); !strings.Contains(got, tc.want) {
				t.Fatalf("race notice = %q, want substring %q", got, tc.want)
			}
			if repeated := mechanicalRaceContext(); repeated != "" {
				t.Fatalf("race notice repeated: %q", repeated)
			}
		})
	}
}

func TestBootstrapRaceFinishNotice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	writeNoticeJSON(t, filepath.Join(root, "opencode", "candidates.json"), `{"bootstrap":{"status":"finished","finished_at":"done","winner":"vendor/winner","results":{"vendor/winner":[true,false,true]}}}`)
	got := mechanicalRaceContext()
	if !strings.Contains(got, "`vendor/winner` won with 2 of 3 tasks") {
		t.Fatalf("bootstrap finish notice = %q", got)
	}
	if repeated := mechanicalRaceContext(); repeated != "" {
		t.Fatalf("bootstrap finish notice repeated: %q", repeated)
	}
}

func TestActiveRaceResultRequiresCurrentWinner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode", "candidates.json")
	writeNoticeJSON(t, path, `{"candidates":[{"id":"vendor/new","status":"measured","mechanical_samples":[true,true,true]},{"id":"vendor/old","status":"measured","mechanical_samples":[true,true,false]}],"race":{"started":true,"finished":true,"winner":"vendor/new","finished_at":"done","stage":"newcomer_vs_leader","entrants":["vendor/new","vendor/old"],"samples":{},"notice_claim_ids":[]}}`)
	if !claimActiveRaceResult(root, "vendor/new") {
		t.Fatal("post-win active top candidate was not claimed")
	}
}

func TestOpenCodeFamilyNoticePersistenceFailureKeepsPendingFamily(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	repo := updateSessionTestRepo(t)
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-17","agents":{"opencode":["alpha/one"]}}`)
	if got := openCodeFamilyContext(repo); got != "" {
		t.Fatalf("baseline notice = %q", got)
	}
	writeNoticeJSON(t, filepath.Join(root, "model-resolution.json"), `{"opencode_snapshot_on":"2026-10-18","agents":{"opencode":["alpha/one","beta/two"]}}`)
	path := filepath.Join(root, "opencode", "family-notices.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if got := openCodeFamilyContext(repo); got != "" {
		t.Fatalf("notice on persistence failure = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(path, "state"))
	if !os.IsNotExist(err) || len(data) != 0 {
		t.Fatalf("failed write unexpectedly persisted: %s, %v", data, err)
	}
}

func TestRaceNoticeClaimPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if claimInventoryNotice(root, "race-start:r1", "race") {
		t.Fatal("claim succeeded without writable inventory")
	}
	data, err := os.ReadFile(filepath.Join(path, "notice_claim_ids"))
	if !os.IsNotExist(err) || len(data) != 0 {
		t.Fatalf("failed claim persisted: %s, %v", data, err)
	}
}
