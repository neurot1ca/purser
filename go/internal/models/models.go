package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/models/cursor"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/policy"
	"purser-hook/internal/proc"
)

const maxCatalogBytes = 16 << 20
const maxListingBytes = 1 << 20

// ResolveLatest reads local caches only; configured IDs are family floors.
func ResolveLatest(agent, model string) string {
	var ids []string
	switch agent {
	case "codex":
		var err error
		ids, err = cachedCodexModels()
		if err != nil {
			return model
		}
	case "agy", "opencode":
		data, err := os.ReadFile(filepath.Join(stateRoot(), "model-resolution.json"))
		var cache resolutionCache
		if err != nil || json.Unmarshal(data, &cache) != nil {
			return model
		}
		if agent == "opencode" {
			if cache.OpenCodeSnapshot == "" && (cache.OpenCodeAttempted != "" || cache.CheckedOn != time.Now().UTC().Format("2006-01-02")) {
				return model
			}
		} else if cache.CheckedOn != time.Now().UTC().Format("2006-01-02") {
			return model
		}
		ids = cache.Agents[agent]
	case "cursor":
		ids = readResolutionCache().Agents[agent]
	default:
		return model
	}
	if agent == "cursor" {
		if strings.HasPrefix(model, "latest:") {
			return cursor.ResolveLatest(ids, strings.TrimPrefix(model, "latest:"))
		}
		return model
	}
	if newer := newerVersion(model, ids); newer != "" {
		return newer
	}
	return model
}

// SameFamily uses the same version boundary as discovery, in either direction.
func SameFamily(a, b string) bool {
	return a == b || newerVersion(a, []string{b}) != "" || newerVersion(b, []string{a}) != ""
}

// CodexSupportsEffort checks an effort against the installed Codex model listing.
func CodexSupportsEffort(model, effort string) bool {
	binary, args := listingCommand("codex")
	if binary == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := proc.Hide(exec.CommandContext(ctx, binary, args...))
	output, err := runListingCommand(ctx, command)
	if err != nil {
		return false
	}
	var listing struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
			Levels     []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(output, &listing) != nil || listing.Models == nil {
		return false
	}
	for _, item := range listing.Models {
		if item.Slug != model || item.Visibility != "list" {
			continue
		}
		for _, level := range item.Levels {
			if level.Effort == effort {
				return true
			}
		}
		return false
	}
	return false
}

// AtLeast accepts the floor or a newer ID in its family.
func AtLeast(floor, model string) bool {
	return model == floor || newerVersion(floor, []string{model}) != ""
}

// NewestVersion selects the newest ID using the model version comparison, with lexical order for unrelated IDs.
func NewestVersion(ids []string) string {
	best := ""
	for _, id := range ids {
		if best == "" || newerVersion(best, []string{id}) != "" || newerVersion(id, []string{best}) == "" && id > best {
			best = id
		}
	}
	return best
}

type resolutionCache struct {
	CheckedOn         string              `json:"checked_on"`
	OpenCodeAttempted string              `json:"opencode_attempted_on,omitempty"`
	OpenCodeSnapshot  string              `json:"opencode_snapshot_on,omitempty"`
	OpenCodeError     string              `json:"opencode_error,omitempty"`
	Agents            map[string][]string `json:"agents"`
}

const (
	defaultListingTimeout = 2 * time.Second
	catalogListingTimeout = 30 * time.Second
)

var versionPattern = regexp.MustCompile(`^(.+?)(\d+(?:\.\d+)*)([^0-9]*)$`)
var listingID = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
var opencodeID = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

type noticeState struct {
	CheckedOn *string       `json:"checked_on"`
	Report    *noticeReport `json:"report"`
}

type noticeReport struct {
	Current   string `json:"current"`
	New       string `json:"new"`
	Delivered bool   `json:"delivered"`
}

type catalog struct {
	Providers map[string]map[string]json.RawMessage `json:"providers"`
}

type loadedConfig struct {
	Values     map[string]any
	RouteOrder []string
}

