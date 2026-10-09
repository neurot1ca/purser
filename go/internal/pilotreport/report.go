package pilotreport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Report struct {
	IDs                     IDs        `json:"ids"`
	Models                  Models     `json:"models"`
	Route                   any        `json:"route"`
	Boundaries              Boundaries `json:"boundaries"`
	Usage                   Usage      `json:"usage"`
	PromptResponseSeconds   []float64  `json:"prompt_response_seconds"`
	APIEquivalentUSD        *ArmCosts  `json:"api_equivalent_usd"`
	LifecycleBlockedSeconds *float64   `json:"lifecycle_blocked_seconds"`
	Incomplete              []string   `json:"incomplete"`
}
type Prices struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"`
}
type ArmCosts struct {
	Main       *float64 `json:"main"`
	Dispatcher *float64 `json:"dispatcher"`
}

type IDs struct {
	Job             string  `json:"job"`
	Task            *string `json:"task"`
	Session         *string `json:"session"`
	DispatcherAgent *string `json:"dispatcher_agent_id"`
}
type Models struct {
	Worker     any      `json:"worker"`
	Effort     any      `json:"effort"`
	Main       []string `json:"claude_main"`
	Dispatcher []string `json:"claude_dispatcher"`
}
type Boundaries struct {
	Started                 *string  `json:"started"`
	Finished                *string  `json:"finished"`
	Reviewer1Started        *string  `json:"reviewer1_started"`
	Reviewer1Finished       *string  `json:"reviewer1_finished"`
	Acceptance              *string  `json:"acceptance"`
	Applied                 *string  `json:"applied"`
	FinalizedLatencySeconds *float64 `json:"finalized_latency_seconds"`
	WorkerSeconds           *float64 `json:"worker_seconds"`
	ToAcceptanceSeconds     *float64 `json:"to_acceptance_seconds"`
}
type Usage struct {
	Main              *Tokens `json:"main"`
	Dispatcher        *Tokens `json:"dispatcher"`
	CoordinationTurns *int    `json:"coordination_turns"`
}
type Tokens struct {
	Input         *int64         `json:"input_tokens"`
	Output        *int64         `json:"output_tokens"`
	CacheCreation *CacheCreation `json:"cache_creation"`
	CacheRead     *int64         `json:"cache_read"`
}
type CacheCreation struct {
	Ephemeral5m *int64 `json:"ephemeral_5m"`
	Ephemeral1h *int64 `json:"ephemeral_1h"`
}

// Build reads only the supplied state and Claude projects roots, making the report deterministic and testable.
func Build(stateRoot, projectsRoot, jobID string) Report {
	return BuildWithPrices(stateRoot, projectsRoot, jobID, nil)
}

