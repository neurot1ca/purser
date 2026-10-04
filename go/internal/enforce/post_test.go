package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPostPrimitivesMatchPythonFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/post_python_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		DecodeInput           any            `json:"decode_input"`
		DecodeExpected        []string       `json:"decode_expected"`
		DecodeFailureInput    any            `json:"decode_failure_input"`
		DecodeFailureExpected string         `json:"decode_failure_expected"`
		ReplacementField      string         `json:"replacement_field"`
		ReplacementBundle     map[string]any `json:"replacement_bundle"`
		ReplacementExpected   map[string]any `json:"replacement_expected"`
		RawValue              any            `json:"raw_value"`
		RawContents           string         `json:"raw_contents"`
		CallKeyEvent          map[string]any `json:"call_key_event"`
		CallKey               string         `json:"call_key"`
		BaseBundleJSON        string         `json:"base_bundle_json"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}

	thread, report, err := decodeCodexResult(fixture.DecodeInput)
	if err != nil || !reflect.DeepEqual([]string{thread, report}, fixture.DecodeExpected) {
		t.Fatalf("decoded result = (%q, %q, %v), want %q", thread, report, err, fixture.DecodeExpected)
	}
	if _, _, err = decodeCodexResult(fixture.DecodeFailureInput); err == nil || err.Error() != fixture.DecodeFailureExpected {
		t.Fatalf("decode failure = %v, want %q", err, fixture.DecodeFailureExpected)
	}

	gotReplacement, err := replacement(fixture.ReplacementBundle, fixture.ReplacementField)
	if err != nil || !reflect.DeepEqual(gotReplacement, fixture.ReplacementExpected) {
		t.Fatalf("replacement = %#v, %v; want Python fixture %#v", gotReplacement, err, fixture.ReplacementExpected)
	}
	if _, err = replacement(fixture.ReplacementBundle, "unsupported"); err == nil {
		t.Fatal("unsupported replacement field was accepted")
	}

	encoded, err := canonicalJSON(baseBundle(""))
	if err != nil || string(encoded) != fixture.BaseBundleJSON {
		t.Fatalf("base bundle bytes differ from Python fixture: %s (%v)", encoded, err)
	}

	key, err := callKey(fixture.CallKeyEvent)
	if err != nil || key != fixture.CallKey {
		t.Fatalf("call key = %q, %v; want %q", key, err, fixture.CallKey)
	}
	path, err := persistRawOutput(t.TempDir(), fixture.CallKeyEvent, fixture.RawValue)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != key+".json" {
		t.Fatalf("raw artifact name = %q, want %q", filepath.Base(path), key+".json")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != fixture.RawContents {
		t.Fatalf("raw artifact bytes = %q, %v; want Python bytes %q", contents, err, fixture.RawContents)
	}
}

func TestBundleFinalizationMatchesPythonFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/bundle_python_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Event     map[string]any `json:"event"`
		Ordinary  bundleFixture  `json:"ordinary"`
		Boundary  bundleFixture  `json:"boundary"`
		Mandatory bundleFixture  `json:"mandatory"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}

	checkArtifact := func(name string, expected bundleFixture) {
		t.Helper()
		bundle := expected.Bundle
		compact, path, err := finalizeBundle(t.TempDir(), fixture.Event, bundle, expected.Cap, 1200)
		if err != nil {
			t.Fatal(err)
		}
		if expected.Overflow {
			if bundle["claude_review_mode"] != "FULL" || bundle["audit_receipt"] != nil {
				t.Fatalf("%s overflow left mode=%v receipt=%v", name, bundle["claude_review_mode"], bundle["audit_receipt"])
			}
		}
		full, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pathJSON, err := json.Marshal(path)
		if err != nil {
			t.Fatal(err)
		}
		compactBytes, err := canonicalJSON(compact)
		wantCompact := strings.Replace(expected.Compact, `"__FULL_PATH__"`, string(pathJSON), 1)
		if err != nil || string(compactBytes) != wantCompact {
			t.Fatalf("%s compact bytes differ from Python fixture: %s (%v)", name, compactBytes, err)
		}
		wantFull := strings.Replace(expected.ArtifactContents, `"__FULL_PATH__"`, string(pathJSON), 1)
		if string(full) != wantFull {
			t.Fatalf("%s full artifact differs from Python fixture\n got: %s\nwant: %s", name, full, wantFull)
		}
		key, _ := callKey(fixture.Event)
		if compact["full_bundle_path"] != path || filepath.Base(path) != key+".json" {
			t.Fatalf("%s compact/full artifact identity mismatch: compact=%v path=%s", name, compact["full_bundle_path"], path)
		}
	}

	checkArtifact("ordinary", fixture.Ordinary)
	boundarySize, err := nonReportBytes(fixture.Boundary.Bundle)
	if err != nil || boundarySize != fixture.Boundary.NonReportBytes || boundarySize != fixture.Boundary.Cap {
		t.Fatalf("UTF-8 boundary bytes=%d cap=%d fixture=%d err=%v", boundarySize, fixture.Boundary.Cap, fixture.Boundary.NonReportBytes, err)
	}
	boundaryCompact, overflow, err := compactBundle(fixture.Boundary.Bundle, fixture.Boundary.Cap)
	if err != nil || overflow {
		t.Fatalf("at-limit multibyte bundle overflow=%v err=%v", overflow, err)
	}
	boundaryBytes, err := canonicalJSON(boundaryCompact)
	if err != nil || string(boundaryBytes) != fixture.Boundary.Compact {
		t.Fatalf("boundary compact differs from Python fixture: %s (%v)", boundaryBytes, err)
	}
	checkArtifact("mandatory", fixture.Mandatory)
}

type bundleFixture struct {
	Bundle           map[string]any `json:"bundle"`
	Cap              int            `json:"cap"`
	NonReportBytes   int            `json:"non_report_bytes"`
	Compact          string         `json:"compact"`
	ArtifactContents string         `json:"artifact_contents"`
	Overflow         bool           `json:"overflow"`
}
