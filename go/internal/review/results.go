package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	NoMaterialFinding    = "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
	ChangesRequired      = "CHANGES_REQUIRED"
	Uncertain            = "UNCERTAIN"
	Reviewer1Unavailable = "REVIEWER1_UNAVAILABLE"
	ResponseInvalid      = "REVIEWER_RESPONSE_INVALID"
)

var completedResults = map[string]bool{NoMaterialFinding: true, ChangesRequired: true, Uncertain: true}
var allResults = map[string]bool{NoMaterialFinding: true, ChangesRequired: true, Uncertain: true, Reviewer1Unavailable: true}
var envelopeFields = []string{"conversation_id", "status", "response", "duration_seconds", "num_turns", "usage"}
var usageTokenFields = map[string]bool{
	"input_tokens": true, "output_tokens": true, "total_tokens": true, "prompt_tokens": true,
	"completion_tokens": true, "cached_tokens": true, "cache_read_tokens": true,
	"cache_write_tokens": true, "thinking_tokens": true,
}
var jsonFence = regexp.MustCompile("(?s)^\\s*```json\\s*\\r?\\n(.*?)\\r?\\n```\\s*$")

type CompletedReview struct {
	Result, Validation                      string
	Findings                                []Finding
	ConversationID, Reviewer1ModelRequested string
	DurationSeconds                         *float64
	Usage                                   map[string]any
	Provider, ServedModelAttestation        string
	RetryCount                              int
	RetryRecovered                          bool
}

func (r CompletedReview) Map() map[string]any {
	return map[string]any{"review_status": "COMPLETED", "available": true, "provider": r.provider(), "result": r.Result, "validation": r.Validation, "findings": findingMaps(r.Findings), "conversation_id": r.ConversationID, "reviewer1_model_requested": r.Reviewer1ModelRequested, "served_model_attestation": r.attestation()}
}

type UnavailableReview struct {
	ErrorCode, Message, Reviewer1ModelRequested string
	DurationSeconds                             *float64
	Usage                                       map[string]any
	RawStdout, DecoderDetail                    *string
	ElapsedSeconds                              *float64
	StderrTail                                  *string
	Provider                                    string
	RetryCount                                  int
	RetryRecovered                              bool
}

func (r UnavailableReview) Map() map[string]any {
	return map[string]any{"review_status": "UNAVAILABLE", "available": false, "provider": r.provider(), "result": Reviewer1Unavailable, "validation": "UNKNOWN", "findings": []map[string]string{FindingMap(r.ErrorCode, r.Message)}, "error_code": r.ErrorCode, "reviewer1_model_requested": r.Reviewer1ModelRequested, "served_model_attestation": "UNAVAILABLE"}
}

type IncompleteReview struct {
	Code, Message                     string
	Omitted                           []string
	Reviewer1ModelRequested, Provider string
}

func (r IncompleteReview) Map() map[string]any {
	return map[string]any{"review_status": "NOT_RUN_INCOMPLETE_PACKET", "available": true, "provider": r.provider(), "result": Uncertain, "validation": "UNKNOWN", "findings": []map[string]string{FindingMap(r.Code, r.Message)}, "omitted": append([]string{}, r.Omitted...), "reviewer1_model_requested": r.Reviewer1ModelRequested, "served_model_attestation": "UNAVAILABLE"}
}

func (r CompletedReview) provider() string {
	if r.Provider != "" {
		return r.Provider
	}
	return "agy"
}
func (r CompletedReview) attestation() string {
	if r.ServedModelAttestation != "" {
		return r.ServedModelAttestation
	}
	return "UNAVAILABLE"
}
func (r UnavailableReview) provider() string {
	if r.Provider != "" {
		return r.Provider
	}
	return "agy"
}
func (r IncompleteReview) provider() string {
	if r.Provider != "" {
		return r.Provider
	}
	return "agy"
}

func FindingMap(code, message string) map[string]string {
	return map[string]string{"code": code, "message": message}
}

func RequestedModel(config map[string]any) string {
	if model, ok := config["reviewer1_model_requested"].(string); ok && model != "" {
		return model
	}
	return "UNAVAILABLE"
}