func BuildWithPrices(stateRoot, projectsRoot, jobID string, prices map[string]Prices) Report {
	r := Report{IDs: IDs{Job: jobID}, Models: Models{Main: []string{}, Dispatcher: []string{}}, PromptResponseSeconds: []float64{}, Incomplete: []string{}}
	jobPath := filepath.Join(stateRoot, "codex_exec_mcp", "jobs", jobID+".json")
	job, err := readObject(jobPath)
	if err != nil {
		r.Incomplete = append(r.Incomplete, "job_record: "+err.Error())
		return r
	}
	task := stringValue(job["taskId"])
	r.IDs.Task = optionalString(task)
	r.Models.Worker, r.Models.Effort, r.Route = job["model"], job["effort"], job["route"]
	r.Boundaries.Started = timestamp(job["startedAt"])
	r.Boundaries.Finished = timestamp(job["finishedAt"])
	applyState := stringValue(job["applyState"])
	if applyState == "applied" {
		r.Boundaries.Applied = timestamp(job["appliedAt"])
	}
	if acceptance, ok := job["acceptance"].(map[string]any); ok {
		r.Boundaries.Acceptance = timestamp(acceptance["acceptedAt"])
	}
	if task == "" {
		r.Incomplete = append(r.Incomplete, "job_task_id_missing")
	}
	if r.Boundaries.Started == nil {
		r.Incomplete = append(r.Incomplete, "job_started_at_missing")
	}

	if task != "" {
		path := filepath.Join(stateRoot, "telemetry", "events", "by_task", task+".jsonl")
		events, readErr := readLines(path)
		if readErr != nil {
			r.Incomplete = append(r.Incomplete, "telemetry: "+readErr.Error())
		} else {
			for _, event := range events {
				switch stringValue(event["event_type"]) {
				case "IMPLEMENTER_STARTED":
					if r.Boundaries.Started == nil {
						r.Boundaries.Started = timestamp(event["ts_utc"])
					}
				case "IMPLEMENTER_FINISHED":
					r.Boundaries.Finished = firstTimestamp(r.Boundaries.Finished, event["ts_utc"])
				case "REVIEWER1_STARTED":
					r.Boundaries.Reviewer1Started = firstTimestamp(r.Boundaries.Reviewer1Started, event["ts_utc"])
				case "REVIEWER1_FINISHED":
					r.Boundaries.Reviewer1Finished = firstTimestamp(r.Boundaries.Reviewer1Finished, event["ts_utc"])
				case "CLAUDE_DECISION_RECORDED":
					r.Boundaries.Acceptance = firstTimestamp(r.Boundaries.Acceptance, event["ts_utc"])
				}
				if r.IDs.DispatcherAgent == nil && stringValue(event["agent_id"]) != "" {
					r.IDs.DispatcherAgent = optionalString(stringValue(event["agent_id"]))
				}
				if r.IDs.Session == nil && stringValue(event["session_id"]) != "" {
					r.IDs.Session = optionalString(stringValue(event["session_id"]))
				}
			}
		}
	}
	if r.IDs.Session == nil {
		r.Incomplete = append(r.Incomplete, "telemetry_session_id_missing")
	}
	if r.Boundaries.Started == nil {
		r.Incomplete = appendUnique(r.Incomplete, "job_started_at_missing")
	}
	r.Boundaries.WorkerSeconds = elapsed(r.Boundaries.Started, r.Boundaries.Finished)
	r.Boundaries.ToAcceptanceSeconds = elapsed(r.Boundaries.Started, r.Boundaries.Acceptance)
	if r.IDs.DispatcherAgent == nil && r.IDs.Session != nil {
		if mainPath, locateErr := locateSession(projectsRoot, *r.IDs.Session); locateErr == nil {
			paths, _ := filepath.Glob(filepath.Join(filepath.Dir(mainPath), *r.IDs.Session, "subagents", "agent-*.jsonl"))
			sort.Strings(paths)
			for _, path := range paths {
				records, readErr := readLines(path)
				if readErr != nil || !hasCodexStart(records, jobID, task) {
					continue
				}
				name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
				r.IDs.DispatcherAgent = optionalString(name)
				break
			}
		}
	}
	finalized := r.Boundaries.Finished
	if applyState == "applied" {
		finalized = r.Boundaries.Applied
	}
	if r.Boundaries.Started != nil && finalized != nil {
		start, e1 := time.Parse(time.RFC3339Nano, *r.Boundaries.Started)
		end, e2 := time.Parse(time.RFC3339Nano, *finalized)
		if e1 == nil && e2 == nil {
			v := end.Sub(start).Seconds()
			r.Boundaries.FinalizedLatencySeconds = &v
		}
	}

	if r.IDs.Session != nil {
		mainPath, locateErr := locateSession(projectsRoot, *r.IDs.Session)
		if locateErr != nil {
			r.Incomplete = append(r.Incomplete, "main_transcript: "+locateErr.Error())
		} else {
			records, readErr := readLines(mainPath)
			if readErr != nil {
				r.Incomplete = append(r.Incomplete, "main_transcript: "+readErr.Error())
			} else {
				usage, turns, models, blocked := analyzeMain(records, jobID, task)
				r.Usage.Main, r.Usage.CoordinationTurns = usage, &turns
				r.Models.Main = models
				r.LifecycleBlockedSeconds = &blocked
				r.PromptResponseSeconds = promptResponses(records, r.Boundaries.Started, finalized, &r.Incomplete)
			}
		}
	} else {
		r.Incomplete = append(r.Incomplete, "main_transcript_session_unavailable")
	}
	if r.IDs.Session != nil && r.IDs.DispatcherAgent != nil {
		mainPath, locateErr := locateSession(projectsRoot, *r.IDs.Session)
		var records []map[string]any
		var readErr error
		if locateErr == nil {
			records, readErr = readLines(filepath.Join(filepath.Dir(mainPath), *r.IDs.Session, "subagents", "agent-"+*r.IDs.DispatcherAgent+".jsonl"))
		}
		if locateErr != nil || readErr != nil {
			cause := readErr
			if locateErr != nil {
				cause = locateErr
			}
			r.Incomplete = append(r.Incomplete, "dispatcher_transcript: "+cause.Error())
		} else {
			usage, models := analyzeDispatcher(records, jobID, task)
			r.Usage.Dispatcher, r.Models.Dispatcher = usage, models
		}
	} else {
		r.Incomplete = append(r.Incomplete, "dispatcher_identity_unavailable")
	}
	sort.Strings(r.Models.Main)
	sort.Strings(r.Models.Dispatcher)
	if prices != nil {
		r.APIEquivalentUSD = &ArmCosts{
			Main:       armCost("main", r.Models.Main, r.Usage.Main, prices, &r.Incomplete),
			Dispatcher: armCost("dispatcher", r.Models.Dispatcher, r.Usage.Dispatcher, prices, &r.Incomplete),
		}
	}
	return r
}

