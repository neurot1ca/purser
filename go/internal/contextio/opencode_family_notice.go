package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"purser-hook/internal/obsidian"
	"purser-hook/internal/profile"
)

var openCodeFamilyVersionPattern = regexp.MustCompile(`^(.+?)(\d+(?:\.\d+)*)([^0-9]*)$`)

func mechanicalRaceContext() string {
	root := stateRoot()
	path := filepath.Join(root, "opencode", "candidates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var inventory struct {
		Race       map[string]any   `json:"race"`
		Bootstrap  map[string]any   `json:"bootstrap"`
		Candidates []map[string]any `json:"candidates"`
	}
	if json.Unmarshal(data, &inventory) != nil {
		return ""
	}
	if inventory.Bootstrap["status"] == "started" {
		started, _ := inventory.Bootstrap["started_at"].(string)
		id := "bootstrap-start:" + started
		if started != "" && claimInventoryNotice(root, id, "bootstrap") {
			n, _ := inventory.Bootstrap["entrants"].([]any)
			return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, and numbers unchanged, in this form: > ⚓ **Model race started:** at setup you chose free OpenCode models for mechanical tasks; Purser is racing all " + itoa(len(n)) + " of them in the background and will report the winner."
		}
	}
	if inventory.Bootstrap["status"] == "finished" {
		winner := textValue(inventory.Bootstrap["winner"])
		finished := textValue(inventory.Bootstrap["finished_at"])
		results, _ := inventory.Bootstrap["results"].(map[string]any)
		values, _ := results[winner].([]any)
		if winner != "" && finished != "" && len(values) > 0 && claimInventoryNotice(root, "bootstrap-finish:"+finished, "bootstrap") {
			successes := 0
			for _, value := range values {
				if value == true {
					successes++
				}
			}
			return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, and numbers unchanged, in this form: > ⚓ **Model race finished:** `" + winner + "` won with " + itoa(successes) + " of " + itoa(len(values)) + " tasks; mechanical tasks will go to it first."
		}
	}
	if inventory.Race["started"] != true || inventory.Race["finished"] == true {
		return ""
	}
	started, _ := inventory.Race["started_at"].(string)
	entrants, _ := inventory.Race["entrants"].([]any)
	if started == "" || len(entrants) == 0 {
		return ""
	}
	models := stringList(entrants)
	if len(models) == 2 && inventory.Race["stage"] == "newcomer_vs_leader" {
		if !claimInventoryNotice(root, "race-start:"+started, "race") {
			return ""
		}
		return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, and model names unchanged, in this form: > ⚓ **Model race started:** OpenCode added `" + models[0] + "`; it is racing `" + models[1] + "` on shadow runs for mechanical tasks."
	}
	if len(models) > 1 {
		leader := ""
		for _, candidate := range inventory.Candidates {
			if candidate["status"] == "measured" {
				leader = textValue(candidate["id"])
				break
			}
		}
		if leader == "" {
			return ""
		}
		if !claimInventoryNotice(root, "race-start:"+started, "race") {
			return ""
		}
		return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, and model names unchanged, in this form: > ⚓ **Model race started:** OpenCode added `" + strings.Join(models, "`, `") + "`; they race each other first, then the winner races `" + leader + "` on shadow runs for mechanical tasks."
	}
	return ""
}

func openCodeFamilyContext(repository string) string {
	root := stateRoot()
	var cache struct {
		Snapshot string              `json:"opencode_snapshot_on"`
		Agents   map[string][]string `json:"agents"`
	}
	data, err := os.ReadFile(filepath.Join(root, "model-resolution.json"))
	if err != nil || json.Unmarshal(data, &cache) != nil || cache.Snapshot == "" || len(cache.Agents["opencode"]) == 0 {
		return ""
	}
	families := make(map[string]bool)
	for _, id := range cache.Agents["opencode"] {
		families[openCodeFamily(id)] = true
	}
	if len(families) == 0 {
		return ""
	}
	path := filepath.Join(root, "opencode", "family-notices.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return ""
	}
	defer unlock()
	state := map[string]any{}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if json.Unmarshal(raw, &state) != nil || state == nil {
			return ""
		}
	} else if !os.IsNotExist(readErr) {
		return ""
	}
	seen, _ := state["families"].([]any)
	known := map[string]bool{}
	for _, raw := range seen {
		if value, ok := raw.(string); ok {
			known[value] = true
		}
	}
	ordered := make([]string, 0, len(families))
	for family := range families {
		ordered = append(ordered, family)
	}
	sort.Strings(ordered)
	if state["baseline"] != true {
		state["baseline"] = true
		state["families"] = stringAnyList(ordered)
		if writeJSONAtomically(path, state) != nil {
			return ""
		}
		return ""
	}
	newFamily := ""
	for _, family := range ordered {
		if !known[family] {
			newFamily = family
			break
		}
	}
	if newFamily == "" {
		return ""
	}
	roles := openCodeRoles(repository)
	if len(roles) == 0 {
		return ""
	}
	updated := append(orderedKnown(known), newFamily)
	sort.Strings(updated)
	state["families"] = stringAnyList(updated)
	if writeJSONAtomically(path, state) != nil {
		return ""
	}
	return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, and numbers unchanged, in this form: > ⚓ **New OpenCode model family:** Purser found `" + newFamily + "`; it can be used for `" + strings.Join(roles, "`, `") + "`. Your configured models have not changed."
}