func Completed(result, validation string, findings []Finding, conversationID string, config map[string]any, durationSeconds *float64, usage map[string]any, provider, servedModelAttestation string, retryCount int) (CompletedReview, error) {
	if !completedResults[result] {
		return CompletedReview{}, errors.New("a completed review cannot carry a failure result")
	}
	return CompletedReview{Result: result, Validation: validation, Findings: append([]Finding{}, findings...), ConversationID: conversationID, Reviewer1ModelRequested: RequestedModel(config), DurationSeconds: durationSeconds, Usage: usage, Provider: provider, ServedModelAttestation: servedModelAttestation, RetryCount: retryCount, RetryRecovered: retryCount > 0}, nil
}

func Unavailable(code, message string, config map[string]any, provider string) UnavailableReview {
	return UnavailableReview{ErrorCode: code, Message: message, Reviewer1ModelRequested: RequestedModel(config), Provider: provider}
}

func StderrTail(value any) *string {
	var text string
	switch v := value.(type) {
	case []byte:
		text = strings.ToValidUTF8(string(v), "�")
	case string:
		text = v
	default:
		return nil
	}
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) > 2048 {
		text = string(runes[len(runes)-2048:])
	}
	return &text
}

func IncompletePacket(packet PacketOmitted, config map[string]any, provider string) IncompleteReview {
	return IncompleteReview{Code: packet.Code, Message: packet.Message, Omitted: append([]string{}, packet.Omitted...), Reviewer1ModelRequested: RequestedModel(config), Provider: provider}
}

func findingMaps(findings []Finding) []map[string]string {
	out := make([]map[string]string, 0, len(findings))
	for _, finding := range findings {
		out = append(out, FindingMap(finding.Code, finding.Message))
	}
	return out
}

type ParsedEnvelope struct {
	ConversationID         string
	Response               map[string]any
	DurationSeconds        *float64
	Usage                  map[string]any
	ServedModelAttestation string
	IncludeMetadata        bool
}

func (p ParsedEnvelope) Map() map[string]any {
	out := map[string]any{"conversation_id": p.ConversationID, "response": p.Response, "served_model_attestation": p.ServedModelAttestation}
	if p.IncludeMetadata {
		out["duration_seconds"] = p.DurationSeconds
		if p.Usage == nil {
			out["usage"] = map[string]any{}
		} else {
			out["usage"] = p.Usage
		}
	}
	return out
}

func ParseEnvelope(adapter string, stdout []byte, includeMetadata bool) (ParsedEnvelope, error) {
	switch adapter {
	case "agy":
		return ParseAgyEnvelope(stdout, includeMetadata)
	case "opencode":
		return ParseOpenCodeEnvelope(stdout, includeMetadata)
	case "codex":
		return ParseCodexEnvelope(stdout, includeMetadata)
	case "claude":
		var envelope map[string]any
		if err := decodeJSON(stdout, &envelope); err != nil {
			return ParsedEnvelope{}, err
		}
		if envelope["type"] != "result" || envelope["subtype"] != "success" || envelope["is_error"] == true {
			return ParsedEnvelope{}, errors.New("Claude envelope did not report success")
		}
		conversation, ok := envelope["session_id"].(string)
		payload, structured := envelope["structured_output"]
		if !ok || conversation == "" || !structured {
			return ParsedEnvelope{}, errors.New("Claude envelope has no session or structured output")
		}
		response, err := ValidateResponse(payload)
		if err != nil {
			return ParsedEnvelope{}, err
		}
		parsed := ParsedEnvelope{ConversationID: conversation, Response: response, ServedModelAttestation: "UNAVAILABLE", IncludeMetadata: includeMetadata}
		if includeMetadata {
			if duration, ok := number(envelope["duration_ms"]); ok && duration >= 0 && !math.IsInf(duration, 0) && !math.IsNaN(duration) {
				seconds := duration / 1000
				parsed.DurationSeconds = &seconds
			}
			if usage, ok := envelope["usage"].(map[string]any); ok {
				parsed.Usage = usageTokens(usage)
			}
		}
		return parsed, nil
	default:
		return ParsedEnvelope{}, fmt.Errorf("unknown reviewer adapter %q", adapter)
	}
}

