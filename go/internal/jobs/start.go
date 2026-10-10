package jobs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
	"purser-hook/internal/retention"
)

type StartOptions struct {
	StateRoot      string
	Validation     StartValidationOptions
	CodexBin       any
	Launch         func(jobID string, generation int64, token string) error
	Await          func(jobID string, record *Record) map[string]any
	RecordDecision func(job map[string]any, decision, kind, sourceJobID string)
}

// Start writes the durable reservation before invoking the injected host launcher.
func Start(stateRoot string, args map[string]any, opt StartOptions) (map[string]any, *Record, error) {
	if opt.StateRoot == "" {
		opt.StateRoot = stateRoot
	}
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	await := func(id string, r *Record) map[string]any {
		if opt.Await != nil {
			return opt.Await(id, r)
		}
		if r == nil {
			return jobResult(map[string]any{"jobId": id, "state": "UNKNOWN_OR_IN_PROGRESS", "errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership was not acknowledged"})
		}
		m := recordMap(r)
		return jobResult(map[string]any{"jobId": id, "state": m["state"], "acceptedAt": m["acceptedAt"], "deadlineAt": m["deadlineAt"]})
	}
	validation := opt.Validation
	validation.LookupExisting = func(id string) map[string]any {
		if r := readJob(dir, id); r != nil {
			return await(id, r)
		}
		return nil
	}
	req, early := ValidateStart(args, validation)
	if early != nil {
		return early, nil, nil
	}
	if req.ExistingResult != nil {
		existing := readJob(dir, req.JobID)
		if existing != nil {
			if err := persistTaskAttempt(opt.StateRoot, recordMap(existing), nonemptyString(args["cwd"])); err != nil {
				log.Printf("task attempt persistence failed for replayed job %s: %v", req.JobID, err)
			}
		}
		return req.ExistingResult, existing, nil
	}
	var thread map[string]any
	var sendBackFrom *Record
	if req.Mode == "reply" {
		id, ok := args["threadId"].(string)
		if !ok {
			return startFailure("UNKNOWN_THREAD", "Thread is unknown to this server"), nil, nil
		}
		thread = LoadRecord(filepath.Join(stateRoot, "codex_exec_mcp", "threads"), id)
		if thread == nil {
			return startFailure("UNKNOWN_THREAD", "Thread is unknown to this server"), nil, nil
		}
		retired := false
		if worktree := nonemptyString(thread["worktreePath"]); worktree != "" {
			isRetiring, retirementErr := retention.IsWorktreeRetiring(stateRoot, worktree)
			info, worktreeErr := os.Stat(worktree)
			retired = worktreeErr != nil || !info.IsDir() || retirementErr != nil || isRetiring
		}
		sendBackFrom = unappliedThreadJob(dir, id)
		var retiredRepo string
		if sendBackFrom != nil && !retired {
			prior := recordMap(sendBackFrom)
			if worktree := nonemptyString(prior["worktreePath"]); worktree != "" {
				isRetiring, retirementErr := retention.IsWorktreeRetiring(stateRoot, worktree)
				info, worktreeErr := os.Stat(worktree)
				retired = worktreeErr != nil || !info.IsDir() || retirementErr != nil || isRetiring
				if retired {
					retiredRepo = nonemptyString(prior["repoRoot"])
				}
			}
		}
		if retired {
			sendBackFrom = nil
			if nonemptyString(thread["repoRoot"]) == "" && retiredRepo != "" {
				thread["repoRoot"] = retiredRepo
			}
			for _, key := range []string{"worktreePath", "snapshot", "baselineSnapshot", "head"} {
				delete(thread, key)
			}
			if repo := nonemptyString(thread["repoRoot"]); repo != "" {
				thread["cwd"] = repo
			}
			if err := SaveRecord(filepath.Join(stateRoot, "codex_exec_mcp", "threads"), thread); err != nil {
				return nil, nil, err
			}
			args["worktree"] = true
			args["cwd"] = thread["cwd"]
		}
		if sendBackFrom != nil {
			prior := recordMap(sendBackFrom)
			for _, key := range []string{"worktreePath", "repoRoot"} {
				if nonemptyString(thread[key]) == "" && nonemptyString(prior[key]) != "" {
					thread[key] = prior[key]
				}
			}
			if snapshot, ok := prior["snapshot"].(string); ok {
				thread["snapshot"], thread["baselineSnapshot"] = snapshot, snapshot
			}
			if worktree, ok := thread["worktreePath"].(string); ok && worktree != "" {
				repo, _ := thread["repoRoot"].(string)
				base, _ := thread["baselineSnapshot"].(string)
				if base == "" {
					base, _ = thread["snapshot"].(string)
				}
				if info, e := os.Stat(worktree); e == nil && info.IsDir() && base != "" && repo != "" {
					if p, e := filepath.EvalSymlinks(repo); e == nil {
						repo = p
					}
					if p, e := filepath.EvalSymlinks(worktree); e == nil {
						worktree = p
					}
					matches, e := ledger.AppliedReplyMatches(repo, worktree, base)
					if e == nil && matches {
						sendBackFrom = nil
					}
				}
			}
		}
		if sendBackFrom == nil {
			if worktree, ok := thread["worktreePath"].(string); ok && worktree != "" {
				repo, _ := thread["repoRoot"].(string)
				if repo != "" {
					repo, _ = filepath.Abs(repo)
					worktree, _ = filepath.Abs(worktree)
					if p, e := filepath.EvalSymlinks(repo); e == nil {
						repo = p
					}
					if p, e := filepath.EvalSymlinks(worktree); e == nil {
						worktree = p
					}
					sum := sha256.Sum256([]byte(repo))
					unlock, e := lease.Lock(filepath.Join(filepath.Dir(dir), "apply", hex.EncodeToString(sum[:])+".lock"))
					if e == nil {
						head, snapshot, rebaseErr := ledger.RebaseAppliedReply(repo, worktree)
						_ = unlock()
						if rebaseErr != nil {
							return startFailure("WORKTREE_REBASE_FAILED", "Applied thread worktree could not be rebased to the current checkout"), nil, nil
						}
						thread["snapshot"], thread["baselineSnapshot"], thread["head"] = snapshot, snapshot, head
					} else {
						return startFailure("WORKTREE_REBASE_FAILED", "Applied thread worktree could not be rebased to the current checkout"), nil, nil
					}
				}
			}
		}
		if nonemptyString(args["cwd"]) == "" && nonemptyString(thread["worktreePath"]) == "" {
			repo := nonemptyString(thread["repoRoot"])
			if repo == "" {
				return startFailure("WORKTREE_SNAPSHOT_FAILED", "Repository root could not be resolved for Codex launch"), nil, nil
			}
			args["cwd"] = repo
		}
	}
	var signal, owner [16]byte
	if _, err := rand.Read(signal[:]); err != nil {
		return nil, nil, err
	}
	if _, err := rand.Read(owner[:]); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, nil, err
		}
	}
	if err := os.Mkdir(filepath.Join(dir, req.JobID+".lock"), 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			if r := readJob(dir, req.JobID); r != nil {
				return await(req.JobID, r), r, nil
			}
			return await(req.JobID, nil), nil, nil
		}
		return nil, nil, err
	}
	accepted := float64(time.Now().UnixNano()) / 1e9
	job := map[string]any{"jobId": req.JobID, "call": req.Call, "state": "QUEUED", "acceptedAt": accepted, "deadlineAt": accepted + req.TimeoutSeconds, "signalToken": hex.EncodeToString(signal[:]), "generation": 1, "ownershipToken": hex.EncodeToString(owner[:])}
	planRepository := ""
	if thread != nil {
		for _, k := range []string{"worktreePath", "snapshot", "head", "repoRoot", "baselineSnapshot"} {
			if v, ok := thread[k]; ok {
				job[k] = v
			}
		}
		if p, ok := job["worktreePath"].(string); ok && p != "" {
			threadID, _ := args["threadId"].(string)
			attached, attachErr := retention.AttachWorktree(stateRoot, p, req.JobID, threadID)
			if attachErr != nil || !attached {
				return startFailure("WORKTREE_RETIRING", "Thread worktree is being retired"), nil, nil
			}
			if job["baselineSnapshot"] == nil {
				job["baselineSnapshot"] = job["snapshot"]
			}
			job["snapshot"] = job["baselineSnapshot"]
		}
	}
	if args["leaseQueued"] == true {
		job["leaseQueued"] = true
	}
	for _, k := range []string{"taskId", "route", "etaMinutes"} {
		if v, ok := args[k]; ok {
			job[k] = v
		}
	}
	if sendBackFrom != nil {
		if priorTask := nonemptyString(recordMap(sendBackFrom)["taskId"]); priorTask != "" && priorTask == nonemptyString(args["taskId"]) {
			job["predecessorJobId"] = nonemptyString(recordMap(sendBackFrom)["jobId"])
		}
	}
	for _, p := range [][2]string{{"model", "jobModel"}, {"effort", "jobEffort"}, {"label", "jobLabel"}, {"phase", "jobPhase"}} {
		v, ok := args[p[1]]
		if !ok {
			v, ok = args[p[0]]
		}
		if ok && v != nil {
			job[p[0]] = v
		}
	}
	if nonemptyString(job["repoRoot"]) == "" && nonemptyString(args["cwd"]) != "" {
		job["repoRoot"] = nonemptyString(args["cwd"])
	}
	if readyCheck(args) {
		job["readinessCheck"] = readinessValue(args)
	}
	if taskID := nonemptyString(job["taskId"]); req.Mode == "new" && taskID != "" {
		repository := nonemptyString(args["cwd"])
		if repository != "" && contextio.ProjectRoot(map[string]any{"cwd": repository}) != "" {
			planRepository = repository
		}
	}
	codexBin := opt.CodexBin
	if req.Agent != "codex" {
		codexBin = nil
	}
	launch, err := marshalPythonJSON(map[string]any{"name": req.Name, "args": args, "codexBin": codexBin, "agent": req.Agent})
	if err != nil {
		return nil, nil, err
	}
	if err = atomicWrite(filepath.Join(dir, req.JobID+".launch"), launch); err != nil {
		return nil, nil, err
	}
	data, err := marshalPythonJSON(job)
	if err != nil {
		return nil, nil, err
	}
	r, err := Decode(data)
	if err != nil {
		return nil, nil, err
	}
	if err = Write(filepath.Join(dir, req.JobID+".json"), r); err != nil {
		return nil, nil, err
	}
	if err = persistTaskAttempt(opt.StateRoot, recordMap(r), nonemptyString(args["cwd"])); err != nil {
		log.Printf("task attempt persistence failed for job %s: %v", req.JobID, err)
	}
	if sendBackFrom != nil {
		prior := recordMap(sendBackFrom)
		if terminalJobState(nonemptyString(prior["state"])) && prior["applyState"] != "applied" {
			if oldID := nonemptyString(prior["jobId"]); oldID != "" {
				lease.ReleaseJobScopeLease(stateRoot, oldID)
			}
		}
	}
	if planRepository != "" {
		taskID := nonemptyString(job["taskId"])
		_, _, _ = contextio.BindPlanJob(stateRoot, planRepository, taskID, req.JobID)
	}
	if req.Mode == "new" && nonemptyString(job["taskId"]) != "" {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if id == req.JobID {
				continue
			}
			lock, e := AcquireOwnership(dir, id)
			if e != nil {
				continue
			}
			earlier, e := Read(filepath.Join(dir, name))
			if e == nil {
				m := recordMap(earlier)
				if m["state"] == "SUCCEEDED" && m["taskId"] == job["taskId"] && nonemptyString(m["worktreePath"]) != "" && m["applyState"] != "applied" && !truthy(m["decisionRecorded"]) {
					recordStartDecision(opt, m, "rejected", "redispatch", req.JobID)
					if e = earlier.Set("decisionRecorded", true); e == nil {
						if e = Write(filepath.Join(dir, name), earlier); e != nil {
							_ = lock.Release()
							return nil, r, e
						}
						if e = persistRejectedTaskAttempt(opt.StateRoot, recordMap(earlier)); e != nil {
							log.Printf("task attempt persistence failed for redispatch %s: %v", nonemptyString(m["jobId"]), e)
						}
					}
				}
			}
			_ = lock.Release()
		}
	}
	if opt.Launch != nil {
		if err = opt.Launch(req.JobID, 1, job["ownershipToken"].(string)); err != nil {
			return jobResult(map[string]any{"jobId": req.JobID, "state": "UNKNOWN_OR_IN_PROGRESS", "errorCode": "PROCESS_UNCONFIRMED", "message": "Host launch unconfirmed"}), r, nil
		}
	}
	if sendBackFrom != nil {
		var oldID string
		sendBackFrom.Get("jobId", &oldID)
		if oldID != "" {
			lock, e := AcquireOwnership(dir, oldID)
			if e == nil {
				current := readJob(dir, oldID)
				if current != nil {
					m := recordMap(current)
					recordStartDecision(opt, m, "send_back", "reply", req.JobID)
					if nonemptyString(m["taskId"]) != "" {
						_ = current.Set("decisionRecorded", true)
						_ = Write(filepath.Join(dir, oldID+".json"), current)
					}
				}
				_ = lock.Release()
			}
		}
	}
	return await(req.JobID, r), r, nil
}

