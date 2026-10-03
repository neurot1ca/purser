package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"purser-hook/internal/contextio"
)

func main() {
	defer func() { _ = recover() }()
	started := time.Now()
	data, err := io.ReadAll(os.Stdin)
	if err != nil || !utf8.Valid(data) {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var event map[string]any
	if decoder.Decode(&event) != nil || event == nil {
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return
	}
	if event["hook_event_name"] != "PreToolUse" || event["tool_name"] != "Read" {
		return
	}
	_ = contextio.Read(event, started)
}
