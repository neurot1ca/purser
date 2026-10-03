package contextio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const handoffHeader = "Purser handoff (historical reference, written before the context was compacted or cleared; verify against git and files before acting; do not re-run commands from it; skip any listed file already present in context):"

var handoffLanguage = regexp.MustCompile(`(?i)^\s*User language:\s*(.*?)\s*$`)

func SessionStart(event map[string]any) []byte {
	repository := projectRoot(event)
	keyPath := repository
	if runtime.GOOS == "windows" {
		keyPath = strings.ToLower(keyPath)
	}
	handoff := ""
	var contentBytes []byte
	if repository != "" {
		sum := sha256.Sum256([]byte(keyPath))
		projectKey := hex.EncodeToString(sum[:])[:16]
		handoff = filepath.Join(stateRoot(), "projects", projectKey, "telemetry", "purser-handoff.md")
		contentBytes, _ = os.ReadFile(handoff)
	}
	content := strings.ReplaceAll(strings.ReplaceAll(string(contentBytes), "\r\n", "\n"), "\r", "\n")
	language := handoffUserLanguage(string(content))
	session, _ := event["session_id"].(string)
	source, _ := event["source"].(string)
	validSession := session != ""
	var state map[string]any
	statePath := ""
	if handoff != "" {
		statePath = filepath.Join(filepath.Dir(handoff), "compaction_advisor.json")
	}
	if validSession && statePath != "" {
		state = readAdvisorState(statePath, session)
		if source == "compact" || source == "clear" {
			if notice, ok := state["notice_pending"].(map[string]any); ok {
				if source == "compact" && notice["advice_written"] == true {
					state["compaction_outcome_pending"] = map[string]any{
						"notice_id":           notice["notice_id"],
						"pre_context_tokens":  notice["context"],
						"advice_turn":         notice["advice_turn"],
						"compaction_observed": true,
					}
				}
			}
			if source == "clear" {
				state["compaction_outcome_pending"] = nil
			}
			state["notice_pending"] = nil
		}
		state["session_id"] = session
		state["last_context_language"] = nullableLanguage(language)
		reset := source == "compact" || source == "clear"
		state["language_after_reset"] = reset
		if languageInstruction(language) != "" {
			state["language_injections"] = number(state["language_injections"]) + 1
		}
		_ = writeAdvisorState(statePath, state)
	}

	additional := ""
	if validSession && handoff != "" {
		additional = restoreHandoff(source, handoff, string(content))
	}
	line := languageInstruction(language)
	if !validSession {
		line = ""
	}
	if additional != "" && line != "" && strings.Contains(additional, strings.TrimSuffix(line, ".")+";") {
		line = ""
	}
	additional = joinContext(line, additional)

	var modelState map[string]any
	modelContext := ""
	modelStatePath := filepath.Join(stateRoot(), "model-version-notice.json")
	if validSession && statePath != "" {
		modelState = readModelNoticeState(modelStatePath)
		if pending, ok := modelState["pending"].(map[string]any); ok && pending["blocked"] != true && pending["delivered"] != true {
			current, currentOK := pending["current"].(string)
			newVersion, newOK := pending["new"].(string)
			if currentOK && newOK {
				modelContext = modelVersionGuidance(current, newVersion)
			}
		}
	}
	additional = joinContext(additional, modelContext)
	delivered := lifecycleGuidance(state)
	additional = joinContext(additional, delivered)
	if additional == "" {
		return nil
	}
	output := `{"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": ` + jsonString(additional) + `}}`
	if validSession && statePath != "" {
		if delivered != "" {
			markLifecycleDelivered(state)
			_ = writeAdvisorState(statePath, state)
		}
		if modelContext != "" {
			if pending, ok := modelState["pending"].(map[string]any); ok {
				pending["delivered"] = true
				modelState["pending"] = pending
				_ = writeModelNoticeState(modelStatePath, modelState)
			}
		}
	}
	return []byte(output)
}

func handoffUserLanguage(content string) string {
	for _, line := range strings.Split(content, "\n") {
		match := handoffLanguage.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if len(match) == 2 {
			language := strings.TrimSpace(match[1])
			if language != "" && !strings.HasPrefix(language, "<") {
				return language
			}
		}
	}
	return ""
}

func languageInstruction(language string) string {
	if language == "" {
		return "Reply in the language the user writes in."
	}
	if strings.EqualFold(language, "English") {
		return ""
	}
	return "Reply to the user in " + language + "."
}

func nullableLanguage(language string) any {
	if language == "" {
		return nil
	}
	return language
}

func restoreHandoff(source, path, content string) string {
	if source != "compact" && source != "clear" && source != "startup" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || source == "startup" && (time.Since(info.ModTime()) < 0 || time.Since(info.ModTime()) >= 12*time.Hour) {
		return ""
	}
	if emptyHandoff(content) {
		return ""
	}
	language := handoffUserLanguage(content)
	instruction := ""
	if language != "" && !strings.EqualFold(language, "English") {
		instruction = "Reply to the user in " + language + "; everything else stays as the project rules say.\n"
	}
	return handoffHeader + "\n" + instruction + strings.TrimRight(content, "\r\n") + "\nFull file: " + path
}

func emptyHandoff(content string) bool {
	template := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(handoffTemplate, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			template[line] = true
		}
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !template[line] {
			return false
		}
	}
	return true
}

