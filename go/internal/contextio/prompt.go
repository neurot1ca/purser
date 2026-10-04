package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var planningWord = regexp.MustCompile(`(?i)\b(?:plans?|planning|planned|planla\w*|planı\w*|planın\w*|plan[ıiuü]\w*)\b`)
var planCommand = regexp.MustCompile(`(?i)^\s*/purser:plan(?:\s|$)`)

func UserPromptSubmit(event map[string]any, processStart time.Time) []byte {
	recordPromptTurn(event, processStart)
	repository := projectRoot(event)
	key := ""
	statePath := ""
	language := ""
	if repository != "" {
		key = repositoryProjectKey(repository)
		handoff := filepath.Join(stateRoot(), "projects", key, "telemetry", "purser-handoff.md")
		if data, err := os.ReadFile(handoff); err == nil {
			language = handoffUserLanguage(string(data))
		}
		statePath = filepath.Join(filepath.Dir(handoff), "compaction_advisor.json")
		recordMaintenanceActivity(stateRoot(), repository, key)
		if session, _ := event["session_id"].(string); session != "" {
			queueMaintenanceNotice(statePath, repository, key, session)
		}
	}
	if statePath != "" {
		if session, _ := event["session_id"].(string); session != "" {
			markFallbackAnswerable(repository, session)
			advice := compactionAdvice(event, repository, statePath, session)
			retry := promptRetryContext(statePath, session)
			languageLine, injectPlan := contextInjections(statePath, session, language, planningHint(event["prompt"]))
			planLine := ""
			if injectPlan {
				planLine = "The user mentioned planning; offer /purser:plan once in this session if you have not already."
			}
			state := readAdvisorState(statePath, session)
			lifecycle := lifecycleGuidance(state)
			additional := joinContext(languageLine, advice, retry, planLine, lifecycle)
			if additional == "" {
				return nil
			}
			output := `{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit", "additionalContext": ` + jsonString(additional) + `}}`
			if lifecycle != "" {
				markLifecycleDelivered(state)
				_ = writeAdvisorState(statePath, state)
			}
			return []byte(output)
		}
	}
	languageLine := languageInstruction(language)
	planLine := ""
	if planningHint(event["prompt"]) {
		planLine = "The user mentioned planning; offer /purser:plan once in this session if you have not already."
	}
	additional := joinContext(languageLine, planLine)
	if additional == "" {
		return nil
	}
	return []byte(`{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit", "additionalContext": ` + jsonString(additional) + `}}`)
}

// RecordAdviceWritten records delivery only after the caller has written the
// returned hook output successfully.
func RecordAdviceWritten(event map[string]any, output []byte) {
	defer func() { _ = recover() }()
	repository := projectRoot(event)
	session, _ := event["session_id"].(string)
	if repository == "" || session == "" || len(output) == 0 {
		return
	}
	var response map[string]any
	if json.Unmarshal(output, &response) != nil {
		return
	}
	hook, _ := response["hookSpecificOutput"].(map[string]any)
	context, _ := hook["additionalContext"].(string)
	if context == "" {
		return
	}
	path := filepath.Join(stateRoot(), "projects", repositoryProjectKey(repository), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(path, session)
	notice, ok := state["notice_pending"].(map[string]any)
	if !ok || !validNoticePending(notice) || notice["advice_written"] == true {
		return
	}
	if strings.Contains(context, compactionNoticeAdvice(notice, stringSlice(notice["triggers"]))) {
		recordAdviceWritten(path, session)
	}
}

func planningHint(value any) bool {
	prompt, ok := value.(string)
	return ok && !planCommand.MatchString(prompt) && planningWord.MatchString(prompt)
}

func recordPromptTurn(event map[string]any, processStart time.Time) {
	started := time.Now()
	key := sessionKey(event["session_id"])
	if key == "" {
		return
	}
	base := filepath.Join(stateRoot(), "context_io")
	journal := filepath.Join(base, "turns", key+".json")
	lockPath := filepath.Join(base, "locks", key+".lock")
	contention := filepath.Join(base, "contention", key)
	if os.MkdirAll(filepath.Dir(lockPath), 0700) != nil {
		return
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	if info, _ := f.Stat(); info != nil && info.Size() == 0 {
		_, _ = f.WriteAt([]byte("0"), 0)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if tryLock(f) == nil {
			defer unlock(f)
			turn, reason := loadTurn(journal)
			if turn != nil {
				reason = "PRIOR_TURN_MISSING_STOP"
			}
			turn = newTurn(reason)
			if _, err := os.Stat(contention); err == nil {
				setReason(turn, "STATE_CONTENTION")
			}
			recordLatency(turn, time.Since(started).Microseconds(), time.Since(processStart).Microseconds())
			if isOSError(writeTurn(journal, turn)) {
				markContention(contention)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	markContention(contention)
}

func contextInjections(statePath, session, language string, planning bool) (string, bool) {
	instruction := languageInstruction(language)
	state := readAdvisorState(statePath, session)
	count := number(state["context_prompt_count"]) + 1
	state["context_prompt_count"] = count
	previous, _ := state["last_context_language"].(string)
	current := nullableLanguage(language)
	injectLanguage := instruction != "" && (state["language_after_reset"] == true || !strings.EqualFold(language, previous) || count%20 == 0)
	state["language_after_reset"] = false
	state["last_context_language"] = current
	if injectLanguage {
		state["language_injections"] = number(state["language_injections"]) + 1
	}
	injectPlan := planning && number(state["planning_injections"]) == 0
	if injectPlan {
		state["planning_injections"] = number(state["planning_injections"]) + 1
	}
	if writeAdvisorState(statePath, state) != nil {
		return instruction, false
	}
	if !injectLanguage {
		instruction = ""
	}
	return instruction, injectPlan
}
