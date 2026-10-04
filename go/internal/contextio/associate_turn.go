package contextio

import (
	"os"
	"path/filepath"
	"reflect"
	"time"
	"unicode/utf8"
)

// AssociateTurn binds the current context journal to a task and project.
func AssociateTurn(stateRoot, sessionID string, taskID any, projectKey any) bool {
	if sessionID == "" || utf8.RuneCountInString(sessionID) > 512 {
		return false
	}
	task, ok := taskID.(string)
	if !ok || task == "" || utf8.RuneCountInString(task) > 128 {
		return false
	}
	key := sessionKey(sessionID)
	base := filepath.Join(stateRoot, "context_io")
	journal := filepath.Join(base, "turns", key+".json")
	lockPath := filepath.Join(base, "locks", key+".lock")
	contention := filepath.Join(base, "contention", key)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		markContention(contention)
		return false
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		markContention(contention)
		return false
	}
	defer f.Close()
	if info, _ := f.Stat(); info != nil && info.Size() == 0 {
		_, _ = f.WriteAt([]byte("0"), 0)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	acquired := false
	for time.Now().Before(deadline) {
		if tryLock(f) == nil {
			acquired = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !acquired {
		markContention(contention)
		return false
	}
	defer unlock(f)

	turn, loadErr := loadTurn(journal)
	if turn == nil {
		return false
	}
	consumeContention(turn, contention)
	if loadErr != "" || turn["task_association_known"] == true {
		return reflect.DeepEqual(turn["task_id"], taskID) && reflect.DeepEqual(turn["project_key"], projectKey)
	}
	turn["task_association_known"] = true
	turn["task_id"] = task
	turn["project_key"] = projectKey
	turn["codex_boundary_index"] = turn["read_count"]
	if err := writeTurn(journal, turn); err != nil {
		if isOSError(err) {
			markContention(contention)
		}
		return false
	}
	return true
}
