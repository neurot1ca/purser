package contextio

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var safeContextTaskID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var safeContextMonth = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)

func stopTurn(event map[string]any, processStart time.Time) {
	stopStarted := time.Now()
	if event["stop_hook_active"] == true {
		return
	}
	key := sessionKey(event["session_id"])
	if key == "" {
		return
	}
	base := filepath.Join(stateRoot(), "context_io")
	journal := filepath.Join(base, "turns", key+".json")
	completed := filepath.Join(base, "completed", key)
	contention := filepath.Join(base, "contention", key)
	lockPath := filepath.Join(base, "locks", key+".lock")
	if os.MkdirAll(filepath.Dir(lockPath), 0700) != nil {
		return
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if info, _ := lock.Stat(); info != nil && info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("0"), 0)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if tryLock(lock) == nil {
			defer unlock(lock)
			if _, err := os.Stat(completed); err == nil {
				if _, err := os.Stat(journal); os.IsNotExist(err) {
					return
				}
			}
			turn, reason := loadTurn(journal)
			if turn == nil {
				if reason == "" {
					reason = "MISSING_USER_PROMPT"
				}
				turn = newTurn(reason)
			}
			consumeContention(turn, contention)
			frozen, ok := turn["final_event"].(map[string]any)
			if !ok {
				frozen = contextTurnEvent(turn, time.Since(stopStarted), time.Since(processStart), time.Now())
				turn["final_event"] = frozen
				if writeTurn(journal, turn) != nil {
					markContention(contention)
					return
				}
			}
			if !persistContextTurn(frozen) {
				return
			}
			if os.MkdirAll(filepath.Dir(completed), 0700) != nil {
				return
			}
			file, err := os.OpenFile(completed, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil && !os.IsExist(err) {
				return
			}
			if file != nil {
				_ = file.Close()
			}
			_ = os.Remove(journal)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	markContention(contention)
}

func contextTurnEvent(turn map[string]any, internal, wall time.Duration, started time.Time) map[string]any {
	observations, _ := turn["observations"].([]any)
	latency, _ := turn["latency"].([]any)
	latency = append(latency, []any{max(int64(0), internal.Microseconds()), max(int64(0), wall.Microseconds())})
	payload := map[string]any{
		"read_count": number(turn["read_count"]), "full_read_count": number(turn["full_read_count"]),
		"targeted_read_count": number(turn["targeted_read_count"]), "unknown_read_count": number(turn["unknown_read_count"]),
		"observed_bytes_total": 0, "exact_line_read_count": 0, "exact_line_count_total": 0,
		"lower_bound_line_read_count": 0, "lower_bound_line_count_total": 0,
		"oversized_read_count": 0, "binary_suspected_read_count": 0, "repeat_count": 0,
		"repeat_known": true, "task_association_known": turn["task_association_known"] == true,
		"hook_sample_count": len(latency), "internal_processing_us_total": 0, "internal_processing_us_max": 0,
		"hook_wall_time_us_total": 0, "hook_wall_time_us_max": 0,
	}
	for _, threshold := range []string{"200", "350", "500"} {
		payload["threshold_"+threshold+"_known_count"] = 0
		payload["threshold_"+threshold+"_candidate_count"] = 0
	}
	for _, item := range observations {
		observation, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if observation["bytes"] != nil {
			payload["observed_bytes_total"] = number(payload["observed_bytes_total"]) + number(observation["bytes"])
		}
		if observation["line_kind"] == "EXACT" {
			payload["exact_line_read_count"] = number(payload["exact_line_read_count"]) + 1
			payload["exact_line_count_total"] = number(payload["exact_line_count_total"]) + number(observation["line_value"])
		}
		if observation["line_kind"] == "LOWER_BOUND" {
			payload["lower_bound_line_read_count"] = number(payload["lower_bound_line_read_count"]) + 1
			payload["lower_bound_line_count_total"] = number(payload["lower_bound_line_count_total"]) + number(observation["line_value"])
		}
		for _, field := range []string{"oversized", "binary_suspected"} {
			if observation[field] == true {
				payload[field+"_read_count"] = number(payload[field+"_read_count"]) + 1
			}
		}
		if observation["repeat"] == true {
			payload["repeat_count"] = number(payload["repeat_count"]) + 1
		}
		if observation["repeat"] == nil {
			payload["repeat_known"] = false
		}
		thresholds, _ := observation["thresholds"].(map[string]any)
		for _, threshold := range []string{"200", "350", "500"} {
			value := thresholds[threshold]
			if value == true || value == false {
				key := "threshold_" + threshold + "_known_count"
				payload[key] = number(payload[key]) + 1
			}
			if value == true {
				key := "threshold_" + threshold + "_candidate_count"
				payload[key] = number(payload[key]) + 1
			}
		}
	}
	for _, sample := range latency {
		pair, ok := sample.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		for index, prefix := range []string{"internal_processing_us", "hook_wall_time_us"} {
			value := max(0, number(pair[index]))
			payload[prefix+"_total"] = number(payload[prefix+"_total"]) + value
			payload[prefix+"_max"] = max(number(payload[prefix+"_max"]), value)
		}
	}
	if turn["task_association_known"] == true && turn["codex_boundary_index"] != nil {
		boundary := number(turn["codex_boundary_index"])
		payload["pre_codex_read_count"] = min(boundary, number(turn["read_count"]))
		payload["post_codex_read_count"] = max(0, number(turn["read_count"])-boundary)
	}
	if reason := str(turn["unknown_reason"]); reason != "" {
		payload["unknown_reason"] = reason
	}
	name := []byte("context-io:" + str(turn["turn_nonce"]))
	namespace, _ := hex.DecodeString("6ba7b8119dad11d180b400c04fd430c8")
	hash := sha1.Sum(append(namespace, name...))
	hash[6] = hash[6]&0x0f | 0x50
	hash[8] = hash[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
	var taskID, projectKey any
	if turn["task_association_known"] == true {
		taskID, projectKey = turn["task_id"], turn["project_key"]
	}
	return map[string]any{"schema_version": 1, "event_id": id, "ts_utc": started.UTC().Format(time.RFC3339Nano), "event_type": "CONTEXT_IO_TURN_RECORDED", "task_id": taskID, "project_key": projectKey, "session_id": nil, "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil, "payload": payload}
}

func persistContextTurn(event map[string]any) bool {
	if event["event_type"] != "CONTEXT_IO_TURN_RECORDED" || number(event["schema_version"]) != 1 {
		return false
	}
	if id := str(event["event_id"]); len(id) != 36 || !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(id) {
		return false
	}
	if task := str(event["task_id"]); task != "" && !safeContextTaskID.MatchString(task) {
		return false
	}
	root := filepath.Join(stateRoot(), "telemetry", "events")
	if os.MkdirAll(root, 0700) != nil {
		return false
	}
	lock, err := os.OpenFile(filepath.Join(root, ".append.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if info, _ := lock.Stat(); info != nil && info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("0"), 0)
	}
	if tryLock(lock) != nil {
		return false
	}
	defer unlock(lock)
	stamp := str(event["ts_utc"])
	if len(stamp) < 7 || !safeContextMonth.MatchString(stamp[:7]) {
		return false
	}
	path := filepath.Join(root, stamp[:7], "session_unknown.jsonl")
	if !appendEventOnce(path, event) {
		return false
	}
	if task := str(event["task_id"]); task != "" {
		if !appendEventOnce(filepath.Join(root, "by_task", task+".jsonl"), event) {
			return false
		}
	}
	return true
}

func appendEventOnce(path string, event map[string]any) bool {
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return false
	}
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	if !repairOwnPartialRecord(path, data) {
		return false
	}
	if existing, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(existing)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			var record map[string]any
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				_ = existing.Close()
				return false
			}
			if record["event_id"] == event["event_id"] {
				_ = existing.Close()
				found, err := json.Marshal(record)
				return err == nil && bytes.Equal(found, data)
			}
		}
		if scanner.Err() != nil {
			_ = existing.Close()
			return false
		}
		if info, err := existing.Stat(); err != nil {
			_ = existing.Close()
			return false
		} else if info.Size() > 0 {
			last := []byte{0}
			if _, err := existing.ReadAt(last, info.Size()-1); err != nil || last[0] != '\n' {
				_ = existing.Close()
				return false
			}
		}
		_ = existing.Close()
	} else if !os.IsNotExist(err) {
		return false
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return false
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	return err == nil && closeErr == nil
}

func repairOwnPartialRecord(path string, data []byte) bool {
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err == nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return false
	}
	if last[0] == '\n' {
		return true
	}
	start := max(int64(0), info.Size()-int64(len(data)+1))
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil {
		return false
	}
	if boundary := bytes.LastIndexByte(tail, '\n'); boundary >= 0 {
		start += int64(boundary + 1)
		tail = tail[boundary+1:]
	} else if start != 0 {
		return false
	}
	if len(tail) == 0 || !bytes.HasPrefix(data, tail) {
		return false
	}
	if file.Truncate(start) != nil {
		return false
	}
	return file.Sync() == nil
}
