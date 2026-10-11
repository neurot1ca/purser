package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type livenessStep struct {
	Kind  string  `json:"kind"`
	At    float64 `json:"at"`
	Bytes float64 `json:"bytes"`
	Force bool    `json:"force"`
}

type livenessOutput struct {
	Written *bool          `json:"written"`
	Job     map[string]any `json:"job"`
}

type livenessCase struct {
	Name       string           `json:"name"`
	Job        map[string]any   `json:"job"`
	Activity   LivenessActivity `json:"activity"`
	Generation int64            `json:"generation"`
	Token      string           `json:"token"`
	Steps      []livenessStep   `json:"steps"`
	Expected   []livenessOutput `json:"expected"`
}

type thresholdCase struct {
	Name     string         `json:"name"`
	Job      map[string]any `json:"job"`
	History  []float64      `json:"history"`
	Expected int            `json:"expected"`
}

func TestLivenessFixtures(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_liveness", "liveness.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read liveness oracle fixture: %v; regenerate it with PURSER_REGEN_GO_JOBS_LIVENESS=1", err)
	}
	var fixture struct {
		Cases      []livenessCase  `json:"cases"`
		Thresholds []thresholdCase `json:"thresholds"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := scratch(t)
			jobID := tc.Job["jobId"].(string)
			path := filepath.Join(root, jobID+".json")
			initial, err := json.Marshal(tc.Job)
			if err != nil {
				t.Fatal(err)
			}
			record, err := Decode(initial)
			if err != nil {
				t.Fatal(err)
			}
			if err = Write(path, record); err != nil {
				t.Fatal(err)
			}
			activity := tc.Activity
			actual := make([]livenessOutput, 0, len(tc.Steps))
			for _, step := range tc.Steps {
				var written *bool
				switch step.Kind {
				case "activity":
					activity["stdoutBytes"] = numberOr(activity["stdoutBytes"], 0) + step.Bytes
					RecordLivenessActivity(activity, step.At)
				case "restart":
					saved, readErr := Read(path)
					if readErr != nil {
						t.Fatal(readErr)
					}
					activity = NewLivenessActivity(saved, "codex", 900, step.At)
				case "checkpoint":
					result, checkpointErr := CheckpointLiveness(path, tc.Generation, tc.Token, activity,
						step.At, true, step.Force)
					if checkpointErr != nil {
						t.Fatal(checkpointErr)
					}
					written = &result
				default:
					t.Fatalf("unknown step %q", step.Kind)
				}
				saved, readErr := Read(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				savedBytes, err := saved.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				var job map[string]any
				if err = json.Unmarshal(savedBytes, &job); err != nil {
					t.Fatal(err)
				}
				actual = append(actual, livenessOutput{Written: written, Job: job})
			}
			if !reflect.DeepEqual(actual, tc.Expected) {
				got, _ := json.MarshalIndent(actual, "", "  ")
				want, _ := json.MarshalIndent(tc.Expected, "", "  ")
				t.Fatalf("liveness replay differs\ngot: %s\nwant: %s", got, want)
			}
		})
	}
	for _, tc := range fixture.Thresholds {
		t.Run("threshold_"+tc.Name, func(t *testing.T) {
			root := scratch(t)
			for index, gap := range tc.History {
				history := map[string]any{
					"jobId": string(rune('a'+index)) + string(rune('a'+index)),
					"state": "SUCCEEDED", "route": "implementation", "activeAgent": "codex",
					"liveness": map[string]any{"agent": "codex", "longestSilentGapSeconds": gap},
				}
				bytes, _ := json.Marshal(history)
				record, decodeErr := Decode(bytes)
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if writeErr := Write(filepath.Join(root, string(rune('a'+index))+".json"), record); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			bytes, _ := json.Marshal(tc.Job)
			job, err := Decode(bytes)
			if err != nil {
				t.Fatal(err)
			}
			if got := StallThreshold(root, job); got != tc.Expected {
				t.Fatalf("threshold %d, want %d", got, tc.Expected)
			}
		})
	}
}