func armCost(arm string, models []string, usage *Tokens, prices map[string]Prices, incomplete *[]string) *float64 {
	if len(models) > 1 {
		*incomplete = appendUnique(*incomplete, arm+"_cost_multiple_models")
		return nil
	}
	if len(models) == 0 {
		*incomplete = appendUnique(*incomplete, arm+"_cost_model_unknown")
		return nil
	}
	price, ok := prices[models[0]]
	if !ok {
		*incomplete = appendUnique(*incomplete, arm+"_price_missing")
		return nil
	}
	if usage == nil || usage.Input == nil || usage.Output == nil || usage.CacheRead == nil || usage.CacheCreation == nil || usage.CacheCreation.Ephemeral5m == nil || usage.CacheCreation.Ephemeral1h == nil {
		*incomplete = appendUnique(*incomplete, arm+"_usage_unknown")
		return nil
	}
	total := (float64(*usage.Input)*price.Input + float64(*usage.Output)*price.Output +
		float64(*usage.CacheCreation.Ephemeral5m)*price.CacheWrite5m +
		float64(*usage.CacheCreation.Ephemeral1h)*price.CacheWrite1h +
		float64(*usage.CacheRead)*price.CacheRead) / 1_000_000
	return &total
}

func elapsed(started, ended *string) *float64 {
	if started == nil || ended == nil {
		return nil
	}
	start, e1 := time.Parse(time.RFC3339Nano, *started)
	end, e2 := time.Parse(time.RFC3339Nano, *ended)
	if e1 != nil || e2 != nil {
		return nil
	}
	seconds := end.Sub(start).Seconds()
	return &seconds
}

func promptResponses(records []map[string]any, started, finalized *string, incomplete *[]string) []float64 {
	responses := []float64{}
	if started == nil || finalized == nil {
		return responses
	}
	start, err1 := time.Parse(time.RFC3339Nano, *started)
	end, err2 := time.Parse(time.RFC3339Nano, *finalized)
	if err1 != nil || err2 != nil {
		return responses
	}
	for i, record := range records {
		if !genuinePrompt(record) {
			continue
		}
		prompt, ok := transcriptTime(record)
		if !ok || prompt.Before(start) || prompt.After(end) {
			continue
		}
		found := false
		for _, reply := range records[i+1:] {
			if !assistant(reply) {
				continue
			}
			message, _ := reply["message"].(map[string]any)
			if message["stop_reason"] != "end_turn" {
				continue
			}
			replyTime, valid := transcriptTime(reply)
			if valid && replyTime.After(prompt) {
				responses = append(responses, replyTime.Sub(prompt).Seconds())
				found = true
				break
			}
		}
		if !found {
			*incomplete = appendUnique(*incomplete, "prompt_response_missing")
		}
	}
	return responses
}

func genuinePrompt(record map[string]any) bool {
	if record["type"] != "user" || record["isMeta"] == true || record["isCompactSummary"] == true {
		return false
	}
	m, _ := record["message"].(map[string]any)
	switch value := m["content"].(type) {
	case string:
		return strings.TrimSpace(value) != "" && !strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "<task-notification>") && !strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "[SYSTEM NOTIFICATION - NOT USER INPUT]")
	case []any:
		text := false
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_result" {
				return false
			}
			if block["type"] == "text" && strings.TrimSpace(stringValue(block["text"])) != "" {
				text = true
			}
		}
		return text
	default:
		return false
	}
}

func transcriptTime(record map[string]any) (time.Time, bool) {
	stamp := stringValue(record["timestamp"])
	if stamp == "" {
		m, _ := record["message"].(map[string]any)
		stamp = stringValue(m["timestamp"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	return parsed, err == nil
}

func locateSession(root, session string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", session+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("session transcript not found")
	}
	if len(paths) > 1 {
		return "", fmt.Errorf("session transcript is ambiguous")
	}
	return paths[0], nil
}
func readObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("expected object")
	}
	return v, nil
}
func readLines(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []map[string]any
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 16*1024*1024)
	for scan.Scan() {
		line := scan.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal(line, &value); err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("expected JSON object line")
		}
		out = append(out, value)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func stringValue(v any) string { s, _ := v.(string); return s }