func persistTaskAttempt(root string, job map[string]any, repository string) error {
	taskID := nonemptyString(job["taskId"])
	if repository == "" {
		repository = nonemptyString(job["repoRoot"])
	}
	if repository == "" {
		repository = nonemptyString(job["cwd"])
	}
	if taskID == "" || repository == "" {
		return nil
	}
	return enforce.SaveTaskAttempt(root, enforce.TaskAttempt{
		ProjectKey: contextio.ProjectKey(repository), TaskID: taskID,
		LatestJobID: nonemptyString(job["jobId"]), PredecessorJobID: nonemptyString(job["predecessorJobId"]),
		SendBackUsed: nonemptyString(job["predecessorJobId"]) != "",
	})
}

func persistRejectedTaskAttempt(root string, job map[string]any) error {
	taskID, jobID := nonemptyString(job["taskId"]), nonemptyString(job["jobId"])
	repository := nonemptyString(job["repoRoot"])
	if taskID == "" || jobID == "" || repository == "" || !truthy(job["decisionRecorded"]) {
		return nil
	}
	project := contextio.ProjectKey(repository)
	previous, status, err := enforce.LoadTaskAttempt(root, project, taskID)
	if err != nil || status == enforce.TaskAttemptCorrupt {
		if err != nil {
			return err
		}
		return errors.New("task attempt record is corrupt")
	}
	if !previous.SendBackUsed {
		return nil
	}
	var failure *enforce.TaskFailureEvidence
	if lunaImplementationAttempt(job) {
		failure = &enforce.TaskFailureEvidence{JobID: jobID, Category: "rejected_after_sendback"}
	}
	return enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: project, TaskID: taskID,
		TerminalDisposition: "rejected", Failure: failure})
}

