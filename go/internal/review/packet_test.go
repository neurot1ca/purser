package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

type packetFixture struct {
	Materials map[string]any `json:"materials"`
	MaxBytes  int            `json:"max_bytes"`
	Expected  struct {
		Kind                string            `json:"kind"`
		Packet              map[string]string `json:"packet"`
		Canonical           string            `json:"canonical"`
		SHA256              string            `json:"sha256"`
		AddedRedactionCount int               `json:"added_redaction_count"`
		AddedAffectedFiles  []string          `json:"added_affected_files"`
		Code                string            `json:"code"`
		Message             string            `json:"message"`
		Omitted             []string          `json:"omitted"`
	} `json:"expected"`
}

func TestPacketOracleGoldens(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "packet", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 6 {
		t.Fatalf("fixture count = %d, want 6", len(paths))
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture packetFixture
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			got := BuildReviewPacket(fixture.Materials, fixture.MaxBytes)
			if fixture.Expected.Kind == "ready" {
				ready, ok := got.(PacketReady)
				if !ok {
					t.Fatalf("BuildReviewPacket() = %#v, want ready", got)
				}
				if !equalPacketMap(ready.Packet, fixture.Expected.Packet) {
					t.Fatalf("packet = %#v, want %#v", ready.Packet, fixture.Expected.Packet)
				}
				if string(ready.Encoded) != fixture.Expected.Canonical {
					t.Fatalf("canonical = %s, want %s", ready.Encoded, fixture.Expected.Canonical)
				}
				digest := sha256.Sum256(ready.Encoded)
				if hex.EncodeToString(digest[:]) != fixture.Expected.SHA256 {
					t.Fatalf("sha256 mismatch: %x", digest)
				}
				if ready.AddedRedactionCount != fixture.Expected.AddedRedactionCount || !equalStrings(ready.AddedAffectedFiles, fixture.Expected.AddedAffectedFiles) {
					t.Fatalf("redaction summary = (%d, %v)", ready.AddedRedactionCount, ready.AddedAffectedFiles)
				}
				return
			}
			omitted, ok := got.(PacketOmitted)
			if !ok {
				t.Fatalf("BuildReviewPacket() = %#v, want omitted", got)
			}
			if omitted.Code != fixture.Expected.Code || omitted.Message != fixture.Expected.Message || !equalStrings(omitted.Omitted, fixture.Expected.Omitted) {
				t.Fatalf("omitted = %#v, want code=%q message=%q omitted=%v", omitted, fixture.Expected.Code, fixture.Expected.Message, fixture.Expected.Omitted)
			}
		})
	}
}

func TestWritePacketDurableFile(t *testing.T) {
	dir := t.TempDir()
	path, err := WritePacket(dir, "", []byte(`{"packet":true}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"packet":true}` {
		t.Fatalf("stored packet = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("packet permissions = %o, want 600", info.Mode().Perm())
	}
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), ".reviewer1-packet-") || !strings.HasSuffix(path, ".json") {
		t.Fatalf("temporary packet path = %q", path)
	}
	fixed, err := WritePacket(dir, "review-packet.json", []byte(`{"fixed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if fixed != filepath.Join(dir, "review-packet.json") {
		t.Fatalf("fixed packet path = %q", fixed)
	}
	if _, err := WritePacket(dir, "review-packet.json", []byte(`{}`)); err == nil {
		t.Fatal("fixed packet path was overwritten")
	}
}

func TestPacketRedactsSensitiveNELLine(t *testing.T) {
	const lineEnding = "\u0085"
	const marker = "[[REDACTED: PRE-EXISTING OR REMOVED SENSITIVE SPAN]]"
	diff := "diff --git a/auth.go b/auth.go\n--- a/auth.go\n+++ b/auth.go\n@@ -1 +1 @@\n-token = \"ghp_1234567890abcdef1234567890\"" + lineEnding + "\n+token = \"safe\"" + lineEnding
	got := BuildReviewPacket(map[string]any{
		"objective":           "Fix parser behavior",
		"acceptance_criteria": "Handles empty input safely",
		"authority_contract":  "Read-only review; no deploy authority",
		"delegated_diff":      diff,
		"validation_summary":  "Focused validation passed",
	}, 10000)
	line, ending := splitLineEnding("-token = \"ghp_1234567890abcdef1234567890\"" + lineEnding)
	if line != "-token = \"ghp_1234567890abcdef1234567890\"" || ending != lineEnding {
		t.Fatalf("splitLineEnding() = (%q, %q), want sensitive line and U+0085 ending", line, ending)
	}
	ready, ok := got.(PacketReady)
	if !ok {
		t.Fatalf("BuildReviewPacket() = %#v, want ready", got)
	}
	want := "diff --git a/auth.go b/auth.go\n--- a/auth.go\n+++ b/auth.go\n@@ -1 +1 @@\n-token = \"" + marker + "\"" + lineEnding + "\n+token = \"safe\"" + lineEnding
	if ready.Packet["delegated_diff"] != want {
		t.Fatalf("redacted delegated diff = %q, want %q", ready.Packet["delegated_diff"], want)
	}
	if !utf8.ValidString(ready.Packet["delegated_diff"]) {
		t.Fatal("redacted delegated diff is not valid UTF-8")
	}
}

func equalPacketMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