func timestamp(v any) *string {
	var t time.Time
	switch x := v.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			return nil
		}
		t = parsed
	case float64:
		t = time.Unix(0, int64(x*float64(time.Second)))
	case json.Number:
		n, err := x.Float64()
		if err != nil {
			return nil
		}
		t = time.Unix(0, int64(n*float64(time.Second)))
	default:
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}
func firstTimestamp(current *string, value any) *string {
	if current != nil {
		return current
	}
	return timestamp(value)
}
func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

func assistant(record map[string]any) bool {
	m, _ := record["message"].(map[string]any)
	role := stringValue(m["role"])
	if role == "" {
		role = stringValue(record["type"])
	}
	return role == "assistant"
}
func content(record map[string]any) []any {
	m, _ := record["message"].(map[string]any)
	blocks, _ := m["content"].([]any)
	return blocks
}
func toolIdentity(block map[string]any) string {
	if id := stringValue(block["id"]); id != "" {
		return id
	}
	return stringValue(block["tool_use_id"])
}
func matchesJob(value any, jobID, taskID string) bool {
	switch x := value.(type) {
	case string:
		return strings.Contains(x, jobID) || taskID != "" && strings.Contains(x, taskID)
	case map[string]any:
		for _, v := range x {
			if matchesJob(v, jobID, taskID) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if matchesJob(v, jobID, taskID) {
				return true
			}
		}
	}
	return false
}
func recordID(record map[string]any) string {
	m, _ := record["message"].(map[string]any)
	if id := stringValue(m["id"]); id != "" {
		return id
	}
	return stringValue(record["requestId"])
}
func requestIDs(record map[string]any) []string {
	ids := []string{}
	if id := stringValue(record["requestId"]); id != "" {
		ids = append(ids, id)
	}
	if m, _ := record["message"].(map[string]any); stringValue(m["id"]) != "" {
		id, found := stringValue(m["id"]), false
		for _, existing := range ids {
			found = found || existing == id
		}
		if !found {
			ids = append(ids, id)
		}
	}
	return ids
}
func model(record map[string]any) string {
	m, _ := record["message"].(map[string]any)
	return stringValue(m["model"])
}
func analyzeMain(records []map[string]any, jobID, taskID string) (*Tokens, int, []string, float64) {
	matchedTools := map[string]bool{}
	matchedCalls := map[string]time.Time{}
	names := map[string]string{}
	assistantForTool := map[string]map[string]any{}
	modelSet := map[string]bool{}
	for i, rec := range records {
		if !assistant(rec) {
			continue
		}
		if m := model(rec); m != "" {
			modelSet[m] = true
		}
		for _, raw := range content(rec) {
			b, ok := raw.(map[string]any)
			if !ok || b["type"] != "tool_use" {
				continue
			}
			id := toolIdentity(b)
			names[id] = strings.ToLower(stringValue(b["name"]))
			if id == "" {
				id = fmt.Sprintf("line:%d", i)
			}
			assistantForTool[id] = rec
			if matchesJob(b["input"], jobID, taskID) {
				matchedTools[id] = true
			}
		}
	}
	resultEnds := map[string]time.Time{}
	for _, rec := range records {
		if assistant(rec) {
			continue
		}
		for _, raw := range content(rec) {
			b, ok := raw.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			id := toolIdentity(b)
			if matchesJob(b["content"], jobID, taskID) {
				matchedTools[id] = true
			}
			if ts := timestamp(rec["timestamp"]); ts != nil {
				end, _ := time.Parse(time.RFC3339Nano, *ts)
				resultEnds[id] = end
			}
		}
	}
	matchedAssistant := map[string]map[string]any{}
	for id := range matchedTools {
		rec, ok := assistantForTool[id]
		if !ok {
			continue
		}
		key := recordID(rec)
		if key == "" {
			key = id
		}
		matchedAssistant[key] = rec
		if strings.Contains(names[id], "codex") {
			if ts := timestamp(rec["timestamp"]); ts != nil {
				start, _ := time.Parse(time.RFC3339Nano, *ts)
				if end, exists := resultEnds[id]; exists && end.After(start) {
					matchedCalls[id] = start
				}
			}
		}
	}
	// Tool results occur on user records; collect durations by pairing their IDs with matched Purser calls.
	var blocked float64
	for _, rec := range records {
		if assistant(rec) {
			continue
		}
		for _, raw := range content(rec) {
			b, ok := raw.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			id := toolIdentity(b)
			if start, ok := matchedCalls[id]; ok && matchesJob(b["content"], jobID, taskID) {
				if ts := timestamp(rec["timestamp"]); ts != nil {
					end, _ := time.Parse(time.RFC3339Nano, *ts)
					if end.After(start) {
						blocked += end.Sub(start).Seconds()
					}
				}
			}
		}
	}
	selected := dedupeAssistant(values(matchedAssistant))
	usage := sumUsage(selected)
	turns := len(selected)
	return usage, turns, keys(modelSet), blocked
}

