// Package hookactivity records when Purser hook events last ran.
package hookactivity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const fileName = "hook-activity.json"

var eventNames = map[string]bool{
	"PostToolUse":        true,
	"PostToolUseFailure": true,
	"PreToolUse":         true,
	"SessionStart":       true,
	"UserPromptSubmit":   true,
	"PreCompact":         true,
	"Stop":               true,
}

// Record updates an event timestamp at most once per hour. Errors are ignored
// so activity tracking cannot affect a hook response.
func Record(stateRoot, event string, now time.Time) {
	if !eventNames[event] || stateRoot == "" {
		return
	}

	path := filepath.Join(stateRoot, fileName)
	activity := Load(stateRoot)
	if lastSeen, ok := activity[event]; ok && lastSeen.After(now.Add(-time.Hour)) {
		return
	}
	activity[event] = now.UTC().Truncate(time.Second)
	data, err := json.Marshal(activity)
	if err != nil {
		return
	}
	if os.MkdirAll(stateRoot, 0700) != nil {
		return
	}
	temp, err := os.CreateTemp(stateRoot, ".hook-activity-*.tmp")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if temp.Chmod(0600) != nil {
		_ = temp.Close()
		return
	}
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return
	}
	_ = os.Rename(tempPath, path)
}

// Load returns valid timestamps from the activity file. Missing or unreadable
// files produce an empty map.
func Load(stateRoot string) map[string]time.Time {
	activity := map[string]time.Time{}
	data, err := os.ReadFile(filepath.Join(stateRoot, fileName))
	if err != nil {
		return activity
	}
	var stored map[string]string
	if json.Unmarshal(data, &stored) != nil {
		return activity
	}
	for event, value := range stored {
		if !eventNames[event] {
			continue
		}
		lastSeen, err := time.Parse(time.RFC3339, value)
		if err == nil {
			activity[event] = lastSeen.UTC()
		}
	}
	return activity
}