func openCodeFamily(id string) string {
	match := openCodeFamilyVersionPattern.FindStringSubmatch(id)
	if len(match) == 0 {
		return id
	}
	return strings.TrimRight(match[1], "-_.")
}

func openCodeRoles(repository string) []string {
	roles := []string{}
	configData, err := os.ReadFile(filepath.Join(obsidian.PluginRoot(), ".claude", "hooks", "orchestrator", "config.json"))
	var config map[string]any
	if err == nil {
		_ = json.Unmarshal(configData, &config)
	}
	userPath := ""
	if home, e := os.UserHomeDir(); e == nil {
		userPath = filepath.Join(home, ".orchestrator", "config.toml")
	}
	user := map[string]any{}
	if raw, e := os.ReadFile(userPath); e == nil {
		user, _, _ = profile.DecodeTOML(raw)
	}
	if repository != "" {
		rolePath := filepath.Join(stateRoot(), "projects", repositoryProjectKey(repository), "roles.toml")
		if raw, e := os.ReadFile(rolePath); e == nil {
			if project, _, decodeErr := profile.DecodeTOML(raw); decodeErr == nil {
				for _, key := range []string{"roles", "coding", "reviewer", "fallback"} {
					if value, ok := project[key]; ok {
						user[key] = value
					}
				}
			}
		}
	}
	rolesTable, _ := user["roles"].(map[string]any)
	coder := textValue(rolesTable["coder"])
	if coder == "" {
		coding, _ := user["coding"].(map[string]any)
		coder = textValue(coding["coder_agent"])
	}
	if coder == "" {
		coder = textValue(config["coder_agent"])
	}
	if coder == "opencode" {
		roles = append(roles, "coder")
	}
	reviewer := ""
	if selected, ok := user["reviewer"].(map[string]any); ok {
		if item, ok := selected["selected"].(map[string]any); ok {
			reviewer = textValue(item["agent"])
		}
	}
	if reviewer == "" {
		coding, _ := user["coding"].(map[string]any)
		reviewer = textValue(coding["reviewer1_agent"])
	}
	if reviewer == "" {
		reviewer = textValue(config["reviewer1_agent"])
	}
	if reviewer == "opencode" && textValue(rolesTable["reviewer"]) != "none" {
		roles = append(roles, "Reviewer-1")
	}
	fallback, _ := user["fallback"].(map[string]any)
	if textValue(fallback["agent"]) == "opencode" {
		roles = append(roles, "fallback")
	}
	return roles
}

func claimActiveRaceResult(root, model string) bool {
	path := filepath.Join(root, "opencode", "candidates.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var inventory map[string]any
	if json.Unmarshal(data, &inventory) != nil {
		return false
	}
	race, _ := inventory["race"].(map[string]any)
	if race == nil || race["finished"] != true || race["winner"] != model {
		return false
	}
	candidates, _ := inventory["candidates"].([]any)
	active := false
	for _, raw := range candidates {
		c, _ := raw.(map[string]any)
		if c["id"] == model && c["status"] == "measured" {
			active = true
		}
	}
	if !active {
		return false
	}
	if measuredCandidateLeader(candidates) != model {
		return false
	}
	finished, _ := race["finished_at"].(string)
	if finished == "" {
		return false
	}
	return claimRaceID(root, race, "race-result:"+finished)
}

func measuredCandidateLeader(candidates []any) string {
	for _, raw := range candidates {
		if candidate, ok := raw.(map[string]any); ok && candidate["status"] == "measured" {
			return textValue(candidate["id"])
		}
	}
	return ""
}

func claimInventoryNotice(root, id, section string) bool {
	path := filepath.Join(root, "opencode", "candidates.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var inventory map[string]any
	if json.Unmarshal(data, &inventory) != nil {
		return false
	}
	var record map[string]any
	if section == "race" {
		record, _ = inventory["race"].(map[string]any)
	} else {
		record, _ = inventory["bootstrap"].(map[string]any)
	}
	if record == nil {
		return false
	}
	return claimRaceID(root, record, id)
}

func claimRaceID(root string, record map[string]any, id string) bool {
	claims, _ := record["notice_claim_ids"].([]any)
	for _, existing := range claims {
		if existing == id {
			return false
		}
	}
	claims = append(claims, id)
	record["notice_claim_ids"] = claims
	path := filepath.Join(root, "opencode", "candidates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var inventory map[string]any
	if json.Unmarshal(data, &inventory) != nil {
		return false
	}
	// Replace the target record with the claimed copy before the atomic write.
	if record["started"] != nil {
		inventory["race"] = record
	} else {
		inventory["bootstrap"] = record
	}
	encoded, err := json.Marshal(inventory)
	if err != nil || atomicWriteContext(path, encoded) != nil {
		return false
	}
	return true
}

func atomicWriteContext(path string, data []byte) error {
	return writeJSONAtomically(path, json.RawMessage(data))
}

func orderedKnown(known map[string]bool) []string {
	values := make([]string, 0, len(known))
	for value := range known {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func stringAnyList(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func stringList(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if id, ok := value.(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

func itoa(value int) string { return strconv.Itoa(value) }