// DiscoverModelVersion checks configured models at most once per UTC day and records a newer version.
func DiscoverModelVersion(repository string) (current, newer string, ok bool) {
	return discoverModelVersion(repository, defaultListingTimeout)
}

// DiscoverModelVersionForCatalogRefresh gives detached catalog work time for cold CLI startup.
func DiscoverModelVersionForCatalogRefresh(repository string) (current, newer string, ok bool) {
	return discoverModelVersion(repository, catalogListingTimeout)
}

func discoverModelVersion(repository string, listingTimeout time.Duration) (current, newer string, ok bool) {
	if strings.TrimSpace(repository) == "" {
		return "", "", false
	}
	statePath := filepath.Join(stateRoot(), "model-version-notice.json")
	unlock, err := contextio.LockFile(statePath + ".lock")
	if err != nil {
		return "", "", false
	}
	defer unlock()
	state := readNoticeState(statePath)
	loaded, err := loadConfig(repository)
	if err != nil {
		return "", "", false
	}
	config := loaded.Values
	cached, err := loadCatalog()
	if err != nil {
		return "", "", false
	}
	checks := make([]modelCheck, 0)
	agent, _ := config["coder_agent"].(string)
	if agent == "" {
		agent = "codex"
	}
	if routes, valid := config["routes"].(map[string]any); valid {
		seen := map[string]bool{}
		ordered := append([]string(nil), loaded.RouteOrder...)
		visited := map[string]bool{}
		for _, name := range ordered {
			visited[name] = true
		}
		for _, name := range sortedRouteNames(routes) {
			if !visited[name] {
				ordered = append(ordered, name)
			}
		}
		for _, name := range ordered {
			route, exists := routes[name]
			if !exists {
				continue
			}
			model := routeModel(route)
			if model != "" && !seen[model] {
				checks = append(checks, modelCheck{agent: agent, model: model})
				seen[model] = true
			}
		}
	}
	if reviewer, valid := config["reviewer1_model_requested"].(string); valid {
		reviewerAgent, _ := config["reviewer1_agent"].(string)
		if reviewerAgent == "" {
			reviewerAgent = "agy"
		}
		checks = append(checks, modelCheck{agent: reviewerAgent, model: reviewer})
	}
	if fallback, valid := config["fallback"].(map[string]any); valid {
		fallbackAgent, _ := fallback["agent"].(string)
		fallbackModel, _ := fallback["model"].(string)
		if fallbackAgent != "" && fallbackModel != "" {
			checks = append(checks, modelCheck{agent: fallbackAgent, model: fallbackModel})
		}
	}
	if config["coder_agent"] == "cursor" || config["reviewer1_agent"] == "cursor" {
		checks = append(checks, modelCheck{agent: "cursor"})
	}
	if fallback, valid := config["fallback"].(map[string]any); valid && fallback["agent"] == "cursor" {
		checks = append(checks, modelCheck{agent: "cursor"})
	}
	openCodeNeeded := false
	for _, check := range checks {
		if check.agent == "opencode" {
			openCodeNeeded = true
			break
		}
	}
	if config["mechanical_opencode_enabled"] != false {
		openCodeNeeded = true
	}
	today := time.Now().UTC().Format("2006-01-02")
	if openCodeNeeded {
		refreshOpenCodeListing(cached.Providers, today, listingTimeout)
	}
	if state.CheckedOn != nil && *state.CheckedOn == today {
		return "", "", false
	}
	listed := map[string]listingResult{}
	resolution := readResolutionCache()
	if resolution.Agents == nil {
		resolution.Agents = map[string][]string{}
	}
	for _, check := range checks {
		if check.model == "" {
			if check.agent == "cursor" {
				if _, exists := listed[check.agent]; !exists {
					result := listModels(check.agent, cached.Providers, defaultListingTimeout)
					listed[check.agent] = result
					if result.available {
						resolution.Agents[check.agent] = result.ids
					}
				}
				continue
			}
		}
		if _, exists := listed[check.agent]; !exists {
			result := listingResult{}
			if check.agent == "opencode" {
				result.ids = resolution.Agents["opencode"]
				result.available = resolution.OpenCodeSnapshot != "" && len(result.ids) > 0
			} else {
				result = listModels(check.agent, cached.Providers, defaultListingTimeout)
			}
			listed[check.agent] = result
			if result.available && check.agent == "agy" {
				resolution.Agents[check.agent] = result.ids
			}
		}
	}
	cachePath := filepath.Join(stateRoot(), "model-resolution.json")
	if unlock, lockErr := contextio.LockFile(cachePath + ".lock"); lockErr == nil {
		latest := readResolutionCache()
		if latest.Agents == nil {
			latest.Agents = map[string][]string{}
		}
		latest.CheckedOn = today
		if result := listed["agy"]; result.available {
			latest.Agents["agy"] = result.ids
		}
		if result := listed["cursor"]; result.available {
			latest.Agents["cursor"] = result.ids
		}
		_ = atomicJSONRetry(cachePath, latest)
		unlock()
	}
	for _, check := range checks {
		if check.model == "" {
			continue
		}
		result := listed[check.agent]
		if !result.available {
			continue
		}
		candidate := ResolveLatest(check.agent, check.model)
		if check.agent == "cursor" {
			family := cursor.Family(check.model)
			candidate = cursor.ResolveLatest(result.ids, family)
			if candidate == "" && !strings.HasPrefix(check.model, "latest:") {
				candidate = check.model
			}
		}
		if candidate == "" {
			continue
		}
		if candidate == check.model || (state.Report != nil && state.Report.Current == check.model && state.Report.New == candidate && state.Report.Delivered) {
			continue
		}
		if state.Report == nil || state.Report.Current != check.model || state.Report.New != candidate {
			state.Report = &noticeReport{Current: check.model, New: candidate}
		}
		if writeNoticeState(statePath, state) != nil {
			return "", "", false
		}
		if recordCheck(statePath) != nil {
			return "", "", false
		}
		return check.model, candidate, true
	}
	if family, candidate := newestCatalogModel(cached.Providers); candidate != "" {
		if state.Report == nil || state.Report.Current != family || state.Report.New != candidate || !state.Report.Delivered {
			if state.Report == nil || state.Report.Current != family || state.Report.New != candidate {
				state.Report = &noticeReport{Current: family, New: candidate}
			}
			if writeNoticeState(statePath, state) != nil {
				return "", "", false
			}
			if recordCheck(statePath) != nil {
				return "", "", false
			}
			return family, candidate, true
		}
	}
	_ = recordCheck(statePath)
	return "", "", false
}

