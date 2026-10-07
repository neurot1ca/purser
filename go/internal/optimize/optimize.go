package optimize

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var weights = map[string]float64{"input_tokens": 1, "cache_creation_input_tokens": 1.25, "cache_read_input_tokens": .1, "output_tokens": 5}
var commandTag = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
var slashCommand = regexp.MustCompile(`(?:^|[^\w])/(?:[\w.-]+:)?[\w.-]+`)
var mcpTool = regexp.MustCompile(`^mcp__(.+?)__(.+)$`)
var wordPattern = regexp.MustCompile(`[a-z0-9]{4,}`)
var tokenPattern = regexp.MustCompile(`[a-z0-9]+`)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}$`)

type tool struct {
	name, id string
	input    map[string]any
}
type request struct {
	id     string
	weight float64
	chars  int
	final  bool
	tools  []tool
}
type usageEvent struct{ kind, name string }
type event struct {
	kind  string
	index int
	names []string
}
type session struct {
	path, cwd, stamp, firstEntry string
	mtime                        time.Time
	requests                     []request
	requestTurns                 [][]int
	entrypoints                  map[string]int
	promptOrder                  []string
	firstPrompts                 map[string]string
	skills                       map[string]float64
	servers                      map[string]float64
	loaded                       map[string]bool
	usage                        []usageEvent
	typed, sidechain             bool
}
type startupItem struct {
	name, kind, status, lastSeen     string
	tokensPerRequest, totalTokens    float64
	requestsLoaded, uses             int
	flagged, observedLoaded, builtIn bool
}
type hiddenRow struct {
	entrypoint, source, status, lastSeen string
	sessions                             int
	share, weightedTokens                float64
}
type sequenceStep []string
type sequence struct {
	steps                 []sequenceStep
	occurrences, sessions int
	saving                float64
}
type row struct {
	status                       string
	value                        float64
	finding, cost, state, action string
}
type renderCandidate struct {
	kind                          string
	item                          *startupItem
	items                         []startupItem
	tokensPerRequest, totalTokens float64
	status, lastSeen              string
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return n
		}
	case json.Number:
		f, _ := n.Float64()
		if f > 0 {
			return f
		}
	case int:
		if n > 0 {
			return float64(n)
		}
	}
	return 0
}
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, p := range x {
			b.WriteString(text(p))
		}
		return b.String()
	case map[string]any:
		if s, ok := x["text"].(string); ok {
			return s
		}
		return text(x["content"])
	}
	return ""
}
func commandText(v any) string {
	if m, ok := v.(map[string]any); ok {
		if blocks, ok := m["content"].([]any); ok {
			var b strings.Builder
			for _, item := range blocks {
				if block, ok := item.(map[string]any); ok && block["type"] == "text" {
					b.WriteString(text(block["text"]))
				}
			}
			return b.String()
		}
	}
	return text(v)
}
func rendered(record map[string]any) string {
	if value, ok := record["rendered"].([]any); ok {
		var parts []string
		for _, item := range value {
			parts = append(parts, text(item))
		}
		return strings.Join(parts, "\n")
	}
	return text(record["rendered"])
}
func serverName(name string) string {
	m := mcpTool.FindStringSubmatch(name)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}
func cleanServer(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func sameServer(left, right string) bool {
	l, r := cleanServer(left), cleanServer(right)
	return l == r || strings.HasSuffix(l, r) || strings.HasSuffix(r, l)
}
func serverKey(servers map[string]float64, name string) string {
	for known := range servers {
		if sameServer(known, name) {
			return known
		}
	}
	return name
}
func asStrings(v any) []string {
	var out []string
	if values, ok := v.([]any); ok {
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func appendUnique(items []string, values ...string) []string {
	for _, value := range values {
		if !containsString(items, value) {
			items = append(items, value)
		}
	}
	return items
}
func removeExact(items, removed []string) []string {
	out := []string{}
	for _, item := range items {
		if !containsString(removed, item) {
			out = append(out, item)
		}
	}
	return out
}
func removeMatching(items, removed []string) []string {
	out := items[:0]
	for _, item := range items {
		keep := true
		for _, value := range removed {
			if sameServer(item, value) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, item)
		}
	}
	return out
}
func max(n, other int) int {
	if n > other {
		return n
	}
	return other
}

func skillRows(a map[string]any) [][2]string {
	content, _ := a["content"].(string)
	rows := [][2]string{}
	lineRE := regexp.MustCompile(`^\s*-\s*([\w.-]+(?::[\w.-]+)*):`)
	for _, line := range strings.Split(content, "\n") {
		if m := lineRE.FindStringSubmatch(line); len(m) == 2 {
			rows = append(rows, [2]string{m[1], line})
		}
	}
	if len(rows) == 0 {
		for _, name := range asStrings(a["names"]) {
			rows = append(rows, [2]string{name, name})
		}
	}
	return rows
}
func normalizedCommand(t tool) string {
	command := ""
	if t.input != nil {
		command, _ = t.input["command"].(string)
	}
	command = strings.TrimSpace(command)
	navigation := regexp.MustCompile(`(?i)^(?:cd|pushd|Set-Location)\s+(?:"[^"]*"|'[^']*'|[^\s;&|]+)\s*(?:&&|;)\s*`)
	for {
		m := navigation.FindStringIndex(command)
		if m == nil {
			return command
		}
		command = strings.TrimSpace(command[m[1]:])
	}
}

var readOnly = map[string]bool{"grep": true, "rg": true, "cat": true, "head": true, "tail": true, "ls": true, "find": true, "wc": true, "get-content": true, "select-string": true, "get-childitem": true}
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true}
var inline = map[string]map[string]bool{"python": {"-": true, "-c": true}, "python3": {"-": true, "-c": true}, "py": {"-": true, "-c": true}, "node": {"-e": true}, "powershell": {"-command": true}, "pwsh": {"-c": true}}

