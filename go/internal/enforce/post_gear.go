package enforce

import (
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/policy"
)

var claimPostGearAdvice = contextio.ClaimGearAdvice

func postGearAdvice(root string, event, pending, loaded, response map[string]any, durationMS float64, completedJob string) {
	defer func() { _ = recover() }()
	if pendingAgent(pending) != "codex" {
		return
	}
	route := stringValue(pending["route"])
	configRoutes := mapValue(loaded["routes"])
	routeConfig := mapValue(configRoutes[route])
	gearRoutes := policy.GearEligibleRoutes(loaded)
	eligible := false
	for _, name := range gearRoutes {
		if name == route {
			eligible = true
			break
		}
	}
	if !eligible || routeConfig == nil {
		return
	}
	stateRoot := defaultPreStateRoot(root)
	key := pendingProjectKey(pending)
	gearUp, err := contextio.Gear(stateRoot, key)
	if err != nil {
		return
	}
	quotaHome := os.Getenv("CODEX_HOME")
	if quotaHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return
		}
		quotaHome = filepath.Join(home, ".codex")
	}
	now := time.Now().UTC()
	quota, ok := contextio.LatestQuota(quotaHome, now)
	if !ok {
		return
	}
	input := contextio.GearAdviceInput{GearUp: gearUp, Quota: quota, Now: now, Route: route,
		Eligible: true, DurationMS: durationMS, Implementer: "codex-main", CompletedJob: completedJob,
		Samples: contextio.RouteDurationSamples(stateRoot, key, route)}
	result := contextio.EvaluateGearAdvice(input)
	if result.Advice == contextio.GearAdviceNone {
		return
	}
	source := stringValue(quota["source_identity"])
	claimed, err := claimPostGearAdvice(stateRoot, source, result.WindowID, result.Advice)
	if err != nil || !claimed {
		return
	}
	_ = contextio.QueueGearAdviceRecovery(stateRoot, key, input, result.Advice, source, result.WindowID)
	// Re-read mutable evidence after the shared claim so a gear/quota change
	// between evaluation and delivery suppresses stale advice.
	currentGear, gearErr := contextio.Gear(stateRoot, key)
	currentQuota, quotaOK := contextio.LatestQuota(quotaHome, time.Now().UTC())
	if gearErr != nil || !quotaOK {
		contextio.ClearGearAdviceRecovery(stateRoot, source, result.WindowID)
		return
	}
	input.GearUp, input.Now, input.Quota = currentGear, time.Now().UTC(), currentQuota
	current := contextio.EvaluateGearAdvice(input)
	if current.Advice != result.Advice || current.WindowID != result.WindowID || currentQuota["source_identity"] != source {
		contextio.ClearGearAdviceRecovery(stateRoot, source, result.WindowID)
		return
	}
	context := contextio.GearAdviceInstruction(result.Advice)
	if response == nil {
		return
	}
	output := mapValue(response["hookSpecificOutput"])
	if output == nil {
		output = map[string]any{"hookEventName": "PostToolUse"}
		response["hookSpecificOutput"] = output
	}
	if context == "" {
		return
	}
	if prior := stringValue(output["additionalContext"]); prior != "" {
		output["additionalContext"] = prior + "\n" + context
	} else {
		output["additionalContext"] = context
	}
	if stringValue(pending["async_call"]) == "" {
		contextio.ClearGearAdviceRecovery(stateRoot, source, result.WindowID)
	}
}

func workerDuration(startedAt, finishedAt any) float64 {
	start, startOK := startedAt.(float64)
	finish, finishOK := finishedAt.(float64)
	if !startOK || !finishOK || start <= 0 || finish <= start {
		return 0
	}
	return (finish - start) * 1000
}