func unappliedThreadJob(dir, thread string) *Record {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var latest *Record
	best := -1.0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		r, e := Read(filepath.Join(dir, entry.Name()))
		if e != nil {
			continue
		}
		m := recordMap(r)
		if m["threadId"] != thread || m["state"] != "SUCCEEDED" || m["applyState"] == "applied" || nonemptyString(m["worktreePath"]) == "" {
			continue
		}
		if retired, e := retention.IsWorktreeRetiring(filepath.Dir(filepath.Dir(dir)), nonemptyString(m["worktreePath"])); e != nil || retired {
			continue
		}
		stamp, ok := m["finishedAt"].(float64)
		if !ok {
			stamp, _ = m["acceptedAt"].(float64)
		}
		if latest == nil || stamp > best {
			latest, best = r, stamp
		}
	}
	return latest
}
func recordStartDecision(opt StartOptions, job map[string]any, decision, kind, source string) {
	if nonemptyString(job["taskId"]) == "" {
		return
	}
	if opt.RecordDecision != nil {
		opt.RecordDecision(job, decision, kind, source)
	} else {
		enforce.RecordClaudeDecision(opt.StateRoot, job, decision, kind, source)
	}
}
func truthy(v any) bool {
	if v == nil || v == false || v == "" {
		return false
	}
	return true
}

func startFailure(code, message string) map[string]any {
	return jobResult(map[string]any{"state": "FAILED", "errorCode": code, "message": message})
}
func readJob(dir, id string) *Record {
	r, e := Read(filepath.Join(dir, id+".json"))
	if e != nil {
		return nil
	}
	var got string
	if !r.Get("jobId", &got) || got != id {
		return nil
	}
	return r
}
func recordMap(r *Record) map[string]any {
	b, _ := r.Bytes()
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
func readyCheck(a map[string]any) bool {
	c, ok := a["readinessCriteria"].(map[string]any)
	if !ok || len(c) != 2 || a["readinessGrounded"] != true {
		return false
	}
	o, x := c["objective"].(string)
	v, y := c["acceptance_criteria"].(string)
	g, z := a["readinessAgent"].(string)
	_, m := a["readinessModel"].(string)
	return x && y && len(o)+len(v) <= 20000 && z && (g == "agy" || g == "off") && m
}
func readinessValue(a map[string]any) map[string]any {
	c := a["readinessCriteria"].(map[string]any)
	return map[string]any{"grounded": true, "objective": c["objective"], "acceptance_criteria": c["acceptance_criteria"], "agent": a["readinessAgent"], "model": a["readinessModel"]}
}
