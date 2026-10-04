package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
			} `json:"expected"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._parse_task_contract" || len(fixture.Cases) == 0 {
		t.Fatalf("invalid Python fixture: %#v", fixture)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			objective, criteria, grounded := parseTaskContract(test.Prompt, test.TaskID, test.Phase, test.Scope)
			if objective != test.Expected.Objective || criteria != test.Expected.Criteria || grounded != test.Expected.Grounded {
				t.Fatalf("got (%q, %q, %t), want (%q, %q, %t)", objective, criteria, grounded, test.Expected.Objective, test.Expected.Criteria, test.Expected.Grounded)
			}
		})
	}
}