func commandWords(command string) []string {
	return strings.FieldsFunc(command, func(r rune) bool { return r == ' ' || r == '\t' || r == '|' || r == ';' || r == '&' })
}
func eligible(tools []tool) bool {
	if len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		if t.name != "Bash" && t.name != "PowerShell" {
			return false
		}
		words := commandWords(normalizedCommand(t))
		if len(words) == 0 {
			return false
		}
		first, second := strings.ToLower(words[0]), ""
		if len(words) > 1 {
			second = strings.ToLower(words[1])
		}
		if inline[first][second] || readOnly[first] || first == "sed" && second == "-n" || first == "git" && readOnlyGit[second] {
			return false
		}
		if regexp.MustCompile(`(?i)^(?:python(?:3)?|py)\s+(?:-\s+)?<<`).MatchString(normalizedCommand(t)) {
			return false
		}
	}
	return true
}
func signature(tools []tool) sequenceStep {
	values := []string{}
	for _, t := range tools {
		name := t.name
		if name == "Bash" || name == "PowerShell" {
			words := commandWords(normalizedCommand(t))
			if len(words) > 2 {
				words = words[:2]
			}
			if len(words) > 0 {
				name += " " + strings.Join(words, " ")
			}
		}
		values = append(values, name)
	}
	sort.Strings(values)
	out := []string{}
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
func genuineTyping(record map[string]any) bool {
	message := strings.TrimSpace(commandText(record["message"]))
	if message == "" || strings.Contains(message, "<command-name>") || strings.Contains(message, "<local-command-") || strings.Contains(message, "<task-notification>") {
		return false
	}
	if origin, ok := record["origin"].(map[string]any); ok && origin["kind"] == "human" {
		return true
	}
	if record["turnOrigin"] == "human" || record["promptSource"] == "typed" {
		return true
	}
	return record["userType"] == "external" && record["promptSource"] != "sdk" && record["promptSource"] != "system"
}
func heading(block string) string {
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "## ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "## "))
		}
	}
	return ""
}
func namesToStrings(matches [][]string) []string {
	out := []string{}
	for _, m := range matches {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
}
func toolIDIn(tools []tool, id string) bool {
	for _, t := range tools {
		if t.id == id && id != "" {
			return true
		}
	}
	return false
}

func readSession(path string) (session, error) {
	s := session{path: path, entrypoints: map[string]int{}, firstPrompts: map[string]string{}, skills: map[string]float64{}, servers: map[string]float64{}, loaded: map[string]bool{}}
	info, err := os.Stat(path)
	if err != nil {
		return s, err
	}
	s.mtime = info.ModTime()
	s.sidechain = strings.EqualFold(filepath.Base(filepath.Dir(path)), "subagents")
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	ids := map[string]int{}
	events := []event{}
	seenListings := map[string]bool{}
	seenMCP := map[string]bool{}
	seenDeferred := map[string]bool{}
	seenInstruction := map[string]bool{}
	activeSkills := []string{}
	activeServers := []string{}
	activeDeferred := []string{}
	commandNames := []string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var record map[string]any
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.UseNumber()
		if decoder.Decode(&record) != nil {
			continue
		}
		if s.cwd == "" {
			s.cwd, _ = record["cwd"].(string)
		}
		if value, ok := record["isSidechain"].(bool); ok {
			s.sidechain = s.sidechain || value
		}
		if value, ok := record["agentId"].(string); ok {
			s.sidechain = s.sidechain || value != ""
		}
		if stamp, ok := record["timestamp"].(string); ok {
			s.stamp = stamp
		}
		entrypoint, _ := record["entrypoint"].(string)
		if entrypoint == "" {
			entrypoint = "unknown"
		}
		if entrypoint != "unknown" {
			if _, exists := s.entrypoints[entrypoint]; !exists {
				s.promptOrder = append(s.promptOrder, entrypoint)
				if s.firstEntry == "" {
					s.firstEntry = entrypoint
				}
			}
			s.entrypoints[entrypoint]++
		}
		message, _ := record["message"].(map[string]any)
		role, _ := message["role"].(string)
		if record["type"] == "user" || role == "user" {
			msg := commandText(message)
			if _, exists := s.firstPrompts[entrypoint]; !exists && strings.TrimSpace(msg) != "" && !strings.Contains(msg, "<command-name>") && !strings.Contains(msg, "<local-command-") && !strings.Contains(msg, "<task-notification>") {
				s.firstPrompts[entrypoint] = strings.Join(strings.Fields(msg), " ")
				if len([]rune(s.firstPrompts[entrypoint])) > 80 {
					s.firstPrompts[entrypoint] = string([]rune(s.firstPrompts[entrypoint])[:80])
				}
				s.promptOrder = append(s.promptOrder, entrypoint)
			}
			s.typed = s.typed || genuineTyping(record)
			matches := commandTag.FindAllStringSubmatch(msg, -1)
			for _, m := range matches {
				commandNames = append(commandNames, m[1])
			}
			for _, value := range slashCommand.FindAllString(msg, -1) {
				commandNames = append(commandNames, strings.TrimSpace(value))
			}
			if strings.TrimSpace(msg) != "" && (strings.Contains(msg, "<command-name>") || !strings.Contains(msg, "<local-command-") || len(matches) > 0) {
				events = append(events, event{kind: "user"})
			}
			if len(matches) > 0 {
				events = append(events, event{kind: "command", names: namesToStrings(matches)})
			}
		}
		if role == "assistant" {
			id, _ := message["id"].(string)
			if id == "" {
				id, _ = record["requestId"].(string)
			}
			if id == "" {
				id = fmt.Sprintf("line:%d", len(s.requests))
			}
			index, exists := ids[id]
			if !exists {
				usage, _ := message["usage"].(map[string]any)
				weight := 0.0
				for key, factor := range weights {
					weight += number(usage[key]) * factor
				}
				index = len(s.requests)
				ids[id] = index
				s.requests = append(s.requests, request{id: id, weight: weight, tools: []tool{}})
				events = append(events, event{kind: "request", index: index})
			}
			req := &s.requests[index]
			if blocks, ok := message["content"].([]any); ok {
				for _, item := range blocks {
					block, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if block["type"] == "text" {
						req.chars += len([]rune(text(block["text"])))
					}
					if block["type"] == "tool_use" {
						name, _ := block["name"].(string)
						toolID, _ := block["id"].(string)
						input, _ := block["input"].(map[string]any)
						if toolID == "" || !toolIDIn(req.tools, toolID) {
							req.tools = append(req.tools, tool{name: name, id: toolID, input: input})
						}
					}
				}
			}
			req.final = req.final || message["stop_reason"] == "end_turn"
		}
		if record["type"] != "attachment" {
			continue
		}
		attachment, _ := record["attachment"].(map[string]any)
		kind, _ := attachment["type"].(string)
		switch kind {
		case "skill_listing":
			rows := skillRows(attachment)
			names := asStrings(attachment["names"])
			if len(names) == 0 {
				for _, row := range rows {
					names = append(names, row[0])
				}
			}
			initial := true
			if value, ok := attachment["isInitial"].(bool); ok {
				initial = value
			}
			if initial {
				activeSkills = append([]string{}, names...)
			} else {
				for _, name := range names {
					if !containsString(activeSkills, name) {
						activeSkills = append(activeSkills, name)
					}
				}
			}
			content, _ := attachment["content"].(string)
			if seenListings[content] {
				continue
			}
			seenListings[content] = true
			lineCosts := map[string]int{}
			size := 0
			for _, row := range rows {
				lineCosts[row[0]] = len([]rune(row[1]))
				size += lineCosts[row[0]]
			}
			if size < 1 {
				size = 1
			}
			for _, name := range names {
				s.loaded[name] = true
				if _, ok := s.skills[name]; !ok {
					s.skills[name] = float64(lineCosts[name]) / float64(size) * float64(len([]rune(content))) / 4
				}
				canonical, source := name, "user"
				if i := strings.LastIndex(name, ":"); i >= 0 {
					canonical, source = name[i+1:], name[:i]
				}
				_ = canonical
				_ = source
			}
		case "mcp_instructions_delta":
			added, blocks, removed := asStrings(attachment["addedNames"]), asStrings(attachment["addedBlocks"]), asStrings(attachment["removedNames"])
			activeServers = removeMatching(activeServers, removed)
			activeServers = appendUnique(activeServers, added...)
			key, _ := json.Marshal([]any{added, blocks})
			if seenMCP[string(key)] {
				continue
			}
			seenMCP[string(key)] = true
			for _, name := range added {
				s.loaded[name] = true
				server := serverKey(s.servers, name)
				if seenInstruction[server] {
					continue
				}
				seenInstruction[server] = true
				blockText := ""
				for _, block := range blocks {
					if sameServer(server, heading(block)) {
						if blockText != "" {
							blockText += "\n"
						}
						blockText += block
					}
				}
				if blockText == "" {
					blockText = rendered(record)
				}
				s.servers[server] += float64(len([]rune(blockText))) / float64(max(1, len(added))) / 4
			}
		case "deferred_tools_delta":
			added, removed := asStrings(attachment["addedNames"]), asStrings(attachment["removedNames"])
			activeDeferred = removeExact(activeDeferred, removed)
			activeDeferred = appendUnique(activeDeferred, added...)
			for _, name := range added {
				server := serverName(name)
				if server == "" {
					continue
				}
				s.loaded[server] = true
				key := serverKey(s.servers, server)
				if !seenDeferred[name] {
					seenDeferred[name] = true
					s.servers[key] += float64(len([]rune(name))) / 4
				}
			}
		case "deferred_tools_record":
			if entries, ok := attachment["entries"].([]any); ok {
				for _, value := range entries {
					if item, ok := value.(map[string]any); ok {
						name, _ := item["name"].(string)
						server := serverName(name)
						if server != "" {
							s.loaded[server] = true
							key := serverKey(s.servers, server)
							if !seenDeferred[name] {
								seenDeferred[name] = true
								s.servers[key] += float64(len([]rune(name))) / 4
							}
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return s, err
	}
	for _, req := range s.requests {
		for _, t := range req.tools {
			if t.name == "Skill" {
				if skill, ok := t.input["skill"].(string); ok {
					s.usage = append(s.usage, usageEvent{"skill", skill})
				}
			}
			if server := serverName(t.name); server != "" {
				s.usage = append(s.usage, usageEvent{"mcp", server})
			}
		}
	}
	for _, name := range commandNames {
		s.usage = append(s.usage, usageEvent{"command", strings.TrimPrefix(name, "/")})
	}
	turn := []int{}
	for _, item := range events {
		if item.kind == "user" {
			if len(turn) > 0 {
				s.requestTurns = append(s.requestTurns, turn)
				turn = nil
			}
		} else if item.kind == "request" {
			turn = append(turn, item.index)
		}
	}
	if len(turn) > 0 {
		s.requestTurns = append(s.requestTurns, turn)
	}
	activeSet := map[string]bool{}
	for _, name := range activeServers {
		activeSet[name] = true
	}
	for _, name := range activeDeferred {
		if server := serverName(name); server != "" {
			activeSet[server] = true
		}
	}
	for name := range s.servers {
		keep := false
		for active := range activeSet {
			if sameServer(name, active) {
				keep = true
			}
		}
		if !keep {
			delete(s.servers, name)
		}
	}
	skillsSet := map[string]bool{}
	for _, name := range activeSkills {
		skillsSet[name] = true
	}
	for name := range s.skills {
		if !skillsSet[name] {
			delete(s.skills, name)
		}
	}
	return s, nil
}

func dateSeen(s session) string {
	if t, err := time.Parse(time.RFC3339Nano, s.stamp); err == nil {
		return t.Format("2006-01-02")
	}
	return s.mtime.Format("2006-01-02")
}
func setting(paths []string, name string) (bool, bool) {
	var state bool
	found := false
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var config map[string]any
		if json.Unmarshal(data, &config) != nil {
			continue
		}
		enabled, _ := config["enabledPlugins"].(map[string]any)
		matches := []bool{}
		if value, ok := enabled[name].(bool); ok {
			matches = append(matches, value)
		} else {
			for key, value := range enabled {
				if strings.HasPrefix(key, name+"@") {
					if flag, ok := value.(bool); ok {
						matches = append(matches, flag)
					}
				}
			}
		}
		if len(matches) == 1 {
			state, found = matches[0], true
		} else if len(matches) > 1 {
			found = false
		}
	}
	return state, found
}
func pluginEnabled(name, home, repo string) *bool {
	paths := []string{filepath.Join(home, ".claude", "settings.json")}
	if repo != "" {
		paths = append(paths, filepath.Join(repo, ".claude", "settings.json"), filepath.Join(repo, ".claude", "settings.local.json"))
	}
	state, known := setting(paths, name)
	if !known {
		return nil
	}
	return &state
}
func pluginPresent(name string, s session) bool {
	for loaded := range s.loaded {
		if loaded == name || strings.HasPrefix(loaded, name+":") || strings.HasPrefix(loaded, "plugin:"+name+":") {
			return true
		}
	}
	for _, use := range s.usage {
		if (use.kind == "skill" || use.kind == "command" || use.kind == "mcp") && (use.name == name || strings.HasPrefix(use.name, name+":") || strings.HasPrefix(use.name, "plugin:"+name+":")) {
			return true
		}
	}
	for _, req := range s.requests {
		for _, t := range req.tools {
			if strings.HasPrefix(t.name, "plugin:"+name+":") || strings.HasPrefix(serverName(t.name), "plugin:"+name+":") {
				return true
			}
		}
	}
	return false
}
func installedMatches(prompt, home string) []string {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var config map[string]any
	if json.Unmarshal(data, &config) != nil {
		return nil
	}
	plugins, _ := config["plugins"].(map[string]any)
	words := map[string]bool{}
	for _, word := range wordPattern.FindAllString(strings.ToLower(prompt), -1) {
		words[word] = true
	}
	for _, word := range []string{"this", "that", "with", "from", "into", "for", "code", "review", "please", "run"} {
		delete(words, word)
	}
	tokens := tokenPattern.FindAllString(strings.ToLower(prompt), -1)
	pairs := map[string]bool{}
	for i := 0; i+1 < len(tokens); i++ {
		pairs[tokens[i]+"\x00"+tokens[i+1]] = true
	}
	type scored struct {
		score int
		name  string
	}
	scores := []scored{}
	for plugin, raw := range plugins {
		installs, ok := raw.([]any)
		if !ok || len(installs) == 0 {
			continue
		}
		terms := map[string]bool{}
		for _, word := range wordPattern.FindAllString(strings.ToLower(plugin), -1) {
			terms[word] = true
		}
		installPath := ""
		if item, ok := installs[len(installs)-1].(map[string]any); ok {
			installPath, _ = item["installPath"].(string)
		}
		sourceText := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(plugin, "-", " "), "_", " "))
		for _, file := range []string{filepath.Join(installPath, ".claude-plugin", "plugin.json"), filepath.Join(installPath, "README.md")} {
			if content, err := os.ReadFile(file); err == nil {
				value := strings.ToLower(string(content))
				for _, word := range wordPattern.FindAllString(value, -1) {
					terms[word] = true
				}
				sourceText += " " + value
			}
		}
		score := 0
		for word := range words {
			if terms[word] {
				score++
			}
		}
		sourceTokens := tokenPattern.FindAllString(sourceText, -1)
		pairScore := 0
		for i := 0; i+1 < len(sourceTokens); i++ {
			if pairs[sourceTokens[i]+"\x00"+sourceTokens[i+1]] {
				pairScore++
			}
		}
		if score >= 2 || pairScore > 0 {
			scores = append(scores, scored{score + pairScore*2, plugin})
		}
	}
	if len(scores) == 0 {
		return nil
	}
	best := 0
	for _, item := range scores {
		if item.score > best {
			best = item.score
		}
	}
	out := []string{}
	for _, item := range scores {
		if item.score == best {
			out = append(out, item.name)
		}
	}
	sort.Strings(out)
	return out
}
func localSources(name, home, repo string) map[string]bool {
	out := map[string]bool{}
	if info, err := os.Stat(filepath.Join(home, ".claude", "skills", name)); err == nil && info.IsDir() {
		out["user"] = true
	}
	if repo != "" {
		if info, err := os.Stat(filepath.Join(repo, ".claude", "skills", name)); err == nil && info.IsDir() {
			out["project"] = true
		}
	}
	return out
}
func skillMatches(candidate, target string) bool {
	return candidate == target || strings.HasSuffix(candidate, ":"+target) || strings.HasSuffix(target, ":"+candidate)
}
func latestScoped(sessions []session, repo string) *session {
	var latest *session
	for i := range sessions {
		if repo == "" || sessions[i].cwd == "" || filepath.Clean(sessions[i].cwd) != filepath.Clean(repo) {
			continue
		}
		if latest == nil || sessions[i].mtime.After(latest.mtime) {
			latest = &sessions[i]
		}
	}
	return latest
}
func median(values []int) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Ints(values)
	if len(values)%2 == 1 {
		return float64(values[len(values)/2])
	}
	return float64(values[len(values)/2-1]+values[len(values)/2]) / 2
}
func pyRound(value float64) int {
	floor := math.Floor(value)
	if value-floor > .5 || (value-floor == .5 && int64(floor)%2 != 0) {
		return int(floor + 1)
	}
	return int(floor)
}
func formatNumber(value float64) string {
	if value >= 1_000_000 {
		return fmt.Sprintf("%.1fM", value/1_000_000)
	}
	if value >= 1000 {
		return fmt.Sprintf("%.1fK", value/1000)
	}
	return strconv.Itoa(pyRound(value))
}
func compactTokens(value float64) string {
	return strings.ReplaceAll(strings.ReplaceAll(formatNumber(value), ".0K", "K"), ".0M", "M")
}
func displayName(value string) string {
	if uuidPattern.MatchString(value) {
		return "an unnamed claude.ai connector"
	}
	return strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(value))
}
func statusText(status, seen, reason string) string {
	label := "status unknown"
	if status == "stopped" {
		if reason == "" {
			reason = "not loaded"
		}
		label = "stopped: " + reason
	} else if status == "active" {
		label = "active"
	} else if status == "enabled" {
		label = "enabled but not observed"
	}
	if seen == "" {
		seen = "unknown"
	}
	return label + " · last seen " + seen
}