func readResolutionCache() resolutionCache {
	var cache resolutionCache
	data, err := os.ReadFile(filepath.Join(stateRoot(), "model-resolution.json"))
	if err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	return cache
}

func refreshOpenCodeListing(providers map[string]map[string]json.RawMessage, today string, timeout time.Duration) {
	path := filepath.Join(stateRoot(), "model-resolution.json")
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	cache := readResolutionCache()
	if cache.Agents == nil {
		cache.Agents = map[string][]string{}
	}
	if cache.OpenCodeAttempted == today {
		return
	}
	cache.OpenCodeAttempted = today
	result := listModels("opencode", providers, timeout)
	if result.available && len(result.ids) > 0 {
		cache.Agents["opencode"] = result.ids
		cache.OpenCodeSnapshot = today
		cache.OpenCodeError = ""
	} else {
		cache.OpenCodeError = result.failure
	}
	_ = atomicJSONRetry(path, cache)
}

func atomicJSONRetry(path string, value any) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = atomicJSON(path, value); err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
		}
	}
	return err
}

func newestCatalogModel(providers map[string]map[string]json.RawMessage) (family, model string) {
	newestRelease := ""
	for provider, models := range providers {
		for id, raw := range models {
			var fact struct {
				Name        string `json:"name"`
				ReleaseDate string `json:"release_date"`
			}
			if json.Unmarshal(raw, &fact) != nil {
				continue
			}
			name, release := strings.ToLower(fact.Name), fact.ReleaseDate
			candidateFamily := ""
			switch provider {
			case "anthropic":
				if strings.HasPrefix(id, "claude-") {
					for _, tier := range []string{"haiku", "sonnet", "opus"} {
						if strings.Contains(id, tier) || strings.Contains(name, tier) {
							candidateFamily = "Claude " + strings.ToUpper(tier[:1]) + tier[1:]
							break
						}
					}
				}
			case "openai":
				if strings.HasPrefix(id, "gpt-") && (strings.HasSuffix(id, "-luna") || strings.HasSuffix(id, "-sol")) {
					candidateFamily = "OpenAI GPT"
				}
			case "google":
				if strings.HasPrefix(id, "gemini-") {
					candidateFamily = "Gemini"
				}
			}
			if candidateFamily == "" {
				continue
			}
			if model == "" || release > newestRelease || release == newestRelease && id > model {
				family, model, newestRelease = candidateFamily, id, release
			}
		}
	}
	return family, model
}

