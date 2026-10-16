package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"purser-hook/internal/enforce"
	"purser-hook/internal/waiter"
)

// decodeMCPHandoff rejects duplicate keys before any identity is collapsed by
// map decoding. This includes the proven _meta correlation field.
func decodeMCPHandoff(data []byte, value any) error {
	if len(data) > enforce.MCPHandoffLimit {
		return errors.New("MCP handoff input too large")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate/invalid MCP handoff field")
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		case json.Delim('['):
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing MCP handoff data")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(value)
}

// prepareMCPCall joins one MCP call to the exact hook-published identity. The
// owner is pinned by the MCP host, never supplied in arguments.
func prepareMCPCall(root string, owner waiter.Owner, data []byte) (*enforce.MCPHandoff, error) {
	var params map[string]any
	if err := decodeMCPHandoff(data, &params); err != nil {
		return nil, err
	}
	meta, _ := params["_meta"].(map[string]any)
	id, _ := meta["claudecode/toolUseId"].(string)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	var lastErr error
	for _, prefix := range []string{"mcp__plugin_purser_codex-main__", "mcp__codex-main__"} {
		h, err := enforce.PrepareMCP(root, owner, id, prefix+name, args)
		if h != nil {
			return h, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// mcp-handoff reads a trusted hook event. It publishes identity only and cannot
// run policy or launch a worker.
func mcpHandoffMain() int {
	owner, err := waiter.SessionOwner()
	var event map[string]any
	if err == nil {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(os.Stdin, enforce.MCPHandoffLimit+1))
		if err == nil {
			err = decodeMCPHandoff(data, &event)
		}
		if err == nil {
			err = enforce.PublishMCPHandoff(stateRoot(), owner, event)
		}
	}
	if err != nil {
		writeJSON(map[string]any{"error": err.Error()})
		return 1
	}
	writeJSON(map[string]any{"written": true})
	return 0
}
