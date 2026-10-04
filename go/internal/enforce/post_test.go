package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