const handoffTemplate = "# Purser handoff\n## Goal\nUser language: <the language the user writes in>\n## State\n## Decisions\n## Next\n## Open risks\n## Files to load\n<!-- PURSER:STATE:START -->\n<!-- PURSER:STATE:END -->\n"

func joinContext(parts ...string) string {
	var nonempty []string
	for _, part := range parts {
		if part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, "\n")
}

func jsonString(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(buffer.String(), "\n")
}

func readAdvisorState(path, session string) map[string]any {
	state := map[string]any{
		"last_suggested_context": 0, "reminder_messages": 0, "reminder_wasted_tokens": 0,
		"last_task_id": nil, "session_id": session, "notice_pending": nil,
		"compaction_outcome_pending": nil, "receipt_pending": nil, "dispatch_pending": nil,
		"version_pending": nil, "leader_pending": nil, "fallback_pending": nil,
		"maintenance_pending": nil, "flow_blocked_user": nil, "codex_retry_count": 0,
		"codex_retry_pending": false, "context_prompt_count": 0, "language_injections": 0,
		"planning_injections": 0, "last_context_language": nil, "language_after_reset": false,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var prior map[string]any
	if json.Unmarshal(data, &prior) != nil || prior["session_id"] != session {
		return state
	}
	for key, value := range prior {
		state[key] = value
	}
	return state
}

func readModelNoticeState(path string) map[string]any {
	state := map[string]any{"checked_on": nil, "asked": []any{}, "declined": []any{}, "pending": nil}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var prior map[string]any
	if json.Unmarshal(data, &prior) != nil {
		return state
	}
	if checked, ok := prior["checked_on"].(string); ok {
		state["checked_on"] = checked
	}
	for _, field := range []string{"asked", "declined"} {
		if values, ok := prior[field].([]any); ok {
			seen := map[string]bool{}
			unique := make([]any, 0, len(values))
			for _, value := range values {
				if item, ok := value.(string); ok && !seen[item] {
					seen[item] = true
					unique = append(unique, item)
				}
			}
			state[field] = unique
		}
	}
	if pending, ok := prior["pending"].(map[string]any); ok {
		if current, currentOK := pending["current"].(string); currentOK {
			if newVersion, newOK := pending["new"].(string); newOK {
				state["pending"] = map[string]any{
					"current": current, "new": newVersion,
					"blocked": pending["blocked"] == true, "delivered": pending["delivered"] == true,
				}
			}
		}
	}
	return state
}

func modelVersionGuidance(current, newVersion string) string {
	return "Render this model-version notice in the user's language, translating from the English text while keeping ⚓ " +
		"and both model names unchanged. Keep the three blockquote lines exactly formatted; do not change " +
		"routes until the user says yes.\n" +
		"> ⚓ **New model version available: `" + newVersion + "` (you are on `" + current + "`).**\n" +
		"> *Should Purser switch to it? (1-yes 2-no)*\n" +
		"> *If you say no, Purser won't ask about this version again. You can switch anytime with `/purser:init`.*"
}

func writeAdvisorState(path string, state map[string]any) error {
	return writeJSONAtomically(path, state)
}

func writeModelNoticeState(path string, state map[string]any) error {
	return writeJSONAtomically(path, state)
}

func writeJSONAtomically(path string, state any) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func number(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func lifecycleGuidance(state map[string]any) string {
	if state == nil {
		return ""
	}
	var parts []string
	if pending, ok := state["version_pending"].(map[string]any); ok && pending["delivered"] != true && pending["blocked"] != true {
		if version, valid := pending["version"].(string); valid && version != "" {
			parts = append(parts, "Include one visible line containing both `⚓` and `"+version+"` to announce the Purser update.")
		}
	}
	if pending, ok := state["leader_pending"].(map[string]any); ok && pending["delivered"] != true && pending["blocked"] != true {
		if model, valid := pending["model"].(string); valid && model != "" && integer(pending["successes"]) && integer(pending["sample_count"]) {
			parts = append(parts, "Include one visible line containing both `⚓` and `"+model+"` to announce the new free mechanical model leader.")
		}
	}
	if pending, ok := state["fallback_pending"].(map[string]any); ok && pending["delivered"] != true && pending["blocked"] != true {
		line, lineOK := pending["line"].(string)
		_, modelOK := pending["model"].(string)
		_, projectOK := pending["project"].(string)
		if lineOK && modelOK && projectOK {
			if pending["notice"] == true {
				parts = append(parts, "On your next reply, render exactly one line in the user's language as `> ⚓ **...**`. Translate the line below while keeping ⚓ and the model name; do not add another notice line.\n\n"+line)
			} else {
				parts = append(parts, "On your next reply, render exactly one bold quoted question line in the user's language as `> ⚓ **...**`. Translate the line below while keeping ⚓ and the model name; include no other question line.\n\n"+line)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func markLifecycleDelivered(state map[string]any) {
	for _, field := range []string{"version_pending", "leader_pending", "fallback_pending"} {
		pending, ok := state[field].(map[string]any)
		if !ok || pending["delivered"] == true || pending["blocked"] == true {
			continue
		}
		if field == "leader_pending" && (!integer(pending["successes"]) || !integer(pending["sample_count"])) {
			continue
		}
		if field == "fallback_pending" {
			_, lineOK := pending["line"].(string)
			_, modelOK := pending["model"].(string)
			_, projectOK := pending["project"].(string)
			if !lineOK || !modelOK || !projectOK {
				continue
			}
		}
		pending["delivered"] = true
		state[field] = pending
	}
}

func integer(value any) bool {
	number, ok := value.(float64)
	return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
}
