package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/hookactivity"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcpMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "wait" {
		os.Exit(waitMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "leases" {
		os.Exit(leasesMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "gear" {
		os.Exit(gearMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "plan" {
		os.Exit(planMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "code-map" {
		os.Exit(codeMapMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "optimize" {
		os.Exit(optimizeMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Exit(checkMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "mechanical-shadow" {
		os.Exit(mechanicalShadowMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(initMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "maintenance" {
		os.Exit(maintenanceMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && (os.Args[1] == "op" || os.Args[1] == "host" || os.Args[1] == "retire-worktree") {
		os.Exit(privateMain())
	}
	defer func() { _ = recover() }()
	started := time.Now()
	enforcementMode := len(os.Args) > 1 && os.Args[1] == "enforce"
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	if !utf8.Valid(data) {
		malformed("UnicodeDecodeError", enforcementMode)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		malformed("JSONDecodeError", enforcementMode)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		malformed("JSONDecodeError", enforcementMode)
		return
	}
	event, ok := value.(map[string]any)
	if !ok {
		if enforcementMode {
			writeJSON(map[string]any{})
		}
		return
	}
	switch event["hook_event_name"] {
	case "PostToolUse":
		if enforcementMode {
			response, failure := enforce.PostToolUse(event, stateRoot())
			if response == nil {
				response, failure = enforce.PostInternalError(event, stateRoot())
			}
			if response != nil {
				if failure != nil {
					_, _ = os.Stderr.WriteString("orchestrator hook internal error: " + errorName(failure) + "\n")
				}
				writeJSON(response)
			}
		}
	case "PostToolUseFailure":
		if enforcementMode {
			if response := enforce.PostToolUseFailure(event, stateRoot()); response != nil {
				writeJSON(response)
			}
		}
	case "PreToolUse":
		if event["tool_name"] == "Read" {
			_ = contextio.Read(event, started)
		} else if response, failure := enforce.Pre(event, stateRoot()); response != nil {
			if failure != nil {
				_, _ = os.Stderr.WriteString("orchestrator hook internal error: " + errorName(failure) + "\n")
			}
			writeJSON(response)
		}
	case "WriteUndeliveredMarker":
		if err := enforce.WriteUndeliveredMarker(stateRoot(), event); err != nil {
			_, _ = os.Stderr.WriteString("undelivered marker write failed: " + errorName(err) + "\n")
			os.Exit(1)
		}
		writeJSON(map[string]any{"written": true})
	case "SessionStart":
		handleSessionStart(event)
	case "UserPromptSubmit":
		if output := contextio.UserPromptSubmit(event, started); len(output) > 0 {
			if written, err := os.Stdout.Write(output); err == nil && written == len(output) {
				contextio.RecordAdviceWritten(event, output)
			}
		}
	case "PreCompact":
		contextio.PreCompact(event)
	case "Stop":
		if output := contextio.Stop(event, started); len(output) > 0 {
			_, _ = os.Stdout.Write(output)
		}
	}
	if eventName, ok := event["hook_event_name"].(string); ok {
		switch eventName {
		case "PostToolUse", "PostToolUseFailure", "PreToolUse", "SessionStart", "UserPromptSubmit", "PreCompact", "Stop":
			hookactivity.Record(stateRoot(), eventName, time.Now())
		}
	}
}

func stateRoot() string {
	return contextio.StateRoot()
}

func writeJSON(value any) {
	data, err := json.Marshal(value)
	if err == nil {
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
}

func malformed(kind string, enforcementMode bool) {
	if !enforcementMode {
		return
	}
	_, _ = os.Stderr.WriteString("orchestrator hook internal error: " + kind + ": malformed hook input\n")
	_, _ = os.Stdout.WriteString("{}\n")
}

func errorName(err error) string {
	return "RuntimeError: " + err.Error()
}
