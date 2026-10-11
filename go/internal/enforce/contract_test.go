package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskContractRequirementsLabelAndMissingCriteria(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		wantCrit   string
		wantGround bool
	}{
		{
			name:       "requirements label stops before validation",
			prompt:     "Goal: Update parser\nRequirements:\n- a\n- b\nValidation: run focused tests",
			wantCrit:   "a; b",
			wantGround: true,
		},
		{
			name:       "required change label collects numbered criteria",
			prompt:     "Goal: Update parser\nVerified context: Existing brief\nRequired change:\n1. Parse required changes\n2. Preserve JSON behavior\nTests: run focused tests",
			wantCrit:   "Parse required changes; Preserve JSON behavior",
			wantGround: true,
		},
		{
			name:       "without criteria label stays ungrounded",
			prompt:     "Goal: Update parser\n- a\n- b\nValidation: run focused tests",
			wantCrit:   "Verify correct implementation within declared scope () with no material regression or scope expansion.",
			wantGround: false,
		},
		{
			name:       "goal only stays ungrounded",
			prompt:     "Goal: Update parser",
			wantCrit:   "Verify correct implementation within declared scope () with no material regression or scope expansion.",
			wantGround: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, criteria, grounded := parseTaskContract(test.prompt, "requirements", "implementation", nil)
			if criteria != test.wantCrit || grounded != test.wantGround {
				t.Fatalf("got (%q, %t), want (%q, %t)", criteria, grounded, test.wantCrit, test.wantGround)
			}
		})
	}
}

func TestTaskContractMatchesPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "task_contract.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string   `json:"name"`
			Prompt   string   `json:"prompt"`
			TaskID   string   `json:"task_id"`
			Phase    string   `json:"phase"`
			Scope    []string `json:"scope"`
			Expected struct {
				Objective string `json:"objective"`
				Criteria  string `json:"criteria"`
				Grounded  bool   `json:"grounded"`
				TaskType  string `json:"task_type"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._parse_task_contract+codex._derive_task_type" || len(fixture.Cases) == 0 {
		t.Fatalf("invalid Python fixture: %#v", fixture)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			objective, criteria, grounded := parseTaskContract(test.Prompt, test.TaskID, test.Phase, test.Scope)
			taskType := deriveTaskType(objective, criteria, test.Phase)
			if objective != test.Expected.Objective || criteria != test.Expected.Criteria || grounded != test.Expected.Grounded || taskType != test.Expected.TaskType {
				t.Fatalf("got (%q, %q, %t, %q), want (%q, %q, %t, %q)", objective, criteria, grounded, taskType, test.Expected.Objective, test.Expected.Criteria, test.Expected.Grounded, test.Expected.TaskType)
			}
		})
	}
}