func ParseAgyEnvelope(stdout []byte, includeMetadata bool) (ParsedEnvelope, error) {
	var envelope map[string]any
	if err := decodeJSON(stdout, &envelope); err != nil {
		return ParsedEnvelope{}, err
	}
	for _, key := range envelopeFields {
		if _, ok := envelope[key]; !ok {
			return ParsedEnvelope{}, errors.New("agy envelope has an unexpected shape")
		}
	}
	conversation, ok := envelope["conversation_id"].(string)
	if !ok || conversation == "" {
		return ParsedEnvelope{}, errors.New("agy envelope has no conversation id")
	}
	status, ok := envelope["status"].(string)
	if !ok || !strings.EqualFold(status, "success") {
		return ParsedEnvelope{}, errors.New("agy envelope did not report success")
	}
	duration, dok := number(envelope["duration_seconds"])
	turns, tok := integer(envelope["num_turns"])
	usage, uok := envelope["usage"].(map[string]any)
	if !dok || duration < 0 || math.IsInf(duration, 0) || math.IsNaN(duration) || !tok || turns < 1 || !uok {
		return ParsedEnvelope{}, errors.New("agy envelope metadata has an unexpected shape")
	}
	payload, err := structuredPayload(envelope)
	if err != nil {
		return ParsedEnvelope{}, err
	}
	response, err := ValidateResponse(payload)
	if err != nil {
		return ParsedEnvelope{}, err
	}
	parsed := ParsedEnvelope{ConversationID: conversation, Response: response, ServedModelAttestation: "UNAVAILABLE", IncludeMetadata: includeMetadata}
	if includeMetadata {
		d := duration
		parsed.DurationSeconds = &d
		parsed.Usage = usageTokens(usage)
	}
	return parsed, nil
}

// ParseAgyStructuredPayload validates the common agy envelope while leaving
// the structured payload to the caller's schema. Readiness has a different
// response schema from Reviewer-1 but shares this transport envelope.
func ParseAgyStructuredPayload(stdout []byte) (any, error) {
	var envelope map[string]any
	if err := decodeJSON(stdout, &envelope); err != nil {
		return nil, err
	}
	for _, key := range envelopeFields {
		if _, ok := envelope[key]; !ok {
			return nil, errors.New("agy envelope has an unexpected shape")
		}
	}
	conversation, conversationOK := envelope["conversation_id"].(string)
	status, statusOK := envelope["status"].(string)
	duration, durationOK := number(envelope["duration_seconds"])
	turns, turnsOK := integer(envelope["num_turns"])
	_, usageOK := envelope["usage"].(map[string]any)
	if !conversationOK || conversation == "" || !statusOK || !strings.EqualFold(status, "success") || !durationOK || duration < 0 || math.IsInf(duration, 0) || math.IsNaN(duration) || !turnsOK || turns < 1 || !usageOK || envelope["structured_output"] == nil {
		return nil, errors.New("agy envelope metadata has an unexpected shape")
	}
	return structuredPayload(envelope)
}

func ParseOpenCodeEnvelope(stdout []byte, includeMetadata bool) (ParsedEnvelope, error) {
	return parseEventStream(stdout, "opencode", includeMetadata)
}
func ParseCodexEnvelope(stdout []byte, includeMetadata bool) (ParsedEnvelope, error) {
	return parseEventStream(stdout, "codex", includeMetadata)
}

func ValidFinding(value any) bool {
	finding, ok := value.(map[string]any)
	if !ok || len(finding) != 2 {
		return false
	}
	code, cok := finding["code"].(string)
	message, mok := finding["message"].(string)
	return cok && utf8.RuneCountInString(code) >= 1 && utf8.RuneCountInString(code) <= 120 && mok && utf8.RuneCountInString(message) >= 1 && utf8.RuneCountInString(message) <= 2000
}

func ValidateResponse(value any) (map[string]any, error) {
	response, ok := value.(map[string]any)
	if !ok || len(response) != 3 {
		return nil, errors.New("review response fields do not match the schema")
	}
	result, rok := response["result"].(string)
	validation, vok := response["validation"].(string)
	findings, fok := response["findings"].([]any)
	if !rok || !allResults[result] {
		return nil, errors.New("review result is outside the allowed union")
	}
	if !vok || (validation != "PASS" && validation != "FAIL" && validation != "UNKNOWN") {
		return nil, errors.New("validation is outside the allowed enum")
	}
	if !fok || len(findings) > 20 {
		return nil, errors.New("findings violate the result schema")
	}
	clean := make([]any, 0, len(findings))
	for _, item := range findings {
		if !ValidFinding(item) {
			return nil, errors.New("findings violate the result schema")
		}
		f := item.(map[string]any)
		clean = append(clean, map[string]any{"code": f["code"], "message": f["message"]})
	}
	if result == NoMaterialFinding && len(findings) > 0 {
		return nil, errors.New("NO_MATERIAL finding verdict cannot include material findings")
	}
	return map[string]any{"result": result, "validation": validation, "findings": clean}, nil
}

