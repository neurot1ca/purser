package initsetup

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	modelversion "purser-hook/internal/models"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

// ReviewerRun lists or changes the machine's Reviewer-1 selection.
func ReviewerRun(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 || (args[0] != "list" && args[0] != "set") {
		return reviewerError(stdout, errors.New("usage: purser-hook reviewer list|set --cwd <dir>"))
	}
	action := args[0]
	fs := flag.NewFlagSet("reviewer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cwd := fs.String("cwd", "", "working directory")
	jsonOutput := fs.Bool("json", false, "JSON output")
	agentName := fs.String("agent", "", "reviewer agent")
	model := fs.String("model", "", "reviewer model")
	effort := fs.String("effort", "", "reviewer effort")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *cwd == "" || action == "list" && (!*jsonOutput || *agentName != "" || *model != "" || *effort != "") || action == "set" && *agentName == "" {
		return reviewerError(stdout, errors.New("invalid reviewer arguments"))
	}
	root, err := filepath.Abs(*cwd)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return reviewerError(stdout, err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return reviewerError(stdout, errors.New("--cwd must be a directory"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return reviewerError(stdout, err)
	}
	detected, err := detect(home, root, options{})
	if err != nil {
		return reviewerError(stdout, err)
	}
	configPath := filepath.Join(home, ".orchestrator", "config.toml")
	effective, err := effectiveUserConfig(root, configPath)
	if err != nil {
		return reviewerError(stdout, err)
	}
	if action == "list" {
		current := reviewerCurrentValues(effective, detected)
		return reviewerJSON(stdout, map[string]any{"current": current, "choices": reviewerChoices(detected)})
	}
	if *agentName == "none" {
		if *model != "" || *effort != "" {
			return reviewerError(stdout, errors.New("agent none does not accept model or effort"))
		}
	} else {
		if err := validateReviewerInstalled(detected, *agentName, *model); err != nil {
			return reviewerError(stdout, err)
		}
		if *model == "" {
			switch *agentName {
			case "codex":
				*model = codexReviewerModel(detected, "luna")
				if *model == "" {
					*model = codexReviewerModel(detected, "sol")
				}
			case "claude":
				*model = "claude-haiku-5-5"
			case "agy":
				*model = agyReviewerModel()
			}
		}
		if *effort == "" {
			switch *agentName {
			case "codex", "claude":
				*effort = "medium"
			}
		}
		if err := validateReviewerSelection(home, *agentName, *model, *effort); err != nil {
			return reviewerError(stdout, err)
		}
	}
	rolePath := profile.ProjectRolesPath(profile.StateRoot(), root)
	sections := []string{"roles", "reviewer"}
	if effective["coding"] != nil {
		sections = append(sections, "coding")
	}
	if err := updateLocked(rolePath, func(original []byte) ([]byte, error) {
		currentEffective, err := effectiveUserConfig(root, configPath)
		if err != nil {
			return nil, err
		}
		text, err := projectRoleText(original, currentEffective, sections...)
		if err != nil {
			return nil, err
		}
		values, _, err := profile.DecodeTOML(text)
		if err != nil {
			return nil, err
		}
		updated := string(text)
		if err := editReviewerConfig(&updated, values, *agentName, *model, *effort, "\n"); err != nil {
			return nil, err
		}
		return []byte(updated), nil
	}); err != nil {
		return reviewerError(stdout, err)
	}
	result := map[string]any{"agent": *agentName}
	if *agentName != "none" {
		result["model"], result["effort"] = *model, nullableString(*effort)
	}
	return reviewerJSON(stdout, map[string]any{"reviewer": result, "scope": "project", "roles_path": rolePath})
}

func reviewerCurrent(path string, detected map[string]any) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return reviewerDefault(detected, "", ""), nil
	}
	if err != nil {
		return nil, err
	}
	values, _, err := profile.DecodeTOML(raw)
	if err != nil {
		return nil, err
	}
	return reviewerCurrentValues(values, detected), nil
}

func reviewerCurrentValues(values map[string]any, detected map[string]any) map[string]any {
	role := ""
	if roles, ok := values["roles"].(map[string]any); ok {
		if value, ok := roles["reviewer"].(string); ok {
			role = value
		}
	}
	selected, _ := values["reviewer"].(map[string]any)
	if choice, ok := selected["selected"].(map[string]any); ok {
		role = asString(choice["agent"])
		return map[string]any{"agent": role, "model": nullableString(asString(choice["model"])), "effort": nullableString(asString(choice["effort"]))}
	}
	model, effort := "", ""
	if coding, ok := values["coding"].(map[string]any); ok {
		model = asString(coding["reviewer1_model_requested"])
		effort = asString(coding["reviewer1_effort"])
		if agent := asString(coding["reviewer1_agent"]); agent != "" {
			role = agent
		}
	}
	if role == "" {
		return reviewerDefault(detected, model, effort)
	}
	if role == "none" {
		return map[string]any{"agent": "none"}
	}
	return map[string]any{"agent": role, "model": nullableString(model), "effort": nullableString(effort)}
}

func reviewerDefault(detected map[string]any, configuredModel, configuredEffort string) map[string]any {
	agents, _ := detected["agents"].(map[string]any)
	for _, name := range []string{"agy", "claude", "codex"} {
		info, _ := agents[name].(map[string]any)
		if info["available"] != true {
			continue
		}
		switch name {
		case "agy":
			return map[string]any{"agent": name, "model": first(configuredModel, agyReviewerModel()), "effort": nil}
		case "claude":
			if !oneOf(configuredEffort, "low", "medium", "high", "xhigh", "max") {
				configuredEffort = "medium"
			}
			return map[string]any{"agent": name, "model": "claude-haiku-5-5", "effort": configuredEffort}
		case "codex":
			model := codexReviewerModel(detected, "luna")
			if model == "" {
				model = codexReviewerModel(detected, "sol")
			}
			return map[string]any{"agent": name, "model": nullableString(model), "effort": "low"}
		}
	}
	return map[string]any{"agent": "none"}
}

func reviewerChoices(detected map[string]any) []map[string]any {
	agents, _ := detected["agents"].(map[string]any)
	choices := []map[string]any{}
	for _, name := range []string{"codex", "claude", "agy", "opencode"} {
		info, _ := agents[name].(map[string]any)
		if info["available"] != true {
			continue
		}
		switch name {
		case "codex":
			for _, option := range codexReviewerOptionsFromDetection(detected) {
				option["installed"] = true
				choices = append(choices, option)
			}
		case "claude":
			choices = append(choices, map[string]any{"agent": name, "model": "claude-haiku-5-5", "effort": "medium", "installed": true})
		case "agy":
			choices = append(choices, map[string]any{"agent": name, "model": agyReviewerModel(), "effort": nil, "installed": true})
		case "opencode":
			models, _ := info["models"].([]any)
			ids := []string{}
			deepseek := []string{}
			for _, item := range models {
				id, _ := item.(string)
				if id != "" {
					ids = append(ids, id)
					if strings.Contains(strings.ToLower(id), "deepseek") {
						deepseek = append(deepseek, id)
					}
				}
			}
			preferred := []string{}
			for _, id := range deepseek {
				if strings.HasPrefix(strings.ToLower(id), "deepseek/") {
					preferred = append(preferred, id)
				}
			}
			if len(preferred) == 0 {
				preferred = deepseek
			}
			pro := []string{}
			for _, id := range preferred {
				if strings.Contains(strings.ToLower(id), "pro") {
					pro = append(pro, id)
				}
			}
			if len(pro) > 0 {
				preferred = pro
			}
			if len(preferred) == 0 {
				preferred = ids
			}
			if id := modelversion.NewestVersion(preferred); id != "" {
				choices = append(choices, map[string]any{"agent": name, "model": id, "effort": nil, "installed": true})
			}
		}
	}
	return append(choices, map[string]any{"agent": "none"})
}

func codexReviewerOptionsFromDetection(detected map[string]any) []map[string]any {
	options, _ := detected["reviewer_options"].([]map[string]any)
	choices := []map[string]any{}
	for _, option := range options {
		if option["agent"] == "codex" {
			choices = append(choices, map[string]any{"agent": "codex", "model": option["model"], "effort": option["effort"]})
		}
	}
	return choices
}

func codexReviewerModel(detected map[string]any, tier string) string {
	for _, option := range codexReviewerOptionsFromDetection(detected) {
		model, _ := option["model"].(string)
		if strings.HasSuffix(model, "-"+tier) {
			return model
		}
	}
	return ""
}

func validateReviewerInstalled(detected map[string]any, agentName, model string) error {
	if !oneOf(agentName, "agy", "claude", "codex", "opencode") {
		return errors.New("unsupported reviewer agent")
	}
	agents, _ := detected["agents"].(map[string]any)
	info, _ := agents[agentName].(map[string]any)
	if info["available"] != true {
		return fmt.Errorf("reviewer agent %s is not installed", agentName)
	}
	if agentName == "agy" && model != "" && model != agyReviewerModel() {
		return errors.New("selected Reviewer-1 model is not the pinned agy model")
	}
	return nil
}

func validateReviewerSelection(home, agentName, model, effort string) error {
	if agentName == "none" {
		return nil
	}
	o := options{reviewer: agentName, reviewerModel: model}
	if !oneOf(agentName, "agy", "claude", "codex", "opencode") {
		return errors.New("unsupported reviewer agent")
	}
	if model == "" && oneOf(agentName, "claude", "codex", "opencode") {
		return fmt.Errorf("reviewer model is required for %s", agentName)
	}
	if err := validateSelection(o, home); err != nil {
		return err
	}
	allowedEffort := false
	switch agentName {
	case "agy", "opencode":
		allowedEffort = effort == ""
	case "codex":
		allowedEffort = oneOf(effort, "low", "medium", "high", "xhigh")
	case "claude":
		allowedEffort = oneOf(effort, "low", "medium", "high", "xhigh", "max")
	}
	if !allowedEffort {
		return errors.New("reviewer effort is invalid for selected agent")
	}
	return nil
}

func agyReviewerModel() string {
	adapter, _ := review.ReviewerAdapter("agy")
	return adapter.AllowedModel()
}

func editReviewerConfig(text *string, values map[string]any, agentName, model, effort, nl string) error {
	roles, ok := values["roles"].(map[string]any)
	if !ok {
		if values["roles"] != nil {
			return errors.New("[roles] must be a table")
		}
		roles = map[string]any{}
		values["roles"] = roles
	}
	roles["reviewer"] = agentName
	*text = upsert(*text, "roles", "reviewer", tomlString(agentName), nl)
	*text = removeKey(*text, "reviewer", "selected", nl)
	*text = removeKey(*text, "reviewer", "executable", nl)
	for _, key := range []string{"reviewer1_agent", "reviewer1_model_requested", "reviewer1_effort", "reviewer1_executable"} {
		*text = removeKey(*text, "coding", key, nl)
	}
	reviewer, ok := values["reviewer"].(map[string]any)
	if !ok {
		reviewer = map[string]any{}
		values["reviewer"] = reviewer
	}
	delete(reviewer, "selected")
	delete(reviewer, "executable")
	coding, ok := values["coding"].(map[string]any)
	if ok {
		for _, key := range []string{"reviewer1_agent", "reviewer1_model_requested", "reviewer1_effort", "reviewer1_executable"} {
			delete(coding, key)
		}
	}
	if agentName == "none" {
		return nil
	}
	if model == "" {
		return nil
	}
	if agentName == "claude" && effort == "" {
		effort = "medium"
	}
	selection := map[string]any{"agent": agentName, "model": model}
	selectedFields := []string{fmt.Sprintf("agent = %s", tomlString(agentName)), fmt.Sprintf("model = %s", tomlString(model))}
	if effort != "" {
		selection["effort"] = effort
		selectedFields = append(selectedFields, fmt.Sprintf("effort = %s", tomlString(effort)))
	}
	reviewer["selected"] = selection
	*text = upsert(*text, "reviewer", "selected", "{ "+strings.Join(selectedFields, ", ")+" }", nl)
	if exe := reviewerExecutable(agentName); exe != "" {
		reviewer["executable"] = exe
		*text = upsert(*text, "reviewer", "executable", tomlString(exe), nl)
		if agentName == "claude" {
			if coding == nil {
				coding = map[string]any{}
				values["coding"] = coding
			}
			reviewerCoding := map[string]any{"reviewer1_agent": agentName, "reviewer1_model_requested": model, "reviewer1_effort": effort, "reviewer1_executable": exe}
			for key, value := range reviewerCoding {
				coding[key] = value
				*text = upsert(*text, "coding", key, tomlString(fmt.Sprint(value)), nl)
			}
		}
	}
	return nil
}

func removeKey(text, section, key, nl string) string {
	lines := strings.SplitAfter(text, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		match := sectionRE.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if match != nil {
			if start >= 0 {
				end = i
				break
			}
			if match[1] == section {
				start = i
			}
		}
	}
	if start < 0 {
		return text
	}
	for i := start + 1; i < end; i++ {
		if match := keyRE.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n")); match != nil && match[1] == key {
			lines = append(lines[:i], lines[i+1:]...)
			break
		}
	}
	return strings.Join(lines, "")
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func reviewerJSON(w io.Writer, value any) int {
	if json.NewEncoder(w).Encode(value) != nil {
		return 1
	}
	return 0
}
func reviewerError(w io.Writer, err error) int {
	_ = reviewerJSON(w, map[string]any{"error": err.Error()})
	return 1
}
