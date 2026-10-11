package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/jobs"
)

func TestSynchronousToolsReplayGolden(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	fixturePath := filepath.Join(filepath.Dir(source), "testdata", "synchronous_python.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name         string         `json:"name"`
			Tool         string         `json:"tool"`
			Arguments    map[string]any `json:"arguments"`
			Mode         string         `json:"mode"`
			ThreadID     string         `json:"threadId"`
			ThreadRecord map[string]any `json:"threadRecord"`
			Result       map[string]any `json:"result"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 6 {
		t.Fatalf("fixture cases=%d, want 6", len(fixture.Cases))
	}
	binary := buildPurserHook(t)
	isolateSynchronousEnvironment(t)
	tempRoot := t.TempDir()
	bin, err := synchronousFakeCodexPrefix()
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			state := filepath.Join(tempRoot, testCase.Name)
			args := replaceFixtureRoot(testCase.Arguments, tempRoot)
			if cwd, ok := args["cwd"].(string); ok {
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.ThreadRecord != nil {
				record := replaceFixtureRoot(testCase.ThreadRecord, tempRoot)
				if cwd, ok := record["cwd"].(string); ok {
					if err := os.MkdirAll(cwd, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := jobs.SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), record); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("PURSER_CODEX_BIN", string(bin))
			t.Setenv("PURSER_SYNC_ORACLE_MODE", testCase.Mode)
			t.Setenv("PURSER_SYNC_ORACLE_THREAD", testCase.ThreadID)
			if testCase.Name == "cancelled" {
				ctx, cancel := context.WithCancel(context.Background())
				result := make(chan map[string]any, 1)
				started := filepath.Join(tempRoot, "cancel-child-started")
				t.Setenv("PURSER_SYNC_ORACLE_STARTED", started)
				go func() {
					value, callErr := runSynchronousContext(ctx, args, testCase.Tool)
					result <- map[string]any{"value": value, "cancelled": errors.Is(callErr, errSynchronousCancelled)}
				}()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(started); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if _, err := os.Stat(started); err != nil {
					cancel()
					t.Fatal("fake child did not start")
				}
				cancel()
				got := <-result
				if got["cancelled"] != true || testCase.Result != nil {
					t.Fatalf("cancel behavior mismatch: got=%#v python response=%#v", got, testCase.Result)
				}
				return
			}
			payload, err := json.Marshal(map[string]any{"args": args})
			if err != nil {
				t.Fatal(err)
			}
			command := testutil.Hide(exec.Command(binary, "op", testCase.Tool))
			command.Stdin = strings.NewReader(string(payload))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("private op: %v: %s", err, output)
			}
			var envelope map[string]any
			if err := json.Unmarshal(output, &envelope); err != nil {
				t.Fatalf("invalid private response %q: %v", output, err)
			}
			got, _ := envelope["result"].(map[string]any)
			if !reflect.DeepEqual(legacyProjection(got), legacyProjection(testCase.Result)) {
				t.Fatalf("Python oracle mismatch\ngot:  %#v\nwant: %#v", got, testCase.Result)
			}
			if testCase.Name == "new_success" {
				if record := jobs.LoadRecord(filepath.Join(state, "codex_exec_mcp", "threads"), testCase.ThreadID); record == nil {
					t.Fatal("successful new call did not persist its thread record")
				}
			}
		})
	}
}

func TestDispatchOperationMatchesSynchronousFixtureText(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "testdata", "synchronous_python.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name      string         `json:"name"`
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
			Result    map[string]any `json:"result"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var selected *struct {
		Name      string         `json:"name"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Result    map[string]any `json:"result"`
	}
	for i := range fixture.Cases {
		if fixture.Cases[i].Name == "new_success" {
			selected = &fixture.Cases[i]
			break
		}
	}
	if selected == nil {
		t.Fatal("Python fixture has no new_success case")
	}
	isolateSynchronousEnvironment(t)
	root := t.TempDir()
	args := replaceFixtureRoot(selected.Arguments, root)
	cwd := args["cwd"].(string)
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	bin, err := synchronousFakeCodexPrefix()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("PURSER_CODEX_BIN", string(bin))
	t.Setenv("PURSER_SYNC_ORACLE_MODE", "success")
	t.Setenv("PURSER_SYNC_ORACLE_THREAD", "thread-new")
	got, err := dispatchOperation(context.Background(), selected.Tool, args, root)
	if err != nil {
		t.Fatal(err)
	}
	gotText := got["content"].([]any)[0].(map[string]any)["text"]
	wantText := selected.Result["content"].([]any)[0].(map[string]any)["text"]
	if gotText != wantText {
		t.Fatalf("result text bytes differ\ngot:  %q\nwant: %q", gotText, wantText)
	}
}

func TestDispatchOperationReadsUsageAfterDelayedCompletion(t *testing.T) {
	bin, err := synchronousFakeCodexPrefix()
	if err != nil {
		t.Fatal(err)
	}
	isolateSynchronousEnvironment(t)
	t.Setenv("PURSER_SYNC_ORACLE_DELAY", "1")
	root := t.TempDir()
	cwd := filepath.Join(root, "repo")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("PURSER_CODEX_BIN", string(bin))
	t.Setenv("PURSER_SYNC_ORACLE_MODE", "success")
	t.Setenv("PURSER_SYNC_ORACLE_THREAD", "thread-new")
	got, err := dispatchOperation(context.Background(), "codex", map[string]any{"cwd": cwd, "prompt": "delayed completion"}, root)
	if err != nil {
		t.Fatal(err)
	}
	gotText := got["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(gotText, `"usage": {"input_tokens": 3, "output_tokens": 2}`) {
		t.Fatalf("result text lost delayed turn usage: %q", gotText)
	}
}

func TestDispatchOperationContextCancellationStopsChild(t *testing.T) {
	isolateSynchronousEnvironment(t)
	root := t.TempDir()
	args := map[string]any{"prompt": "cancel", "cwd": root}
	started := filepath.Join(root, "child-started")
	bin, err := synchronousFakeCodexPrefix()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("PURSER_CODEX_BIN", string(bin))
	t.Setenv("PURSER_SYNC_ORACLE_MODE", "cancel")
	t.Setenv("PURSER_SYNC_ORACLE_THREAD", "thread-cancelled")
	t.Setenv("PURSER_SYNC_ORACLE_STARTED", started)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, callErr := dispatchOperation(ctx, "codex", args, root)
		result <- callErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("fake child did not start")
	}
	cancel()
	select {
	case callErr := <-result:
		if !errors.Is(callErr, errSynchronousCancelled) {
			t.Fatalf("dispatch error=%v, want synchronous cancellation", callErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not stop after context cancellation")
	}
}

func synchronousFakeCodexPrefix() ([]byte, error) {
	return json.Marshal([]string{os.Args[0], "-test.run=^TestSynchronousFakeCodexProcess$"})
}

func TestSynchronousFakeCodexProcess(t *testing.T) {
	if os.Getenv("PURSER_SYNC_FAKE_CODEX") != "1" {
		return
	}
	if marker := os.Getenv("PURSER_SYNC_ORACLE_STARTED"); marker != "" {
		if err := os.WriteFile(marker, []byte("started"), 0600); err != nil {
			os.Exit(2)
		}
	}
	mode := os.Getenv("PURSER_SYNC_ORACLE_MODE")
	thread := os.Getenv("PURSER_SYNC_ORACLE_THREAD")
	if mode == "cancel" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	write := func(value map[string]any) {
		data, _ := json.Marshal(value)
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
	write(map[string]any{"type": "thread.started", "thread_id": thread})
	if mode == "failure" {
		write(map[string]any{"type": "turn.failed", "message": "oracle worker failure"})
		os.Exit(1)
	}
	content := "oracle final text"
	if mode == "unicode" {
		content = "Türkçe İstanbul: \u2028 separator \u2029 separator \b backspace \f formfeed literal \\u2028"
	}
	outputPath := os.Args[len(os.Args)-1]
	if err := os.WriteFile(outputPath, []byte(content), 0600); err != nil {
		os.Exit(2)
	}
	write(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": content}})
	if os.Getenv("PURSER_SYNC_ORACLE_DELAY") == "1" {
		time.Sleep(50 * time.Millisecond)
	}
	write(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}})
	os.Exit(0)
}

func isolateSynchronousEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PURSER_AGENT", "codex")
	t.Setenv("PURSER_SYNC_FAKE_CODEX", "1")
	t.Setenv("PATH", t.TempDir())
}

func TestToolJSONUnicodeEscapeGolden(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	fixturePath := filepath.Join(filepath.Dir(source), "testdata", "synchronous_python.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string         `json:"name"`
			Result map[string]any `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		if testCase.Name != "unicode_content" {
			continue
		}
		structured, ok := testCase.Result["structuredContent"].(map[string]any)
		if !ok {
			t.Fatal("Python fixture has no structured content")
		}
		content, ok := structured["content"].(string)
		if !ok {
			t.Fatal("Python fixture has no content text")
		}
		result := legacySuccess("thread-unicode", content, map[string]any{"input_tokens": float64(3), "output_tokens": float64(2)}, true)
		response := result["content"].([]any)[0].(map[string]any)["text"].(string)
		want := testCase.Result["content"].([]any)[0].(map[string]any)["text"].(string)
		if response != want {
			t.Fatalf("Python JSON bytes differ\ngot:  %q\nwant: %q", response, want)
		}
		return
	}
	t.Fatal("Python fixture has no unicode_content case")
}

func legacyProjection(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, 4)
	for _, key := range []string{"content", "isError", "structuredContent", "typedEvidence"} {
		if item, ok := value[key]; ok {
			result[key] = item
		}
	}
	return result
}

func replaceFixtureRoot(value map[string]any, root string) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		switch v := item.(type) {
		case string:
			result[key] = strings.ReplaceAll(v, "__ROOT__", root)
		case map[string]any:
			result[key] = replaceFixtureRoot(v, root)
		default:
			result[key] = item
		}
	}
	return result
}
