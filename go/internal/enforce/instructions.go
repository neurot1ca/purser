package enforce

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const independenceInstructions = "Codex must not blindly accept Claude's proposed solution. It should independently verify the relevant repository state and project rules, prefer the smallest correct implementation, and explicitly flag any instruction that would introduce a regression, unnecessary complexity, scope expansion, or conflict with existing contracts."

const workerValidationInstruction = "Worker validation: End your final report with exactly one `VALIDATION:` JSON line. Use only these keys: `artifacts`, `commands`, `exit_codes`, `source`, `status`, `summary`. Set `source` to `WORKER_REPORTED`; set `status` to `PASS`, `FAIL`, or `UNKNOWN`; list the commands actually run in `commands` and matching integer `exit_codes`. List only final validation runs in `commands`; put any deliberate red check in the report prose, never in the `VALIDATION:` line. Do not emit another validation line."

const focusedTestsInstruction = "Run focused tests for what you changed; do not run the full test suite unless the task asks for it."
const fullSuiteTestsInstruction = "Run focused tests while working, then run the full test suite once before your final report."
const smallestChangeInstruction = "Make the smallest change that meets the criteria; add no speculative helpers, abstractions, flags, or refactors."
const unrelatedHangInstruction = "If an unrelated test hangs or runs far too long, stop it, report it as skipped, and continue."

var (
	fullSuitePattern    = regexp.MustCompile(`(?i)\b(?:run|execute)\s+(?:the\s+)?(full\s+(?:test\s+)?suite|whole\s+test\s+suite|all\s+tests)\b`)
	negatedSuitePattern = regexp.MustCompile(`(?i)\b(?:(?:do\s+not|don't|never|avoid)\s+(?:run|execute)\s+|(?:no|without)\s+)(the\s+)?(full\s+(test\s+)?suite|whole\s+test\s+suite|all\s+tests)\b`)
	claudeSuitePattern  = regexp.MustCompile(`(?i)Claude\s+runs\s+(the\s+full\s+suite|it)\s+once\s+(at|before)\s+acceptance`)
)

// ClaudeRunsFullSuiteAtAcceptance mirrors the repository-level Python oracle.
func ClaudeRunsFullSuiteAtAcceptance(repository string) bool {
	if repository == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(repository, "AGENTS.md"))
	return err == nil && claudeSuitePattern.Match(data)
}

// AsksWorkerToRunFullSuite keeps each positive match unless its own preceding
// 32 characters negate running the matching suite.
func AsksWorkerToRunFullSuite(prompt string) bool {
	return WorkerFullSuiteRequestPhrase(prompt) != ""
}

func WorkerFullSuiteRequestPhrase(prompt string) string {
	runes := []rune(prompt)
	for _, match := range fullSuitePattern.FindAllStringIndex(prompt, -1) {
		start, end := byteIndexToRune(runes, match[0]), byteIndexToRune(runes, match[1])
		if !pythonWordBoundary(runes, start) || !pythonWordBoundary(runes, end) {
			continue
		}
		windowStart := start - 32
		if windowStart < 0 {
			windowStart = 0
		}
		window := string(runes[windowStart:end])
		negated := negatedSuitePattern.MatchString(window)
		if !negated {
			return string(runes[start:end])
		}
	}
	return ""
}

func byteIndexToRune(value []rune, byteIndex int) int {
	// Go regexp indexes bytes. Matching ASCII terms makes the conversion safe
	// via the prefix byte count without changing Python's character window.
	bytes := 0
	for i, r := range value {
		if bytes >= byteIndex {
			return i
		}
		bytes += len(string(r))
	}
	return len(value)
}

func pythonWordBoundary(value []rune, at int) bool {
	left, right := false, false
	if at > 0 {
		left = pythonWordRune(value[at-1])
	}
	if at < len(value) {
		right = pythonWordRune(value[at])
	}
	return left != right
}

func pythonWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// AppendWorkerValidationInstruction preserves the caller's exact text and
// mirrors Python's full and partial idempotence rules.
func AppendWorkerValidationInstruction(prompt string, fullSuiteRequired bool) string {
	testInstruction := focusedTestsInstruction
	if fullSuiteRequired {
		testInstruction = fullSuiteTestsInstruction
	}
	additions := []string{testInstruction, smallestChangeInstruction, unrelatedHangInstruction}
	instruction := strings.Join(append([]string{workerValidationInstruction}, additions...), " ")
	if strings.Contains(prompt, instruction) {
		return prompt
	}
	if strings.Contains(prompt, workerValidationInstruction) {
		missing := make([]string, 0, len(additions))
		for _, sentence := range additions {
			if !strings.Contains(prompt, sentence) {
				missing = append(missing, sentence)
			}
		}
		if len(missing) == 0 {
			return prompt
		}
		separator := " "
		if prompt == "" || strings.HasSuffix(prompt, "\n") {
			separator = ""
		}
		return prompt + separator + strings.Join(missing, " ")
	}
	separator := "\n"
	if prompt == "" || strings.HasSuffix(prompt, "\n") {
		separator = ""
	}
	return prompt + separator + instruction
}

// RewriteInstructionInput applies the instruction-related portion of Codex
// Pre's tool-input rewrite, leaving caller-supplied values untouched otherwise.
func RewriteInstructionInput(input map[string]any, phase string, newCall bool, agent string, fullSuiteRequired bool) map[string]any {
	updated := make(map[string]any, len(input)+1)
	for key, value := range input {
		updated[key] = value
	}
	if prompt, ok := updated["prompt"].(string); ok && phase == "implementation" {
		updated["prompt"] = AppendWorkerValidationInstruction(prompt, fullSuiteRequired)
	}
	if phase == "plan" && newCall {
		if config, ok := updated["config"].(map[string]any); ok {
			clean := make(map[string]any, len(config))
			for key, value := range config {
				if key != "sandbox_mode" && key != "approval_policy" && key != "sandbox_workspace_write" {
					clean[key] = value
				}
			}
			updated["config"] = clean
		}
	}
	if newCall && agent == "codex" {
		if _, exists := input["developer-instructions"]; !exists {
			updated["developer-instructions"] = independenceInstructions
		}
	}
	return updated
}