// DiscoverCachedCodexVersion checks the visible models in CODEX_HOME without
// invoking the Codex CLI or loading the model catalog.
func DiscoverCachedCodexVersion() (current, newer string, ok bool) {
	statePath := filepath.Join(stateRoot(), "model-version-notice.json")
	unlock, err := contextio.LockFile(statePath + ".lock")
	if err != nil {
		return "", "", false
	}
	defer unlock()

	state := readNoticeState(statePath)
	if state.Report != nil && !state.Report.Delivered {
		return state.Report.Current, state.Report.New, true
	}
	candidates, err := cachedCodexModels()
	if err != nil || len(candidates) == 0 {
		return "", "", false
	}
	loaded, err := loadConfig("")
	if err != nil {
		return "", "", false
	}
	routes, valid := loaded.Values["routes"].(map[string]any)
	if !valid {
		return "", "", false
	}
	seen := map[string]bool{}
	ordered := append([]string(nil), loaded.RouteOrder...)
	visited := map[string]bool{}
	for _, name := range ordered {
		visited[name] = true
	}
	for _, name := range sortedRouteNames(routes) {
		if !visited[name] {
			ordered = append(ordered, name)
		}
	}
	for _, name := range ordered {
		model := routeModel(routes[name])
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		candidate := newerVersion(model, candidates)
		if candidate == "" || (state.Report != nil && state.Report.Current == model && state.Report.New == candidate && state.Report.Delivered) {
			continue
		}
		if state.Report == nil || state.Report.Current != model || state.Report.New != candidate {
			state.Report = &noticeReport{Current: model, New: candidate}
		}
		if writeNoticeState(statePath, state) != nil {
			return "", "", false
		}
		return model, candidate, true
	}
	return "", "", false
}

// DiscoverCodexCLIVersion compares the installed CLI with models_cache.json's
// client_version and records each offered client version once.
func DiscoverCodexCLIVersion() (installed, available string, ok bool) {
	available, err := cachedCodexClientVersion()
	if err != nil || !cliVersionPattern.MatchString(available) {
		return "", "", false
	}
	statePath := filepath.Join(stateRoot(), "codex-cli-version.json")
	unlock, err := contextio.LockFile(statePath + ".lock")
	if err != nil {
		return "", "", false
	}
	defer unlock()

	var state struct {
		CheckedOn        string   `json:"checked_on"`
		InstalledVersion *string  `json:"installed_version"`
		Asked            []string `json:"asked"`
	}
	if data, readErr := os.ReadFile(statePath); readErr == nil {
		_ = json.Unmarshal(data, &state)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if state.CheckedOn != today {
		state.CheckedOn = today
		state.InstalledVersion = installedCodexVersion()
		if state.Asked == nil {
			state.Asked = []string{}
		}
		if atomicJSON(statePath, state) != nil {
			return "", "", false
		}
	}
	if state.InstalledVersion == nil || !cliVersionPattern.MatchString(*state.InstalledVersion) ||
		compareVersionNumbers(available, *state.InstalledVersion) <= 0 || has(state.Asked, available) {
		return "", "", false
	}
	state.Asked = appendUnique(state.Asked, available)
	if atomicJSON(statePath, state) != nil {
		return "", "", false
	}
	return *state.InstalledVersion, available, true
}

var cliVersionPattern = regexp.MustCompile(`^\d+(?:\.\d+){1,2}$`)
var cliVersionOutputPattern = regexp.MustCompile(`(?:^|[^\d])(\d+\.\d+(?:\.\d+)?)`)

func cachedCodexClientVersion() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if strings.TrimSpace(home) == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(home, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(expandHome(home), "models_cache.json"))
	if err != nil {
		return "", err
	}
	var cache struct {
		ClientVersion string          `json:"client_version"`
		Models        json.RawMessage `json:"models"`
	}
	if json.Unmarshal(data, &cache) != nil {
		return "", errors.New("invalid Codex models cache")
	}
	var models []json.RawMessage
	if json.Unmarshal(cache.Models, &models) != nil || models == nil {
		return "", errors.New("invalid Codex models cache")
	}
	return cache.ClientVersion, nil
}

