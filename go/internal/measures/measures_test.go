package measures

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLedgerItems(t *testing.T) {
	data := []byte("# Other\n- **Ignored.** Result: done\n\n## Measurements ledger\n- **Pending item.** Source data. Due 2026-10-10. Result: pending; see docs/pending.md and data/run.jsonl.\n- **Finished item.** Source data. Due: 2026-09-01. Result: completed; see docs/final.md.\n")
	items := Parse(data, "Measurements ledger", func(path string) bool {
		return path == "docs/pending.md" || path == "data/run.jsonl" || path == "docs/final.md"
	})
	if len(items) != 2 || items[0].Status != "pending" || items[1].Status != "done" || items[0].Due != "2026-10-10" || items[1].Due != "2026-09-01" {
		t.Fatalf("parsed items: %#v", items)
	}
	if len(items[0].Paths) != 2 || items[0].Paths[0] != "docs/pending.md" || items[0].Paths[1] != "data/run.jsonl" {
		t.Fatalf("paths: %#v", items[0].Paths)
	}
}

func TestSyncProjectionSafetyAndDisabled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	vault := filepath.Join(t.TempDir(), "vault")
	t.Setenv("PURSER_OBSIDIAN_VAULT", vault)
	plugin := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".orchestrator", "profile.toml"), "schema_version = 1\n[project]\nmeasurement_ledger = \"MEASURE.md#Measurements\"\n")
	write(filepath.Join(root, "MEASURE.md"), "## Measurements\n- **Completion rate.** Due 2026-10-10. Result: pending; docs/report.md\n")
	write(filepath.Join(root, "docs", "report.md"), "# Report\n")
	config := filepath.Join(plugin, ".claude", "hooks", "orchestrator", "config.json")
	write(config, `{"obsidian_projection":{"enabled":true,"vault_root":"`+strings.ReplaceAll(vault, `\`, `\\`)+`"}}`)
	count, err := Sync(context.Background(), root)
	if err != nil || count != 1 {
		t.Fatalf("first sync = %d, %v", count, err)
	}
	key := slug(filepath.Base(root))
	note := filepath.Join(vault, "Measurements", key, "completion-rate.md")
	if _, err := os.Stat(note); err != nil {
		t.Fatal(err)
	}
	count, err = Sync(context.Background(), root)
	if err != nil || count != 0 {
		t.Fatalf("unchanged sync = %d, %v", count, err)
	}
	if err := os.WriteFile(note, []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	count, err = Sync(context.Background(), root)
	if err != nil || count != 0 {
		t.Fatalf("protected sync = %d, %v", count, err)
	}
	data, _ := os.ReadFile(note)
	if string(data) != "user edit\n" {
		t.Fatalf("user edit overwritten: %q", data)
	}
	second := filepath.Join(t.TempDir(), "project")
	write(filepath.Join(second, ".orchestrator", "profile.toml"), "schema_version = 1\n[project]\nmeasurement_ledger = \"MEASURE.md#Measurements\"\n")
	write(filepath.Join(second, "MEASURE.md"), "## Measurements\n- **Second item.** Result: done\n")
	write(config, `{"obsidian_projection":{"enabled":true,"vault_root":"`+strings.ReplaceAll(vault, `\`, `\\`)+`"}}`)
	count, err = Sync(context.Background(), second)
	if err != nil || count != 1 {
		t.Fatalf("colliding project sync = %d, %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(vault, "Measurements", slug(filepath.Base(second))+"-"+projectKey(second), "Ledger.md")); err != nil {
		t.Fatalf("collision folder missing: %v", err)
	}
	write(config, `{"obsidian_projection":{"enabled":false,"vault_root":"`+strings.ReplaceAll(vault, `\`, `\\`)+`"}}`)
	other := filepath.Join(t.TempDir(), "third-project")
	write(filepath.Join(other, ".orchestrator", "profile.toml"), "schema_version = 1\n[project]\nmeasurement_ledger = \"MEASURE.md\"\n")
	write(filepath.Join(other, "MEASURE.md"), "- **No output.** Result: done\n")
	write(filepath.Join(other, ".claude", "hooks", "orchestrator", "config.json"), `{"obsidian_projection":{"enabled":false,"vault_root":"`+strings.ReplaceAll(vault, `\`, `\\`)+`"}}`)
	count, err = Sync(context.Background(), other)
	if err != nil || count != 0 {
		t.Fatalf("disabled sync = %d, %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(vault, "Measurements", slug(filepath.Base(other)))); !os.IsNotExist(err) {
		t.Fatalf("disabled projection wrote output: %v", err)
	}
}
