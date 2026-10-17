package contextio

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"purser-hook/internal/obsidian"
)

const purserReleaseURL = "https://api.github.com/repos/neurot1ca/purser-claude-orchestrator/releases/latest"

var purserVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type purserUpdateState struct {
	CheckedOn       string `json:"checked_on"`
	Latest          string `json:"latest"`
	AnnouncedLatest string `json:"announced_latest,omitempty"`
}

// CheckPurserUpdate checks GitHub at most once per UTC day. Failures are silent
// so a later session can retry.
func CheckPurserUpdate() { CheckPurserUpdateAt(purserReleaseURL) }

// CheckPurserUpdateAt allows the release endpoint to be injected in tests.
func CheckPurserUpdateAt(endpoint string) {
	if os.Getenv("PURSER_NO_UPDATE_CHECK") == "1" || !validPurserVersion(installedPurserVersion()) {
		return
	}
	path := filepath.Join(stateRoot(), "update-check.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	today := time.Now().UTC().Format("2006-01-02")
	state := readPurserUpdateState(path)
	if state.CheckedOn == today {
		return
	}
	state.CheckedOn = today
	if writeJSONAtomically(path, state) != nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "purser")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if json.Unmarshal(body, &release) != nil || !validPurserVersion(release.Tag) {
		return
	}
	state.Latest = strings.TrimPrefix(release.Tag, "v")
	_ = writeJSONAtomically(path, state)
}

// TakePurserUpdateNotice returns and records the one-time release notice.
func TakePurserUpdateNotice() string {
	installed := installedPurserVersion()
	if !validPurserVersion(installed) {
		return ""
	}
	path := filepath.Join(stateRoot(), "update-check.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return ""
	}
	defer unlock()
	state := readPurserUpdateState(path)
	if !validPurserVersion(state.Latest) || state.AnnouncedLatest == state.Latest || comparePurserVersions(state.Latest, installed) <= 0 {
		return ""
	}
	state.AnnouncedLatest = state.Latest
	if writeJSONAtomically(path, state) != nil {
		return ""
	}
	return "⚓ Purser " + state.Latest + " is available (you have " + installed + "). Update with /plugin marketplace update purser-claude-orchestrator, then restart Claude Code."
}

func installedPurserVersion() string {
	data, err := os.ReadFile(filepath.Join(obsidian.PluginRoot(), ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	var plugin struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &plugin) != nil {
		return ""
	}
	return strings.TrimPrefix(plugin.Version, "v")
}

func readPurserUpdateState(path string) purserUpdateState {
	data, err := os.ReadFile(path)
	if err != nil {
		return purserUpdateState{}
	}
	var state purserUpdateState
	if json.Unmarshal(data, &state) != nil {
		return purserUpdateState{}
	}
	return state
}

func validPurserVersion(version string) bool { return purserVersionPattern.MatchString(version) }

func comparePurserVersions(a, b string) int {
	left, right := purserVersionPattern.FindStringSubmatch(strings.TrimPrefix(a, "v")), purserVersionPattern.FindStringSubmatch(strings.TrimPrefix(b, "v"))
	if left == nil || right == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		if len(left[i]) > len(right[i]) {
			return 1
		}
		if len(left[i]) < len(right[i]) {
			return -1
		}
		if cmp := strings.Compare(left[i], right[i]); cmp != 0 {
			return cmp
		}
	}
	return 0
}