func installedCodexVersion() *string {
	executable, err := exec.LookPath("codex")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := proc.Hide(exec.CommandContext(ctx, executable, "--version"))
	command.Stdout, command.Stderr = &stdout, &stderr
	if command.Run() != nil {
		return nil
	}
	result := stdout.Bytes()
	if len(result) == 0 {
		result = stderr.Bytes()
	}
	match := cliVersionOutputPattern.FindSubmatch(result)
	if len(match) < 2 {
		return nil
	}
	version := string(match[1])
	return &version
}

func cachedCodexModels() ([]string, error) {
	home := os.Getenv("CODEX_HOME")
	if strings.TrimSpace(home) == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(home, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(expandHome(home), "models_cache.json"))
	if err != nil {
		return nil, err
	}
	var cache struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cache); err != nil || cache.Models == nil {
		return nil, errors.New("invalid Codex models cache")
	}
	ids := []string{}
	for _, model := range cache.Models {
		if model.Visibility == "list" && model.Slug != "" {
			ids = appendUnique(ids, model.Slug)
		}
	}
	return ids, nil
}

type modelCheck struct{ agent, model string }
type listingResult struct {
	available bool
	ids       []string
	failure   string
}

// Variables keep the process and filesystem boundaries replaceable in package tests.
var loadConfig = func(repository string) (loadedConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return loadedConfig{}, err
	}
	configPath := obsidian.ConfigPath()
	config, err := policy.LoadCoderConfigForRepository(configPath, filepath.Join(home, ".orchestrator", "config.toml"), repository)
	if err != nil {
		return loadedConfig{}, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return loadedConfig{}, err
	}
	routeOrder, err := routeKeyOrder(raw)
	if err != nil {
		routes, _ := config["routes"].(map[string]any)
		routeOrder = sortedRouteNames(routes)
	}
	return loadedConfig{Values: config, RouteOrder: routeOrder}, nil
}

func sortedRouteNames(routes map[string]any) []string {
	order := make([]string, 0, len(routes))
	for name := range routes {
		order = append(order, name)
	}
	sort.Strings(order)
	return order
}

func routeKeyOrder(raw []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := start.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("config is not a JSON object")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("config key is not a string")
		}
		if name != "routes" {
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return nil, err
			}
			continue
		}
		routesStart, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if delimiter, ok := routesStart.(json.Delim); !ok || delimiter != '{' {
			return nil, errors.New("routes is not a JSON object")
		}
		order := []string{}
		for decoder.More() {
			routeKey, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			routeName, ok := routeKey.(string)
			if !ok {
				return nil, errors.New("route key is not a string")
			}
			var route json.RawMessage
			if err := decoder.Decode(&route); err != nil {
				return nil, err
			}
			order = append(order, routeName)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return order, nil
	}
	return nil, errors.New("config has no routes")
}

var loadCatalog = func() (catalog, error) {
	root := filepath.Join(stateRoot(), "model-catalog", "catalog.json")
	providers, err := readCachedProviders(root)
	if err == nil {
		return catalog{Providers: providers}, nil
	}
	bootstrap := filepath.Join(obsidian.PluginRoot(), ".claude", "hooks", "orchestrator", "model_catalog_bootstrap.json")
	data, readErr := os.ReadFile(bootstrap)
	if readErr != nil {
		return catalog{}, readErr
	}
	providers, err = validateProviders(data)
	if err != nil {
		return catalog{}, err
	}
	return catalog{Providers: providers}, nil
}

