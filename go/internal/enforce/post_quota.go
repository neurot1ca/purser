package enforce

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const postQuotaTailBytes = 256 << 10

func postReadQuota(threadID string) map[string]any {
	if threadID == "" || strings.ContainsAny(threadID, `/\\*?[]`) {
		return nil
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".codex")
	}
	now := time.Now()
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		folder := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		paths, _ := filepath.Glob(filepath.Join(folder, "rollout-*-"+threadID+".jsonl"))
		if len(paths) > 0 {
			return postReadQuotaPath(paths[0])
		}
	}
	for _, year := range []int{now.Year(), now.Year() - 1} {
		pattern := filepath.Join(home, "sessions", strconv.Itoa(year), "[0-1][0-9]", "[0-3][0-9]", "rollout-*-"+threadID+".jsonl")
		paths, _ := filepath.Glob(pattern)
		if len(paths) > 0 {
			return postReadQuotaPath(paths[0])
		}
	}
	return nil
}

// ReadQuota returns the quota snapshot for a Codex rollout thread.
func ReadQuota(threadID string) map[string]any { return postReadQuota(threadID) }

func postReadQuotaPath(path string) map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	offset := info.Size() - postQuotaTailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err = file.Seek(offset, 0); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, postQuotaTailBytes))
	if err != nil {
		return nil
	}
	if offset > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var event map[string]any
		if json.Unmarshal([]byte(lines[i]), &event) != nil {
			continue
		}
		payload := mapValue(event["payload"])
		limits := mapValue(payload["rate_limits"])
		if limits == nil {
			continue
		}
		result := map[string]any{}
		for _, item := range []struct {
			source, label string
			minutes       int
		}{{"primary", "5h", 300}, {"secondary", "7d", 10080}} {
			window := mapValue(limits[item.source])
			minutes, valid := pythonInt(window["window_minutes"])
			if !valid || minutes != item.minutes {
				continue
			}
			used, usedOK := window["used_percent"].(float64)
			reset, resetOK := window["resets_at"].(float64)
			if !usedOK || !resetOK || math.IsNaN(used) || math.IsInf(used, 0) || math.IsNaN(reset) || math.IsInf(reset, 0) {
				continue
			}
			result[item.label+"_used_pct"] = used
			at := time.UnixMicro(int64(math.Round(reset * 1e6))).UTC()
			layout := "2006-01-02T15:04:05Z"
			if at.Nanosecond() != 0 {
				layout = "2006-01-02T15:04:05.000000Z"
			}
			result[item.label+"_resets_at"] = at.Format(layout)
		}
		if stamp, ok := event["timestamp"].(string); ok {
			result["snapshot_at"] = stamp
			return result
		}
		if len(result) > 0 {
			return result
		}
	}
	return nil
}

func postQuotaSystemMessage(value any) string {
	quota := mapValue(value)
	if quota == nil {
		return ""
	}
	parts := []string{}
	for _, item := range []struct{ window, reset, used string }{{"5h", "5h_resets_at", "5h_used_pct"}, {"7d", "7d_resets_at", "7d_used_pct"}} {
		used, ok := quota[item.used].(float64)
		if !ok || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		reset, ok := quota[item.reset].(string)
		if !ok || reset == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, reset)
		if err != nil {
			continue
		}
		local := parsed.Local()
		left := int(math.RoundToEven(100 - used))
		if left < 0 {
			left = 0
		}
		if left > 100 {
			left = 100
		}
		label := local.Format("15:04")
		if item.window == "7d" {
			label = local.Format("Jan 2")
		}
		parts = append(parts, item.window+" "+strconv.Itoa(left)+"% (resets "+label+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return "⚓ **Purser:** Codex quota left: " + strings.Join(parts, " · ")
}

func appendPostQuotaSystemMessage(response map[string]any, quota any) {
	message := postQuotaSystemMessage(quota)
	if message == "" {
		return
	}
	if existing := stringValue(response["systemMessage"]); existing != "" {
		response["systemMessage"] = existing + "\n" + message
	} else {
		response["systemMessage"] = message
	}
}
