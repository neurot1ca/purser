package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonLedgerFixtures(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	got := Load(path, "go-pass3-fixture")
	if got.Status != "OK" || got.Code != "LEDGER_LOADED" || got.Record["task_id"] != "go-pass3-fixture" {
		t.Fatalf("valid Python fixture: %#v", got)
	}

	if err := os.WriteFile(path, fixture[:len(fixture)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("truncated fixture: %#v", got)
	}

	var envelope map[string]any
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["checksum"] = "0000000000000000000000000000000000000000000000000000000000000000"
	changed, _ := json.Marshal(envelope)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("checksum mismatch: %#v", got)
	}

	payload := envelope["payload"].(map[string]any)
	payload["unexpected"] = true
	canonical, _ := canonicalJSON(payload)
	digest := sha256.Sum256(canonical)
	envelope["checksum"] = hex.EncodeToString(digest[:])
	changed, _ = json.Marshal(envelope)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("checksummed invalid record: %#v", got)
	}
}