var lookPath = exec.LookPath
var runListingCommand = func(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	proc.ConfigureProcessTreeKill(command)
	output := &limitedOutput{limit: maxListingBytes}
	command.Stdout = output
	err := command.Run()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return output.data, nil
}

type limitedOutput struct {
	data  []byte
	limit int
}

func (output *limitedOutput) Write(data []byte) (int, error) {
	remaining := output.limit - len(output.data)
	if remaining <= 0 || len(data) > remaining {
		if remaining > 0 {
			output.data = append(output.data, data[:remaining]...)
		}
		return 0, errors.New("model listing exceeds output limit")
	}
	output.data = append(output.data, data...)
	return len(data), nil
}

var listModels = func(agent string, providers map[string]map[string]json.RawMessage, timeout time.Duration) listingResult {
	binary, args := listingCommand(agent)
	if binary == "" {
		return listingResult{failure: agent + " executable not found"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := proc.Hide(exec.CommandContext(ctx, binary, args...))
	output, err := runListingCommand(ctx, command)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return listingResult{failure: agent + " model listing timed out"}
		}
		return listingResult{failure: agent + " model listing failed: " + err.Error()}
	}
	ids, err := parseListing(agent, string(output))
	if err != nil {
		return listingResult{failure: agent + " model listing parse failed: " + err.Error()}
	}
	return listingResult{available: true, ids: ids}
}

func listingCommand(agent string) (string, []string) {
	switch agent {
	case "codex":
		binary, _ := lookPath("codex")
		return binary, []string{"debug", "models"}
	case "agy":
		binary, _ := lookPath("agy")
		return binary, []string{"models"}
	case "cursor":
		name := "cursor-agent"
		if runtime.GOOS == "windows" {
			name += ".cmd"
		}
		binary, _ := lookPath(name)
		return binary, []string{"models"}
	case "opencode":
		binary, _ := opencodeBinary()
		return binary, []string{"models"}
	default:
		return "", nil
	}
}

