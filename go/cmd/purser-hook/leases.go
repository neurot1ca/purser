package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"purser-hook/internal/lease"
)

func leasesMain(args []string) int {
	list := len(args) == 1 && args[0] == "--list"
	release := len(args) == 2 && args[0] == "--release" && args[1] != ""
	if !list && !release {
		_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook leases --list | --release TASK_ID")
		return 2
	}

	key := "active"
	if release {
		key = "released"
	}
	entries := make([]map[string]any, 0)
	projects, err := filepath.Glob(filepath.Join(stateRoot(), "projects", "*", "scope_leases.json"))
	if err != nil {
		return leasesError(err)
	}
	sort.Strings(projects)
	now := float64(time.Now().UnixNano()) / 1e9
	for _, path := range projects {
		store := lease.New(filepath.Dir(path))
		var rows []map[string]any
		if list {
			rows, err = store.Active(now)
		} else {
			rows, err = store.ReleaseTask(args[1], now)
		}
		if err != nil {
			return leasesError(err)
		}
		for _, row := range rows {
			entry := map[string]any{"project_key": filepath.Base(filepath.Dir(path))}
			for name, value := range row {
				entry[name] = value
			}
			entries = append(entries, entry)
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{key: entries}); err != nil {
		return leasesError(err)
	}
	_, _ = os.Stdout.Write(output.Bytes())
	return 0
}

func leasesError(err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "Lease record could not be read or updated: %v\n", err)
	return 1
}
