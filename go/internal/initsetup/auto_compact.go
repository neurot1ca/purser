package initsetup

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AutoCompactRun updates or reports the global automatic compaction preference.
func AutoCompactRun(args []string, out, errOut io.Writer) int {
	usage := "usage: purser-hook auto-compact on|off|status [--json]"
	if len(args) == 0 || (args[0] != "on" && args[0] != "off" && args[0] != "status") {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("auto-compact", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	state := action
	if action == "on" || action == "off" {
		if err := writeAutoCompact(action == "on"); err != nil {
			fmt.Fprintf(errOut, "auto-compact preference could not be updated: %v\n", err)
			return 1
		}
	} else {
		raw, err := os.ReadFile(filepath.Join(stateRootPath(), "auto-compact.json"))
		if os.IsNotExist(err) {
			state = "not_set"
		} else if err != nil {
			fmt.Fprintf(errOut, "auto-compact preference could not be read: %v\n", err)
			return 1
		} else {
			var marker struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &marker); err != nil {
				fmt.Fprintf(errOut, "auto-compact preference is invalid: %v\n", err)
				return 1
			}
			if marker.Enabled {
				state = "on"
			} else {
				state = "off"
			}
		}
	}
	ack, err := json.Marshal(map[string]string{"auto_compact": state})
	if err != nil {
		fmt.Fprintf(errOut, "auto-compact acknowledgement could not be encoded: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(out, string(ack)); err != nil {
		fmt.Fprintf(errOut, "auto-compact acknowledgement could not be written: %v\n", err)
		return 1
	}
	return 0
}

func writeAutoCompact(enabled bool) error {
	marker := filepath.Join(stateRootPath(), "auto-compact.json")
	return atomicWrite(marker, []byte(fmt.Sprintf("{\"enabled\": %t}", enabled)), false)
}