func ParseBareOrFencedResponse(text string) (map[string]any, error) {
	payload := text
	if match := jsonFence.FindStringSubmatch(text); match != nil {
		payload = match[1]
	}
	var value any
	if err := decodeJSON([]byte(payload), &value); err != nil {
		return nil, err
	}
	return ValidateResponse(value)
}

func parseEventStream(stdout []byte, provider string, includeMetadata bool) (ParsedEnvelope, error) {
	lines := strings.Split(string(stdout), "\n")
	textParts := []string{}
	conversation, served := "UNAVAILABLE", "UNAVAILABLE"
	completed := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := decodeJSON([]byte(line), &event); err != nil {
			return ParsedEnvelope{}, err
		}
		if event == nil {
			return ParsedEnvelope{}, fmt.Errorf("%s event was not an object", provider)
		}
		if session, ok := event["sessionID"].(string); ok && session != "" {
			conversation = session
		} else if session, ok := event["thread_id"].(string); ok && session != "" {
			conversation = session
		}
		model, _ := event["model"].(string)
		modelID, _ := event["modelID"].(string)
		modelProvider, _ := event["providerID"].(string)
		if model != "" {
			served = model
		} else if modelID != "" {
			if modelProvider != "" {
				served = modelProvider + "/" + modelID
			} else {
				served = modelID
			}
		}
		typ, _ := event["type"].(string)
		if provider == "opencode" {
			part, _ := event["part"].(map[string]any)
			if typ == "text" && part != nil {
				if value, ok := part["text"].(string); ok {
					textParts = append(textParts, value)
				}
			}
			if typ == "step_finish" && part != nil && part["reason"] == "stop" {
				completed = true
			}
		}
		item, _ := event["item"].(map[string]any)
		if typ == "item.completed" && item != nil && item["type"] == "agent_message" {
			if text, ok := item["text"].(string); ok {
				textParts = append(textParts, text)
			}
		}
		if provider == "codex" && typ == "turn.completed" {
			completed = true
		}
		if typ == "error" || typ == "turn.failed" {
			return ParsedEnvelope{}, fmt.Errorf("%s event stream reported a failure", provider)
		}
		part, _ := event["part"].(map[string]any)
		state, _ := part["state"].(map[string]any)
		if provider == "opencode" && typ == "tool_use" && state != nil && state["status"] == "error" {
			return ParsedEnvelope{}, errors.New("OpenCode reported a failed tool")
		}
	}
	if len(textParts) == 0 {
		return ParsedEnvelope{}, fmt.Errorf("%s event stream had no final assistant text", provider)
	}
	if !completed {
		return ParsedEnvelope{}, fmt.Errorf("%s event stream had no successful completion marker", provider)
	}
	final := textParts[len(textParts)-1]
	if provider == "opencode" {
		final = strings.Join(textParts, "")
	}
	response, err := ParseBareOrFencedResponse(strings.TrimSpace(final))
	if err != nil {
		return ParsedEnvelope{}, err
	}
	return ParsedEnvelope{ConversationID: conversation, Response: response, ServedModelAttestation: served, IncludeMetadata: includeMetadata, Usage: map[string]any{}}, nil
}

func structuredPayload(envelope map[string]any) (any, error) {
	if structured, ok := envelope["structured_output"]; ok && structured != nil {
		if text, ok := structured.(string); ok {
			var value any
			if err := decodeJSON([]byte(text), &value); err != nil {
				return nil, err
			}
			return value, nil
		}
		return structured, nil
	}
	response, ok := envelope["response"].(string)
	if ok {
		var value any
		if err := decodeJSON([]byte(response), &value); err != nil {
			return nil, err
		}
		return value, nil
	}
	return envelope["response"], nil
}

func usageTokens(usage map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range usage {
		if !usageTokenFields[key] {
			continue
		}
		n, ok := number(value)
		if ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			out[key] = value
		}
	}
	return out
}

func decodeJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("extra data after JSON value")
		}
		return err
	}
	return nil
}
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
func integer(value any) (int64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}