func analyzeDispatcher(records []map[string]any, jobID, taskID string) (*Tokens, []string) {
	start, end := -1, len(records)
	for i, rec := range records {
		if codexStartFor(rec, jobID, taskID) || matchesJob(rec, jobID, taskID) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, []string{}
	}
	for i := start + 1; i < len(records); i++ {
		if !codexStartFor(records[i], "", "") {
			continue
		}
		input := codexStartInput(records[i])
		otherJob, otherTask := stringValue(input["jobId"]), stringValue(input["taskId"])
		if otherJob != jobID && otherTask != taskID {
			end = i
			break
		}
	}
	return analyzeWhole(records[start:end])
}

func analyzeWhole(records []map[string]any) (*Tokens, []string) {
	selected := []map[string]any{}
	models := map[string]bool{}
	for _, rec := range records {
		if !assistant(rec) {
			continue
		}
		if m := model(rec); m != "" {
			models[m] = true
		}
		selected = append(selected, rec)
	}
	return sumUsage(dedupeAssistant(selected)), keys(models)
}

func dedupeAssistant(records []map[string]any) []map[string]any {
	seen := map[string]bool{}
	selected := make([]map[string]any, 0, len(records))
	for i, rec := range records {
		ids := requestIDs(rec)
		duplicate := false
		for _, id := range ids {
			duplicate = duplicate || seen[id]
		}
		if duplicate {
			continue
		}
		if len(ids) == 0 {
			ids = []string{fmt.Sprintf("line:%d", i)}
		}
		for _, id := range ids {
			seen[id] = true
		}
		selected = append(selected, rec)
	}
	return selected
}

func codexStartInput(record map[string]any) map[string]any {
	for _, raw := range content(record) {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_use" || !strings.Contains(strings.ToLower(stringValue(block["name"])), "codex_start") {
			continue
		}
		if input, ok := block["input"].(map[string]any); ok {
			return input
		}
	}
	return nil
}

func codexStartFor(record map[string]any, jobID, taskID string) bool {
	input := codexStartInput(record)
	if input == nil {
		return false
	}
	if jobID == "" && taskID == "" {
		return true
	}
	return jobID != "" && stringValue(input["jobId"]) == jobID || taskID != "" && stringValue(input["taskId"]) == taskID
}

func hasCodexStart(records []map[string]any, jobID, taskID string) bool {
	for _, record := range records {
		if codexStartFor(record, jobID, taskID) {
			return true
		}
	}
	return false
}
func values(m map[string]map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for _, rec := range m {
		out = append(out, rec)
	}
	return out
}
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k == "<synthetic>" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func sumUsage(records []map[string]any) *Tokens {
	if len(records) == 0 {
		return nil
	}
	result := &Tokens{}
	var in, out, read, c5, c1 int64
	hasIn, hasOut, hasRead, has5, has1 := false, false, false, false, false
	for _, rec := range records {
		m, _ := rec["message"].(map[string]any)
		usage, _ := m["usage"].(map[string]any)
		in += token(usage, "input_tokens", &hasIn)
		out += token(usage, "output_tokens", &hasOut)
		read += token(usage, "cache_read_input_tokens", &hasRead)
		if ttl, ok := usage["cache_creation"].(map[string]any); ok {
			c5 += token(ttl, "ephemeral_5m_input_tokens", &has5)
			c1 += token(ttl, "ephemeral_1h_input_tokens", &has1)
		} else {
			c5 += token(usage, "ephemeral_5m_input_tokens", &has5)
			c1 += token(usage, "ephemeral_1h_input_tokens", &has1)
		}
	}
	if hasIn {
		result.Input = &in
	}
	if hasOut {
		result.Output = &out
	}
	if hasRead {
		result.CacheRead = &read
	}
	if has5 || has1 {
		result.CacheCreation = &CacheCreation{}
		if has5 {
			result.CacheCreation.Ephemeral5m = &c5
		}
		if has1 {
			result.CacheCreation.Ephemeral1h = &c1
		}
	}
	return result
}
func token(m map[string]any, key string, present *bool) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		*present = true
		return int64(n)
	case json.Number:
		value, err := n.Int64()
		if err == nil {
			*present = true
			return value
		}
	}
	return 0
}
