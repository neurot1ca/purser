package contextio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadTranscriptRecordsLargeMatchesPreviousTailBehavior(t *testing.T) {
	const limit = 4 << 20
	before := []byte("{\"id\":\"before\"}\n")
	straddling := []byte("{\"id\":\"straddling\",\"pad\":\"" + strings.Repeat("x", 1024) + "\"}\n")
	inside := []byte("{\"id\":\"inside\"}\n")
	paddingSize := limit - 512 - len(inside) - len("{\"pad\":\"\"}\n")
	data := bytes.Join([][]byte{
		before,
		straddling,
		inside,
		[]byte("{\"pad\":\"" + strings.Repeat("x", paddingSize) + "\"}\n"),
	}, nil)
	if len(data) <= limit {
		t.Fatalf("fixture size = %d, want greater than %d", len(data), limit)
	}

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	want := previousTranscriptRecords(data)
	if got := readTranscriptRecords(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("readTranscriptRecords() differs from previous tail behavior: got %d records, want %d", len(got), len(want))
	}
}

func TestReadTranscriptRecordsSmallFile(t *testing.T) {
	data := []byte("{\"id\":\"one\"}\n{\"id\":\"two\"}\n")
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := readTranscriptRecords(path), previousTranscriptRecords(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("readTranscriptRecords() = %#v, want %#v", got, want)
	}
}

func previousTranscriptRecords(data []byte) []map[string]any {
	if len(data) > 4<<20 {
		data = data[len(data)-(4<<20):]
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	var records []map[string]any
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var record map[string]any
		if json.Unmarshal(line, &record) == nil && record != nil {
			records = append(records, record)
		}
	}
	return records
}
