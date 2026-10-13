package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"purser-hook/internal/obsidian"
)

var changelogHeader = regexp.MustCompile(`(?m)^##\s+(\S+)\s+—\s+\d{4}-\d{2}-\d{2}\s*$`)
var changelogBullet = regexp.MustCompile(`^\s*[-*+]\s+`)

func sessionUpdateContext(session, repository string, state map[string]any) string {
	var notices []string
	if notice := mechanicalLeaderContext(session, repository, state); notice != "" {
		notices = append(notices, notice)
	}
	if notice := pluginVersionContext(session, repository, state); notice != "" {
		notices = append(notices, notice)
	}
	return strings.Join(notices, "\n")
}

func pluginVersionContext(session, repository string, state map[string]any) string {
	manifest, err := os.ReadFile(filepath.Join(obsidian.PluginRoot(), ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	var plugin map[string]any
	if json.Unmarshal(manifest, &plugin) != nil {
		return ""
	}
	version, ok := plugin["version"].(string)
	if !ok || version == "" {
		return ""
	}
	versionPath := filepath.Join(stateRoot(), "plugin-version.json")
	data, err := os.ReadFile(versionPath)
	announced := ""
	if err == nil {
		var value map[string]any
		if json.Unmarshal(data, &value) == nil {
			announced, _ = value["announced_version"].(string)
		}
	}
	if announced == "" {
		_ = writeJSONAtomically(versionPath, map[string]any{"announced_version": version})
		return ""
	}
	if announced == version || state == nil || repository == "" || session == "" {
		return ""
	}
	changelog, err := os.ReadFile(filepath.Join(obsidian.PluginRoot(), "CHANGELOG.md"))
	if err != nil {
		return ""
	}
	text := string(changelog)
	match := changelogHeader.FindStringSubmatchIndex(text)
	for match != nil && text[match[2]:match[3]] != version {
		text = text[match[1]:]
		match = changelogHeader.FindStringSubmatchIndex(text)
	}
	if match == nil {
		return ""
	}
	section := text[match[1]:]
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	var bullets []string
	for _, line := range strings.Split(section, "\n") {
		if changelogBullet.MatchString(line) {
			bullets = append(bullets, strings.TrimSpace(line))
		}
	}
	if len(bullets) == 0 {
		return ""
	}
	pending, _ := state["version_pending"].(map[string]any)
	delivered := pending != nil && pending["version"] == version && pending["delivered"] == true
	state["version_pending"] = map[string]any{"version": version, "blocked": false, "delivered": delivered}
	if delivered {
		return ""
	}
	state["version_pending"].(map[string]any)["delivered"] = true
	_ = writeJSONAtomically(versionPath, map[string]any{"announced_version": version})
	return "On your next reply, put exactly one notice line first, then handle the user's request in full; the notice never replaces the task. Render the notice in the user's language, translating from English, in this form: " +
		"`> ⚓ **Purser upgraded to " + version + ":** <one short sentence summarizing the bullets>`. " +
		"Keep ⚓ and " + version + ". " +
		"Source bullets (summarize them in one short sentence):\n" + strings.Join(bullets, "\n")
}

func mechanicalLeaderContext(session, repository string, state map[string]any) string {
	if state == nil || repository == "" || session == "" {
		return ""
	}
	path := filepath.Join(stateRoot(), "opencode", "leader.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var leader struct {
		Model       *string      `json:"model"`
		Successes   *json.Number `json:"successes"`
		SampleCount *json.Number `json:"sample_count"`
		Announced   *bool        `json:"announced"`
	}
	if json.Unmarshal(data, &leader) != nil || leader.Announced == nil || *leader.Announced {
		return ""
	}
	if leader.Model == nil || *leader.Model == "" || leader.Successes == nil || leader.SampleCount == nil {
		return ""
	}
	model := *leader.Model
	successes64, successesErr := leader.Successes.Int64()
	sampleCount64, sampleCountErr := leader.SampleCount.Int64()
	if successesErr != nil || sampleCountErr != nil {
		return ""
	}
	successes, sampleCount := int(successes64), int(sampleCount64)
	pending, _ := state["leader_pending"].(map[string]any)
	delivered := pending != nil && pending["model"] == model && pending["delivered"] == true
	state["leader_pending"] = map[string]any{"model": model, "successes": successes,
		"sample_count": sampleCount, "blocked": false, "delivered": delivered}
	if delivered {
		return ""
	}
	state["leader_pending"].(map[string]any)["delivered"] = true
	return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged, in this form: " +
		"> ⚓ **Free model ranking changed:** `" + model + "` succeeded in " + strconv.Itoa(successes) + " of its last " + strconv.Itoa(sampleCount) +
		" attempts and moved to the top of the ranking. Mechanical tasks will go to this model first."
}