func opencodeBinary() (string, error) {
	launcher, err := lookPath("opencode")
	if err != nil {
		return "", err
	}
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	root := filepath.Dir(launcher)
	packages := []string{
		filepath.Join(root, "node_modules", "opencode-ai", "node_modules"),
		filepath.Join(filepath.Dir(root), "node_modules", "opencode-ai", "node_modules"),
		filepath.Join(filepath.Dir(root), "node_modules"),
		filepath.Join(filepath.Dir(root), "opencode-ai", "node_modules"),
	}
	variants := []string{}
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			variants = []string{"opencode-windows-x64", "opencode-windows-x64-baseline"}
		case "arm64":
			variants = []string{"opencode-windows-arm64"}
		}
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			variants = []string{"opencode-linux-x64"}
		case "arm64":
			variants = []string{"opencode-linux-arm64"}
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			variants = []string{"opencode-darwin-x64"}
		case "arm64":
			variants = []string{"opencode-darwin-arm64"}
		}
	}
	for _, base := range packages {
		for _, variant := range variants {
			candidate := filepath.Join(base, variant, "bin", name)
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(launcher), ".exe") {
		candidate := filepath.Join(root, "node_modules", "opencode-ai", "bin", name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		return "", nil
	}
	return launcher, nil
}

func parseListing(agent, output string) ([]string, error) {
	ids := []string{}
	switch agent {
	case "codex":
		var value struct {
			Models []struct {
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
				Levels     []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		if json.Unmarshal([]byte(output), &value) != nil || value.Models == nil {
			return nil, errors.New("invalid codex listing")
		}
		for _, item := range value.Models {
			if item.Visibility != "list" {
				continue
			}
			if item.Slug == "" || !listingID.MatchString(item.Slug) || len(item.Levels) == 0 {
				return nil, errors.New("invalid codex model")
			}
			for _, level := range item.Levels {
				if level.Effort == "" {
					return nil, errors.New("invalid codex effort")
				}
			}
			ids = append(ids, item.Slug)
		}
	case "opencode":
		for _, line := range strings.Split(output, "\n") {
			id := strings.TrimSpace(line)
			if opencodeID.MatchString(id) {
				ids = appendUnique(ids, id)
			}
		}
	case "agy":
		for _, line := range strings.Split(output, "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" || !listingID.MatchString(parts[0]) {
				continue
			}
			ids = appendUnique(ids, parts[0])
			id := parts[0]
			index := strings.LastIndexByte(id, '-')
			if index >= 0 {
				suffix := id[index+1:]
				if suffix == "low" || suffix == "medium" || suffix == "high" || suffix == "max" {
					id = id[:index]
				}
			}
			ids = appendUnique(ids, id)
		}
	case "cursor":
		if !strings.Contains(output, "Available models") {
			return nil, errors.New("invalid Cursor listing header")
		}
		for _, line := range strings.Split(output, "\n") {
			parts := strings.SplitN(line, " - ", 2)
			if len(parts) != 2 || !listingID.MatchString(parts[0]) {
				continue
			}
			ids = appendUnique(ids, parts[0])
		}
	default:
		return nil, errors.New("unsupported coder agent")
	}
	if len(ids) == 0 {
		return nil, errors.New("empty model listing")
	}
	return ids, nil
}

func readCachedProviders(path string) (map[string]map[string]json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCatalogBytes+1))
	if err != nil || len(data) > maxCatalogBytes {
		return nil, errors.New("invalid model catalog cache")
	}
	var envelope struct {
		FetchedAt string          `json:"fetched_at"`
		Providers json.RawMessage `json:"providers"`
		Dropped   json.RawMessage `json:"dropped_count"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.FetchedAt == "" {
		return nil, errors.New("invalid model catalog cache")
	}
	if _, err = time.Parse(time.RFC3339Nano, envelope.FetchedAt); err != nil {
		return nil, err
	}
	if len(envelope.Dropped) > 0 {
		var dropped int64
		if string(envelope.Dropped) == "null" || json.Unmarshal(envelope.Dropped, &dropped) != nil || dropped < 0 {
			return nil, errors.New("invalid model catalog dropped count")
		}
	}
	return validateProviders(envelope.Providers)
}

func validateProviders(data []byte) (map[string]map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || len(raw) == 0 {
		return nil, errors.New("invalid model catalog providers")
	}
	providers := map[string]map[string]json.RawMessage{}
	validCount := 0
	for providerID, providerRaw := range raw {
		if providerID == "" {
			continue
		}
		var provider struct {
			Models map[string]json.RawMessage `json:"models"`
		}
		models := map[string]json.RawMessage{}
		if json.Unmarshal(providerRaw, &provider) == nil && provider.Models != nil {
			models = provider.Models
		} else if json.Unmarshal(providerRaw, &models) != nil {
			continue
		}
		checked := map[string]json.RawMessage{}
		for modelID, factRaw := range models {
			if modelID == "" {
				continue
			}
			var fact struct {
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Reasoning *bool           `json:"reasoning"`
				Family    *string         `json:"family"`
				Limits    json.RawMessage `json:"limits"`
				Limit     json.RawMessage `json:"limit"`
			}
			if json.Unmarshal(factRaw, &fact) != nil || fact.Name == "" || fact.Reasoning == nil || fact.Family != nil && *fact.Family == "" {
				continue
			}
			limits := fact.Limit
			if len(limits) == 0 {
				limits = fact.Limits
			}
			if len(limits) > 0 {
				if string(limits) == "null" {
					continue
				}
				var values map[string]json.RawMessage
				if json.Unmarshal(limits, &values) != nil {
					continue
				}
				valid := true
				for _, key := range []string{"context", "output"} {
					if rawLimit, exists := values[key]; exists {
						var count json.Number
						if json.Unmarshal(rawLimit, &count) != nil {
							valid = false
							continue
						}
						if _, ok := positiveInteger(count); !ok {
							valid = false
						}
					}
				}
				if !valid {
					continue
				}
			}
			checked[modelID] = factRaw
		}
		providers[providerID] = checked
		validCount += len(checked)
	}
	if validCount == 0 {
		return nil, errors.New("empty model catalog")
	}
	return providers, nil
}

func stateRoot() string {
	if value := os.Getenv("PURSER_STATE_DIR"); value != "" {
		return resolve(filepath.Clean(expandHome(value)))
	}
	if value := os.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		return resolve(filepath.Join(expandHome(value), "purser"))
	}
	home, _ := os.UserHomeDir()
	return resolve(filepath.Join(home, ".claude", "purser"))
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func resolve(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			relative, _ := filepath.Rel(current, absolute)
			return filepath.Join(real, relative)
		}
		if filepath.Dir(current) == current {
			return filepath.Clean(absolute)
		}
	}
}

func readNoticeState(path string) noticeState {
	fallback := noticeState{}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return fallback
	}
	_ = json.Unmarshal(raw["checked_on"], &fallback.CheckedOn)
	var report map[string]json.RawMessage
	if json.Unmarshal(raw["report"], &report) == nil && report != nil {
		var current, newVersion string
		if json.Unmarshal(report["current"], &current) == nil && json.Unmarshal(report["new"], &newVersion) == nil {
			var delivered bool
			_ = json.Unmarshal(report["delivered"], &delivered)
			fallback.Report = &noticeReport{Current: current, New: newVersion, Delivered: delivered}
		}
	}
	if fallback.Report == nil { // migrate an undelivered notice created by an earlier release
		var pending map[string]json.RawMessage
		if json.Unmarshal(raw["pending"], &pending) == nil && pending != nil {
			var current, newVersion string
			if json.Unmarshal(pending["current"], &current) == nil && json.Unmarshal(pending["new"], &newVersion) == nil {
				var delivered bool
				_ = json.Unmarshal(pending["delivered"], &delivered)
				fallback.Report = &noticeReport{Current: current, New: newVersion, Delivered: delivered}
			}
		}
	}
	return fallback
}

func writeNoticeState(path string, state noticeState) error {
	return atomicJSON(path, state)
}

func recordCheck(path string) error {
	state := readNoticeState(path)
	today := time.Now().UTC().Format("2006-01-02")
	state.CheckedOn = &today
	return writeNoticeState(path, state)
}

func atomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	var data []byte
	data, err = json.Marshal(value)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func routeModel(route any) string {
	if entry, ok := route.(map[string]any); ok {
		model, _ := entry["model"].(string)
		return model
	}
	model, _ := route.(string)
	return model
}

func newerVersion(current string, candidates []string) string {
	base := versionPattern.FindStringSubmatch(current)
	if len(base) != 4 || base[1] == "" || base[3] == "" || !hasLetter(base[1]+base[3]) {
		return ""
	}
	best := ""
	bestParts := ""
	for _, candidate := range candidates {
		parts := versionPattern.FindStringSubmatch(candidate)
		if len(parts) != 4 || parts[1] != base[1] || parts[3] != base[3] || !hasLetter(parts[1]+parts[3]) {
			continue
		}
		if compareVersionNumbers(parts[2], base[2]) <= 0 {
			continue
		}
		if best == "" || compareVersionNumbers(parts[2], bestParts) > 0 || compareVersionNumbers(parts[2], bestParts) == 0 && candidate > best {
			best, bestParts = candidate, parts[2]
		}
	}
	return best
}

func compareVersionNumbers(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	for i := 0; i < length; i++ {
		av, bv := new(big.Int), new(big.Int)
		if i < len(left) {
			av.SetString(left[i], 10)
		}
		if i < len(right) {
			bv.SetString(right[i], 10)
		}
		if compared := av.Cmp(bv); compared != 0 {
			return compared
		}
	}
	return 0
}

func hasLetter(value string) bool {
	for _, char := range value {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' {
			return true
		}
	}
	return false
}

func has(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func appendUnique(values []string, value string) []string {
	if !has(values, value) {
		return append(values, value)
	}
	return values
}