func sequenceKey(steps []sequenceStep) string { data, _ := json.Marshal(steps); return string(data) }
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func uniqueSteps(steps []sequenceStep) []sequenceStep {
	out := []sequenceStep{}
	for _, step := range steps {
		exists := false
		for _, prior := range out {
			if equalStrings(step, prior) {
				exists = true
			}
		}
		if !exists {
			out = append(out, step)
		}
	}
	return out
}
func containsSequence(longer, shorter []sequenceStep) bool {
	if len(shorter) == 0 || len(shorter) > len(longer) {
		return false
	}
	for i := 0; i <= len(longer)-len(shorter); i++ {
		same := true
		for j := range shorter {
			if !equalStrings(longer[i+j], shorter[j]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}
func sequenceCandidates(sessions []session) []sequence {
	type aggregate struct {
		occurrences int
		sessions    map[string]bool
		costs       []float64
	}
	all := map[string]*aggregate{}
	for _, session := range sessions {
		for _, turn := range session.requestTurns {
			runs := [][]int{}
			run := []int{}
			for _, index := range turn {
				if len(session.requests[index].tools) == 0 {
					continue
				}
				if !eligible(session.requests[index].tools) {
					if len(run) > 0 {
						runs = append(runs, run)
						run = nil
					}
					continue
				}
				run = append(run, index)
			}
			if len(run) > 0 {
				runs = append(runs, run)
			}
			for _, current := range runs {
				for n := 3; n <= 6; n++ {
					next := map[string]int{}
					for start := 0; start+n <= len(current); start++ {
						steps := []sequenceStep{}
						for _, index := range current[start : start+n] {
							steps = append(steps, signature(session.requests[index].tools))
						}
						if len(uniqueSteps(steps)) < 2 {
							continue
						}
						key := sequenceKey(steps)
						if start < next[key] {
							continue
						}
						if all[key] == nil {
							all[key] = &aggregate{sessions: map[string]bool{}}
						}
						item := all[key]
						item.occurrences++
						item.sessions[session.path] = true
						for _, index := range current[start : start+n] {
							item.costs = append(item.costs, session.requests[index].weight)
						}
						next[key] = start + n
					}
				}
			}
		}
	}
	out := []sequence{}
	for key, item := range all {
		if len(item.sessions) < 3 {
			continue
		}
		var steps []sequenceStep
		_ = json.Unmarshal([]byte(key), &steps)
		sort.Float64s(item.costs)
		med := 0.0
		if len(item.costs) > 0 {
			if len(item.costs)%2 == 1 {
				med = item.costs[len(item.costs)/2]
			} else {
				med = (item.costs[len(item.costs)/2-1] + item.costs[len(item.costs)/2]) / 2
			}
		}
		out = append(out, sequence{steps: steps, occurrences: item.occurrences, sessions: len(item.sessions), saving: float64(len(steps)-1) * float64(item.occurrences) * med})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].saving != out[j].saving {
			return out[i].saving > out[j].saving
		}
		if len(out[i].steps) != len(out[j].steps) {
			return len(out[i].steps) > len(out[j].steps)
		}
		return sequenceKey(out[i].steps) < sequenceKey(out[j].steps)
	})
	return out
}
func selectSequences(candidates []sequence, limit int) []sequence {
	out := []sequence{}
	for _, candidate := range candidates {
		skip := false
		for _, prior := range out {
			if containsSequence(prior.steps, candidate.steps) || containsSequence(candidate.steps, prior.steps) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, candidate)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func compactionSettings(home string) (bool, int) {
	enabled, threshold := true, 250000
	data, err := os.ReadFile(filepath.Join(home, ".orchestrator", "config.toml"))
	if err != nil {
		return enabled, threshold
	}
	section := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line == "[compaction]"
			continue
		}
		if !section {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "enabled" {
			enabled = value != "false"
		}
		if key == "threshold_tokens" {
			if fields := strings.Fields(value); len(fields) > 0 {
				if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
					threshold = n
				}
			}
		}
	}
	return enabled, threshold
}

func Analyze(projects, home, repo string, days int) (map[string]any, error) {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	sessions := []session{}
	if err := filepath.WalkDir(projects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		s, err := readSession(path)
		if err == nil && !s.mtime.Before(cutoff) {
			sessions = append(sessions, s)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	requests := []request{}
	for _, s := range sessions {
		requests = append(requests, s.requests...)
	}
	totalWeight := 0.0
	for _, r := range requests {
		totalWeight += r.weight
	}
	interactiveEntries := map[string]bool{"cli": true, "claude-desktop": true}
	for _, s := range sessions {
		if s.typed {
			for entry := range s.entrypoints {
				interactiveEntries[entry] = true
			}
		}
	}
	for _, s := range sessions {
		if repo != "" && filepath.Clean(s.cwd) == filepath.Clean(repo) && s.firstEntry == "unknown" {
			interactiveEntries["unknown"] = true
		}
	}
	interactive := []session{}
	for _, s := range sessions {
		if !s.sidechain && (s.typed || interactiveEntries[s.firstEntry] || (repo != "" && filepath.Clean(s.cwd) == filepath.Clean(repo))) {
			interactive = append(interactive, s)
		}
	}
	var current *session
	for i := range interactive {
		if current == nil || interactive[i].mtime.After(current.mtime) {
			current = &interactive[i]
		}
	}
	requestCount := 0
	if current != nil {
		requestCount = len(current.requests)
	}
	usage := []usageEvent{}
	for _, s := range sessions {
		usage = append(usage, s.usage...)
	}
	skillUses := func(name string) int {
		count := 0
		for _, item := range usage {
			if (item.kind == "skill" || item.kind == "command") && skillMatches(item.name, name) {
				count++
			}
		}
		return count
	}
	startup := []startupItem{}
	duplicates := map[string]map[string]bool{}
	if current != nil {
		for name, cost := range current.skills {
			canonical := name[strings.LastIndex(name, ":")+1:]
			sources := localSources(canonical, home, repo)
			if strings.Contains(name, ":") {
				sources[strings.SplitN(name, ":", 2)[0]] = true
			}
			for source := range sources {
				if duplicates[canonical] == nil {
					duplicates[canonical] = map[string]bool{}
				}
				duplicates[canonical][source] = true
			}
			builtIn := !strings.Contains(name, ":") && len(sources) == 0
			uses := skillUses(name)
			startup = append(startup, startupItem{name: "skill:" + name, kind: "skill", tokensPerRequest: cost, requestsLoaded: requestCount, uses: uses, builtIn: builtIn, totalTokens: cost * float64(requestCount), flagged: !builtIn && uses == 0 && cost >= 100})
		}
		for name, cost := range current.servers {
			uses := 0
			for _, item := range usage {
				if item.kind == "mcp" && sameServer(name, item.name) {
					uses++
				}
			}
			startup = append(startup, startupItem{name: "mcp:" + name, kind: "MCP", tokensPerRequest: cost, requestsLoaded: requestCount, uses: uses, totalTokens: cost * float64(requestCount), flagged: uses == 0 && cost >= 100})
		}
		plugins := map[string]bool{}
		for name := range current.skills {
			if i := strings.Index(name, ":"); i > 0 {
				plugins[name[:i]] = true
			}
		}
		for plugin := range plugins {
			cost, uses := 0.0, 0
			for name, value := range current.skills {
				if strings.HasPrefix(name, plugin+":") {
					cost += value
					uses += skillUses(name)
				}
			}
			for _, item := range usage {
				if item.kind == "mcp" && sameServer(plugin, item.name) {
					uses++
				}
			}
			startup = append(startup, startupItem{name: "plugin:" + plugin, kind: "plugin", tokensPerRequest: cost, requestsLoaded: requestCount, uses: uses, totalTokens: cost * float64(requestCount), flagged: uses == 0 && cost >= 100})
		}
	}
	sort.Slice(startup, func(i, j int) bool {
		if startup[i].flagged != startup[j].flagged {
			return startup[i].flagged
		}
		if startup[i].totalTokens != startup[j].totalTokens {
			return startup[i].totalTokens > startup[j].totalTokens
		}
		return startup[i].name < startup[j].name
	})
	if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var config map[string]any
		if json.Unmarshal(data, &config) == nil {
			if servers, ok := config["mcpServers"].(map[string]any); ok {
				for name := range servers {
					startup = append(startup, startupItem{name: name, kind: "registered connector", status: "registered", flagged: true})
				}
			}
		}
	}
	duplicateRows := []map[string]any{}
	for name, sources := range duplicates {
		if len(sources) >= 2 {
			values := []string{}
			for source := range sources {
				values = append(values, source)
			}
			sort.Strings(values)
			duplicateRows = append(duplicateRows, map[string]any{"name": name, "sources": values})
		}
	}
	sort.Slice(duplicateRows, func(i, j int) bool { return duplicateRows[i]["name"].(string) < duplicateRows[j]["name"].(string) })
	replies := []int{}
	for _, s := range interactive {
		for _, turn := range s.requestTurns {
			if len(turn) == 0 {
				continue
			}
			index := turn[len(turn)-1]
			for i := len(turn) - 1; i >= 0; i-- {
				if s.requests[turn[i]].final {
					index = turn[i]
					break
				}
			}
			replies = append(replies, s.requests[index].chars)
		}
	}
	medianReply := median(replies)
	replyRule := false
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
		replyRule = regexp.MustCompile(`(?m)^# Replies\s*$`).Match(data)
	}
	sequences := selectSequences(sequenceCandidates(sessions), 5)
	enabled, threshold := compactionSettings(home)
	scoped := []session{}
	for _, s := range sessions {
		if repo != "" && filepath.Clean(s.cwd) == filepath.Clean(repo) {
			scoped = append(scoped, s)
		}
	}
	latest := latestScoped(sessions, repo)
	seenFor := func(match func(session) bool) string {
		result := ""
		for _, s := range scoped {
			if match(s) && (result == "" || dateSeen(s) > result) {
				result = dateSeen(s)
			}
		}
		return result
	}
	for i := range startup {
		item := &startup[i]
		if item.kind == "plugin" {
			plugin := strings.TrimPrefix(item.name, "plugin:")
			state := pluginEnabled(plugin, home, repo)
			if state == nil {
				item.status = "unknown"
			} else if *state {
				item.status = "enabled"
			} else {
				item.status = "stopped"
			}
			item.observedLoaded = latest != nil && pluginPresent(plugin, *latest)
			item.lastSeen = seenFor(func(s session) bool {
				if pluginPresent(plugin, s) {
					return true
				}
				for _, prompt := range s.firstPrompts {
					if containsString(installedMatches(prompt, home), plugin) {
						return true
					}
				}
				return false
			})
		} else if item.kind == "registered connector" {
			continue
		} else {
			name := strings.TrimPrefix(item.name, "mcp:")
			loaded := false
			if latest != nil {
				for value := range latest.loaded {
					if value == name || sameServer(name, value) {
						loaded = true
					}
				}
			}
			if latest == nil {
				item.status = "unknown"
			} else if loaded {
				item.status = "active"
			} else {
				item.status = "stopped"
			}
			item.lastSeen = seenFor(func(s session) bool {
				for value := range s.loaded {
					if value == name || sameServer(name, value) {
						return true
					}
				}
				return false
			})
		}
	}
	hidden := []hiddenRow{}
	allEntry := map[string]float64{}
	promptCosts := map[string]map[string]float64{}
	promptDates := map[string]map[string]string{}
	for _, s := range sessions {
		value := 0.0
		for _, req := range s.requests {
			value += req.weight
		}
		allEntry[s.firstEntry] += value
		if !interactiveEntries[s.firstEntry] {
			if promptCosts[s.firstEntry] == nil {
				promptCosts[s.firstEntry] = map[string]float64{}
				promptDates[s.firstEntry] = map[string]string{}
			}
			prompt := "(no first prompt recorded)"
			if len(s.promptOrder) > 0 {
				prompt = s.firstPrompts[s.promptOrder[0]]
			}
			promptCosts[s.firstEntry][prompt] += value
			if dateSeen(s) > promptDates[s.firstEntry][prompt] {
				promptDates[s.firstEntry][prompt] = dateSeen(s)
			}
		}
	}
	entryNames := []string{}
	for name := range allEntry {
		entryNames = append(entryNames, name)
	}
	sort.Slice(entryNames, func(i, j int) bool { return allEntry[entryNames[i]] > allEntry[entryNames[j]] })
	for _, ep := range entryNames {
		value := allEntry[ep]
		if interactiveEntries[ep] || value <= 0 {
			continue
		}
		hiddenSessions := 0
		for _, s := range sessions {
			if s.firstEntry == ep {
				hiddenSessions++
			}
		}
		prompts := []string{}
		for prompt := range promptCosts[ep] {
			prompts = append(prompts, prompt)
		}
		sort.Slice(prompts, func(i, j int) bool { return promptCosts[ep][prompts[i]] > promptCosts[ep][prompts[j]] })
		if len(prompts) > 3 {
			prompts = prompts[:3]
		}
		source, status, last := "", "unknown", ""
		for i, prompt := range prompts {
			matches := installedMatches(prompt, home)
			psource := ""
			if len(matches) == 1 {
				psource = matches[0]
			}
			pstatus := "unknown"
			if psource != "" {
				state := pluginEnabled(psource, home, repo)
				if state != nil && *state {
					pstatus = "active"
				} else if state != nil && !*state {
					pstatus = "stopped"
				}
			}
			if i == 0 {
				source, status, last = psource, pstatus, promptDates[ep][prompt]
			}
		}
		lastSession := ""
		for _, s := range sessions {
			if s.firstEntry == ep && dateSeen(s) > lastSession {
				lastSession = dateSeen(s)
			}
		}
		if last == "" {
			last = lastSession
		}
		hidden = append(hidden, hiddenRow{entrypoint: ep, source: source, status: status, lastSeen: last, sessions: hiddenSessions, weightedTokens: value, share: value / totalWeight})
	}
	return map[string]any{"startup": startup, "hidden": hidden, "sequences": sequences, "duplicate_skills": duplicateRows, "median_reply_chars": medianReply, "reply_rule_present": replyRule, "reply_count": len(replies), "compaction": map[string]any{"enabled": enabled, "threshold_tokens": threshold}, "days": days, "startup_note": "System prompt and eager tool schemas are not recorded in transcripts and cannot be attributed.", "weighted_tokens": totalWeight, "requests": len(requests), "sessions": len(sessions)}, nil
}

func first(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func containsStartupItem(items []startupItem, target startupItem) bool {
	for _, item := range items {
		if item.name == target.name {
			return true
		}
	}
	return false
}
func startupCandidates(startup []startupItem) []renderCandidate {
	rows := []startupItem{}
	for _, item := range startup {
		if item.flagged {
			rows = append(rows, item)
		}
	}
	docSkills, docPlugins := []startupItem{}, map[string]bool{}
	for _, item := range rows {
		if item.kind != "skill" {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(item.name, "skill:"), ":")
		last, related := strings.ToLower(parts[len(parts)-1]), false
		if last == "pptx" || last == "xlsx" || last == "docx" || last == "pdf" {
			for _, part := range parts {
				part = strings.ToLower(part)
				if part == "anthropic" || part == "document-skills" || part == "document-skills-plugin" {
					related = true
				}
			}
		}
		if related {
			docSkills = append(docSkills, item)
			docPlugins[parts[0]] = true
		}
	}
	plugins := map[string]bool{}
	for _, item := range rows {
		if item.kind == "plugin" {
			name := strings.TrimPrefix(item.name, "plugin:")
			if !docPlugins[name] {
				plugins[name] = true
			}
		}
	}
	out := []renderCandidate{}
	for _, item := range rows {
		if item.kind == "plugin" && docPlugins[strings.TrimPrefix(item.name, "plugin:")] {
			continue
		}
		short := strings.SplitN(item.name, ":", 2)[len(strings.SplitN(item.name, ":", 2))-1]
		if item.kind == "skill" {
			if containsStartupItem(docSkills, item) {
				continue
			}
			skip := false
			for plugin := range plugins {
				if strings.HasPrefix(short, plugin+":") {
					skip = true
				}
			}
			if skip {
				continue
			}
		}
		copy := item
		out = append(out, renderCandidate{kind: item.kind, item: &copy, tokensPerRequest: item.tokensPerRequest, totalTokens: item.totalTokens, status: item.status, lastSeen: item.lastSeen})
	}
	if len(docSkills) > 0 {
		statuses, active := map[string]bool{}, false
		last, per, total := "", 0.0, 0.0
		for _, item := range docSkills {
			statuses[item.status] = true
			active = active || item.status == "active"
			if item.lastSeen > last {
				last = item.lastSeen
			}
			per += item.tokensPerRequest
			total += item.totalTokens
		}
		status := "unknown"
		if len(statuses) == 1 && statuses["stopped"] {
			status = "stopped"
		} else if active {
			status = "active"
		}
		out = append(out, renderCandidate{kind: "document_skills", items: docSkills, tokensPerRequest: per, totalTokens: total, status: status, lastSeen: last})
	}
	return out
}
func render(report map[string]any) string {
	days, _ := report["days"].(int)
	if days == 0 {
		days = 30
	}
	entries := []row{}
	startup, _ := report["startup"].([]startupItem)
	for _, candidate := range startupCandidates(startup) {
		item := candidate.item
		perRequest, status, lastSeen, kind := candidate.tokensPerRequest, candidate.status, candidate.lastSeen, candidate.kind
		if item != nil {
			perRequest, status, lastSeen, kind = item.tokensPerRequest, item.status, item.lastSeen, item.kind
		}
		per500 := perRequest * 500
		cost := "~" + compactTokens(per500) + " / 500 msg"
		reason := ""
		if kind == "plugin" && status == "stopped" {
			reason = "plugin disabled"
		}
		if status == "stopped" && reason == "" {
			reason = "not loaded"
		}
		state := statusText(status, lastSeen, reason)
		action := ""
		if status == "stopped" {
			action = "—"
		}
		if kind == "MCP" {
			name := strings.TrimPrefix(item.name, "mcp:")
			entries = append(entries, row{status, per500, displayName(name) + " connector", cost, state, first(action, "Remove connector")})
		} else if kind == "plugin" {
			entries = append(entries, row{status, per500, displayName(strings.TrimPrefix(item.name, "plugin:")) + " plugin", cost, state, first(action, "Disable plugin")})
		} else if kind == "document_skills" {
			names := []string{}
			for _, doc := range candidate.items {
				parts := strings.Split(doc.name, ":")
				names = append(names, parts[len(parts)-1])
			}
			docState := state
			if status == "stopped" {
				docState = statusText(status, lastSeen, "not loaded")
			}
			docAction := "Remove skills"
			if status == "stopped" {
				docAction = "—"
			}
			entries = append(entries, row{status, per500, "Unused document skills (" + strings.Join(names, ", ") + ")", cost, docState, docAction})
		} else if kind == "registered connector" {
			entries = append(entries, row{status, per500, displayName(item.name) + " connector", cost, state, "Review connector settings"})
		} else {
			name := item.name
			if i := strings.LastIndex(name, ":"); i >= 0 {
				name = name[i+1:]
			}
			entries = append(entries, row{status, per500, "Unused " + displayName(name) + " skill", cost, state, first(action, "Remove skill")})
		}
	}
	sequences, _ := report["sequences"].([]sequence)
	for _, item := range sequences {
		steps := []string{}
		for _, step := range item.steps {
			steps = append(steps, strings.Join(step, " + "))
		}
		entries = append(entries, row{"active", item.saving, "Repeated Bash work: " + strings.Join(steps, " → "), "~" + compactTokens(item.saving) + " / " + strconv.Itoa(days) + " days", statusText("active", "", ""), "Make one script"})
	}
	hidden, _ := report["hidden"].([]hiddenRow)
	for _, item := range hidden {
		launcher := "unknown launcher"
		if item.source != "" {
			launcher = displayName(strings.SplitN(item.source, "@", 2)[0])
		}
		reason := ""
		if item.status == "stopped" && item.source != "" {
			reason = "plugin disabled"
		}
		action := "Review launcher"
		if item.status == "stopped" {
			action = "—"
		}
		entries = append(entries, row{item.status, item.weightedTokens, fmt.Sprintf("%s: %d hidden sessions", launcher, item.sessions), "~" + compactTokens(item.weightedTokens) + " / " + strconv.Itoa(days) + " days", statusText(item.status, item.lastSeen, reason), action})
	}
	medianValue, _ := report["median_reply_chars"].(float64)
	replyRule, _ := report["reply_rule_present"].(bool)
	if medianValue > 800 && !replyRule {
		entries = append(entries, row{"active", medianValue, fmt.Sprintf("Your replies average ~%d characters.", pyRound(medianValue)), fmt.Sprintf("~%d chars/reply", pyRound(medianValue)), statusText("active", "", ""), "Add reply rule to CLAUDE.md"})
	}
	priority := map[string]int{"active": 0, "enabled": 0, "unknown": 1, "stopped": 2}
	sort.SliceStable(entries, func(i, j int) bool {
		if priority[entries[i].status] != priority[entries[j].status] {
			return priority[entries[i].status] < priority[entries[j].status]
		}
		return entries[i].value > entries[j].value
	})
	lines := []string{"> ⚓ **Purser optimize**", "> ", "> *Biggest token drains*", "> ", "> | # | Finding | Cost | Status | Action |", "> | --- | --- | --- | --- | --- |"}
	for i, item := range entries {
		if i >= 7 {
			break
		}
		cells := []string{item.finding, item.cost, item.state, item.action}
		for j := range cells {
			cells[j] = strings.ReplaceAll(strings.ReplaceAll(cells[j], "|", "\\|"), "\n", " ")
		}
		lines = append(lines, fmt.Sprintf("> | %d | %s | %s | %s | %s |", i+1, cells[0], cells[1], cells[2], cells[3]))
	}
	stopped := []row{}
	for _, item := range entries {
		if item.status == "stopped" {
			stopped = append(stopped, item)
		}
	}
	if len(stopped) > 0 {
		summaries := []string{}
		for _, item := range stopped {
			label := item.finding
			if strings.Contains(label, " hidden sessions") {
				label = strings.SplitN(label, ":", 2)[0] + " hidden reviews"
			}
			amount := strings.SplitN(item.cost, " /", 2)[0]
			date := ""
			if index := strings.LastIndex(item.state, "last seen "); index >= 0 {
				date = ", last seen " + item.state[index+len("last seen "):]
			}
			summaries = append(summaries, fmt.Sprintf("%s (%s tokens%s)", label, amount, date))
		}
		lines = append(lines, "> ", "> *Already stopped: "+strings.Join(summaries, "; ")+".*")
	}
	if len(entries) == 0 {
		lines = append(lines, "> ", "> *No clear token waste found in your recent sessions.*")
	}
	lines = append(lines, "> ")
	duplicates, _ := report["duplicate_skills"].([]map[string]any)
	for _, item := range duplicates {
		sources, _ := item["sources"].([]string)
		lines = append(lines, fmt.Sprintf("> Duplicate skills: %s appears in %s.", item["name"], strings.Join(sources, ", ")))
	}
	if len(duplicates) == 0 {
		lines = append(lines, "> No additional duplicate-skill issues were found.")
	}
	if advice, ok := report["compaction"].(map[string]any); ok {
		on, _ := advice["enabled"].(bool)
		threshold, _ := advice["threshold_tokens"].(int)
		state := "Off"
		if on {
			state = "On"
		}
		lines = append(lines, fmt.Sprintf("> Compaction advice: %s; threshold: %d context tokens.", state, threshold))
	}
	replyCount, _ := report["reply_count"].(int)
	lines = append(lines, fmt.Sprintf("> Median final reply: %d characters across %d turns.", pyRound(medianValue), replyCount))
	if replyRule {
		lines = append(lines, "> Reply rule: # Replies is present in ~/.claude/CLAUDE.md.")
	}
	if medianValue > 800 && !replyRule {
		lines = append(lines, "> Suggested reply rule:", "> # Replies", "> Lead with the answer.", "> Keep replies short, at most 5 lines unless asked for detail.", "> No tables unless asked.")
	}
	for _, item := range startup {
		if item.kind == "plugin" && item.status == "stopped" && item.observedLoaded {
			lines = append(lines, "> Observation: "+strings.TrimPrefix(item.name, "plugin:")+" is disabled in the current configuration but was loaded in the observed session.")
		}
	}
	lines = append(lines, "> Estimates come from recorded session data. System prompts and eager tool schemas are not recorded, so their costs cannot be attributed here.", "> ", "> *🧠 Purser learns as it works in this project: it measures how long its work really takes, sharpens its estimates, and points out where tokens and time can be saved.*")
	return strings.Join(lines, "\n")
}
func Run(out io.Writer, projects, home, repo string, days int) error {
	report, err := Analyze(projects, home, repo, days)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, render(report))
	if err == nil {
		_, err = io.WriteString(out, "\n")
	}
	return err
}
