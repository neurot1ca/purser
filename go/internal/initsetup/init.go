// Package initsetup implements the native /purser:init CLI.
package initsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/jobs"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

type options struct {
	detect, list, plan, apply, concise, trust, scaffold, jsonOutput                   bool
	coder, model, fallbackAgent, fallbackModel, mode, effort, reviewer, reviewerModel string
	maintenance, mechanical, autoCompact                                              *bool
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Run parses and executes one init operation, returning the CLI exit code.
func Run(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := options{}
	fs.BoolVar(&o.detect, "detect", false, "detect installed agents and current setup")
	fs.BoolVar(&o.list, "list-models", false, "list models")
	fs.BoolVar(&o.plan, "plan", false, "show setup plan")
	fs.BoolVar(&o.apply, "apply", false, "apply setup")
	fs.BoolVar(&o.concise, "enable-concise-style", false, "enable concise style")
	fs.BoolVar(&o.jsonOutput, "json", false, "JSON output")
	fs.StringVar(&o.coder, "coder", "", "coder agent")
	fs.StringVar(&o.model, "model", "", "coder model")
	fs.StringVar(&o.fallbackAgent, "fallback-agent", "", "fallback agent")
	fs.StringVar(&o.fallbackModel, "fallback-model", "", "fallback model")
	fs.StringVar(&o.mode, "mode", "", "route mode")
	fs.StringVar(&o.effort, "effort", "", "model effort")
	fs.StringVar(&o.reviewer, "reviewer", "", "Reviewer-1 agent")
	fs.StringVar(&o.reviewerModel, "reviewer-model", "", "Reviewer-1 model")
	var maintenance, noMaintenance, mechanical, noMechanical, autoCompact, noAutoCompact bool
	fs.BoolVar(&maintenance, "maintenance", false, "enable maintenance reports")
	fs.BoolVar(&noMaintenance, "no-maintenance", false, "disable maintenance reports")
	fs.BoolVar(&mechanical, "opencode-mechanical", false, "enable OpenCode mechanical route")
	fs.BoolVar(&noMechanical, "no-opencode-mechanical", false, "disable OpenCode mechanical route")
	fs.BoolVar(&autoCompact, "auto-compact", false, "enable automatic /compact")
	fs.BoolVar(&noAutoCompact, "no-auto-compact", false, "disable automatic /compact")
	fs.BoolVar(&o.trust, "trust", false, "trust this project")
	fs.BoolVar(&o.scaffold, "scaffold-profile", false, "create project profile")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (maintenance && noMaintenance) || (mechanical && noMechanical) || (autoCompact && noAutoCompact) {
		fmt.Fprintln(stderr, "purser:init: invalid arguments")
		return 2
	}
	if maintenance {
		v := true
		o.maintenance = &v
	}
	if noMaintenance {
		v := false
		o.maintenance = &v
	}
	if mechanical {
		v := true
		o.mechanical = &v
	}
	if noMechanical {
		v := false
		o.mechanical = &v
	}
	if autoCompact {
		v := true
		o.autoCompact = &v
	}
	if noAutoCompact {
		v := false
		o.autoCompact = &v
	}
	count := 0
	for _, yes := range []bool{o.detect, o.list, o.plan, o.apply, o.concise} {
		if yes {
			count++
		}
	}
	if count != 1 {
		fmt.Fprintln(stderr, "purser:init: exactly one operation is required")
		return 2
	}
	if o.detect {
		if o.coder != "" && !oneOf(o.coder, "codex", "claude", "opencode", "agy") {
			fmt.Fprintln(stderr, "purser:init: unsupported coder")
			return 2
		}
		if !o.jsonOutput || o.mode != "" || o.effort != "" || o.reviewer != "" || o.reviewerModel != "" || o.fallbackAgent != "" || o.fallbackModel != "" || o.mechanical != nil || o.trust || o.scaffold || o.maintenance != nil || o.autoCompact != nil || (o.model != "" && o.coder == "") {
			fmt.Fprintln(stderr, "purser:init: --detect requires --json and accepts only optional --coder and --model")
			return 2
		}
	}
	if o.list && (!o.jsonOutput || !oneOf(o.coder, "codex", "opencode", "agy") || o.model != "" || o.mode != "" || o.effort != "" || o.reviewer != "" || o.reviewerModel != "" || o.fallbackAgent != "" || o.fallbackModel != "" || o.mechanical != nil || o.trust || o.scaffold || o.maintenance != nil || o.autoCompact != nil) {
		fmt.Fprintln(stderr, "purser:init: --list-models requires --json and --coder codex, opencode, or agy")
		return 2
	}
	if o.concise {
		if o.jsonOutput || o.coder != "" || o.model != "" || o.fallbackAgent != "" || o.fallbackModel != "" || o.mode != "" || o.effort != "" || o.reviewer != "" || o.reviewerModel != "" || o.mechanical != nil || o.trust || o.scaffold || o.maintenance != nil || o.autoCompact != nil {
			fmt.Fprintln(stderr, "purser:init: --enable-concise-style accepts no other options")
			return 2
		}
		return enableConcise(stdout, stderr)
	}
	if o.plan && o.jsonOutput || (!o.detect && !o.list && (o.coder == "" || o.reviewer == "")) {
		fmt.Fprintln(stderr, "purser:init: --plan requires --coder and --reviewer without --json; --apply requires --coder and --reviewer")
		return 2
	}
	if !o.detect && !o.list && (!oneOf(o.coder, "codex", "claude", "opencode", "agy") || !oneOf(o.reviewer, "agy", "opencode", "codex", "claude", "none")) {
		fmt.Fprintln(stderr, "purser:init: unsupported coder or reviewer")
		return 2
	}
	if o.fallbackAgent != "" != (o.fallbackModel != "") {
		fmt.Fprintln(stderr, "purser:init: --fallback-agent and --fallback-model must be used together")
		return 2
	}
	if o.fallbackAgent != "" && !oneOf(o.fallbackAgent, "codex", "claude", "opencode", "agy") {
		fmt.Fprintln(stderr, "purser:init: unsupported fallback agent")
		return 2
	}
	if o.coder == "claude" && o.fallbackAgent != "" {
		fmt.Fprintln(stderr, "purser:init: fallback selection is not available when Claude is the primary coder")
		return 2
	}
	if o.maintenance != nil && o.coder != "codex" {
		fmt.Fprintln(stderr, "purser:init: maintenance reports can only be configured when Codex is the coder")
		return 2
	}
	if o.mode != "" && !oneOf(o.mode, "balanced", "maximum", "manual") {
		fmt.Fprintln(stderr, "purser:init: invalid mode")
		return 2
	}
	if o.mode == "manual" && o.model == "" {
		fmt.Fprintln(stderr, "purser:init: --mode manual requires --model")
		return 2
	}
	if oneOf(o.mode, "balanced", "maximum") && (o.model != "" || o.effort != "") || o.mode == "" && o.effort != "" || o.mode == "" && o.model != "" && !oneOf(o.coder, "opencode", "agy") {
		fmt.Fprintln(stderr, "purser:init: invalid model or effort for selected mode")
		return 2
	}
	if o.reviewer == "none" && o.reviewerModel != "" || o.reviewerModel != "" && o.reviewer == "" {
		fmt.Fprintln(stderr, "purser:init: reviewer model is incompatible with reviewer selection")
		return 2
	}
	if oneOf(o.reviewer, "codex", "opencode", "claude") && o.reviewerModel == "" {
		fmt.Fprintf(stderr, "purser:init: --reviewer %s requires --reviewer-model\n", o.reviewer)
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "purser:init: cannot resolve project directory")
		return 1
	}
	root, _ = filepath.Abs(root)
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "purser:init: cannot resolve home directory")
		return 1
	}
	configPath := filepath.Join(home, ".orchestrator", "config.toml")
	if o.detect {
		value, e := detect(home, root, o)
		if e != nil {
			return printError(stderr, e)
		}
		return writeIndented(stdout, value)
	}
	if o.list {
		value, e := listModels(o.coder)
		if e != nil {
			return printError(stderr, e)
		}
		return writeIndented(stdout, value)
	}
	if err := validateSelection(o, home); err != nil {
		return printError(stderr, err)
	}
	return setup(stdout, stderr, home, root, configPath, o)
}

func oneOf(value string, choices ...string) bool {
	for _, x := range choices {
		if value == x {
			return true
		}
	}
	return false
}
func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func printError(w io.Writer, err error) int { fmt.Fprintf(w, "purser:init: %v\n", err); return 1 }
func writeIndented(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if enc.Encode(v) != nil {
		return 1
	}
	return 0
}

func executable(name string) string { p, _ := exec.LookPath(name); return p }
func agent(name string) map[string]any {
	p := executable(name)
	if name == "opencode" {
		if prefix, e := jobs.ResolveAgentPrefix(name, nil); e == nil && len(prefix) > 0 {
			p = prefix[0]
		}
	}
	if p == "" {
		return map[string]any{"available": false, "path": nil, "version": nil}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "--version")
	proc.ConfigureNoWindow(cmd)
	proc.ConfigureProcessTreeKill(cmd)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	version := any(nil)
	if cmd.Run() == nil {
		v := strings.TrimSpace(b.String())
		if v != "" {
			version = v
		}
	}
	return map[string]any{"available": true, "path": p, "version": version}
}

func detect(home, root string, o options) (map[string]any, error) {
	configPath := filepath.Join(home, ".orchestrator", "config.toml")
	data := map[string]any{}
	if raw, e := os.ReadFile(configPath); e == nil {
		var err error
		data, _, err = profile.DecodeTOML(raw)
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	providers := loadProviders(home)
	agents := map[string]any{}
	modelSets := map[string][]map[string]any{}
	for _, name := range []string{"codex", "claude", "opencode", "agy"} {
		a := agent(name)
		if name == "claude" {
			agents[name] = a
			continue
		}
		listing := discoverModels(name, providers)
		modelSets[name] = listing.models
		if name == "codex" {
			a["models"] = stringsFromModels(listing.models)
			a["options"] = modelOptions(name, listing.models)
		}
		if name == "agy" {
			a["models"] = stringsFromModels(listing.models)
			if len(listing.models) > 0 {
				a["options"] = modelOptions(name, listing.models)
			}
		}
		if name == "opencode" {
			a["models"] = stringsFromModels(listing.models)
			if len(listing.models) > 0 {
				a["options"] = modelOptions(name, listing.models)
			} else {
				a["reason"] = listing.reason
			}
		}
		agents[name] = a
	}
	roles, _ := data["roles"].(map[string]any)
	trust, _ := data["trust"].(map[string]any)
	trusted := false
	if paths, ok := trust["paths"].([]any); ok {
		for _, p := range paths {
			if s, ok := p.(string); ok && samePath(s, root) {
				trusted = true
			}
		}
	}
	maintenanceEnabled := any(nil)
	if canonical, e := gitProjectRoot(root); e == nil {
		key := contextio.ProjectKey(canonical)
		path := filepath.Join(stateRootPath(), "projects", key, "maintenance", "opt-in.json")
		if raw, e := os.ReadFile(path); e == nil {
			var marker map[string]any
			if json.Unmarshal(raw, &marker) == nil && marker["project_root"] == canonical {
				if b, ok := marker["enabled"].(bool); ok {
					maintenanceEnabled = b
				}
			}
		}
	}
	autoCompactEnabled := any(nil)
	if raw, e := os.ReadFile(filepath.Join(stateRootPath(), "auto-compact.json")); e == nil {
		var marker map[string]any
		if json.Unmarshal(raw, &marker) == nil {
			if b, ok := marker["enabled"].(bool); ok {
				autoCompactEnabled = b
			}
		}
	}
	coderFamily := selectedFamily(data, o, providers)
	reviewerOptions := []map[string]any{}
	for _, name := range []string{"codex", "agy", "opencode"} {
		for _, m := range modelSets[name] {
			family := vendorFamily(asString(m["family"]))
			entry := map[string]any{"agent": name, "model": m["id"], "label": m["id"], "family": family}
			if family == "" {
				entry["family"] = nil
			}
			if coderFamily != "" {
				entry["same_family"] = family == coderFamily
			}
			reviewerOptions = append(reviewerOptions, entry)
		}
	}
	var familyValue any
	if coderFamily != "" {
		familyValue = coderFamily
	}
	return map[string]any{"agents": agents, "reviewer_options": reviewerOptions, "coder_family": familyValue, "roles": defaultMap(roles), "trusted": trusted, "profile_exists": exists(filepath.Join(root, ".orchestrator", "profile.toml")), "maintenance_enabled": maintenanceEnabled, "auto_compact_enabled": autoCompactEnabled}, nil
}
func defaultMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
func exists(p string) bool { _, e := os.Stat(p); return e == nil }
func samePath(a, b string) bool {
	x, _ := filepath.Abs(a)
	y, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(x), filepath.Clean(y))
}

func listModels(agent string) (map[string]any, error) {
	listing := discoverModels(agent, loadProviders(mustHome()))
	result := map[string]any{"status": "available", "models": listing.models, "source": listing.source}
	if len(listing.models) == 0 {
		result["status"] = "unavailable"
		result["models"] = []any{}
		result["source"] = listing.source
		result["reason"] = listing.reason
	}
	return result, nil
}

type discovered struct {
	models         []map[string]any
	source, reason string
}

func mustHome() string { h, _ := os.UserHomeDir(); return h }
func loadProviders(home string) map[string]map[string]map[string]any {
	result := map[string]map[string]map[string]any{}
	raw, e := os.ReadFile(filepath.Join(stateRootPath(), "model-catalog", "catalog.json"))
	if e != nil {
		return result
	}
	var envelope struct {
		Providers map[string]struct {
			Models map[string]map[string]any `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return result
	}
	for p, v := range envelope.Providers {
		result[p] = v.Models
	}
	return result
}
func discoverModels(name string, providers map[string]map[string]map[string]any) discovered {
	arg := []string{"models"}
	binary := executable(name)
	if name == "opencode" {
		if prefix, e := jobs.ResolveAgentPrefix(name, nil); e == nil && len(prefix) > 0 {
			binary = prefix[0]
		}
	}
	if name == "codex" {
		binary = executable("codex")
		arg = []string{"debug", "models"}
	}
	source := name + " " + strings.Join(arg, " ")
	if binary == "" {
		reason := "binary not found"
		if name == "opencode" {
			reason = "OpenCode command and platform binary failed"
		}
		return discovered{source: source, reason: reason}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, arg...)
	proc.ConfigureNoWindow(cmd)
	proc.ConfigureProcessTreeKill(cmd)
	raw, e := cmd.Output()
	if e != nil {
		return discovered{source: source, reason: e.Error()}
	}
	items := []map[string]any{}
	if name == "codex" {
		var value struct {
			Models []struct {
				Slug, Visibility string
				Levels           []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		if json.Unmarshal(raw, &value) != nil || value.Models == nil {
			return discovered{source: source, reason: "invalid codex model listing"}
		}
		for _, m := range value.Models {
			if m.Visibility != "list" {
				continue
			}
			efforts := []string{}
			for _, l := range m.Levels {
				efforts = append(efforts, l.Effort)
			}
			items = append(items, modelFact(m.Slug, efforts, providers, source))
		}
	}
	if name == "opencode" {
		for _, line := range strings.Split(string(raw), "\n") {
			id := strings.TrimSpace(line)
			if strings.Contains(id, "/") {
				items = append(items, modelFact(id, nil, providers, source))
			}
		}
	}
	if name == "agy" {
		group := map[string]map[string]any{}
		order := []string{}
		for _, line := range strings.Split(string(raw), "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			id := parts[0]
			base := id
			effort := ""
			if i := strings.LastIndex(id, "-"); i > 0 && oneOf(id[i+1:], "low", "medium", "high", "max") {
				base = id[:i]
				effort = id[i+1:]
			}
			item, ok := group[base]
			if !ok {
				effs := []string{}
				item = modelFact(base, effs, providers, source)
				group[base] = item
				order = append(order, base)
			}
			if effort != "" {
				item["efforts"] = append(item["efforts"].([]string), effort)
				variants, _ := item["variants"].(map[string]string)
				if variants == nil {
					variants = map[string]string{}
				}
				variants[effort] = id
				item["variants"] = variants
			} else {
				item["efforts"] = nil
			}
		}
		for _, id := range order {
			items = append(items, group[id])
		}
	}
	if len(items) == 0 {
		return discovered{source: source, reason: "invalid or empty model listing"}
	}
	return discovered{models: items, source: source}
}
func modelFact(id string, efforts []string, providers map[string]map[string]map[string]any, source string) map[string]any {
	var fam any
	provider, bare, ok := strings.Cut(id, "/")
	if ok {
		if m := providers[provider][bare]; m != nil {
			fam = m["family"]
		}
	} else {
		for _, models := range providers {
			if m := models[id]; m != nil {
				if fam != nil {
					fam = nil
					break
				}
				fam = m["family"]
			}
		}
	}
	if fam == nil {
		fam = modelFamily(id)
	}
	return map[string]any{"id": id, "efforts": efforts, "family": fam, "source": source}
}
func modelFamily(id string) string {
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "anthropic"
	case strings.HasPrefix(id, "gemini-"):
		return "google"
	case strings.HasPrefix(id, "gpt-"):
		return "openai"
	}
	if strings.Contains(id, "/") {
		return strings.SplitN(id, "/", 2)[0]
	}
	return ""
}
func stringsFromModels(items []map[string]any) []any {
	out := []any{}
	for _, m := range items {
		out = append(out, m["id"])
	}
	return out
}
func modelOptions(agent string, models []map[string]any) []any {
	entries := []map[string]any{}
	for _, m := range models {
		entries = append(entries, m)
	}
	if agent != "opencode" {
		out := []any{}
		for _, m := range entries {
			id := asString(m["id"])
			out = append(out, map[string]any{"label": id, "models": []string{id}, "family": vendorFamily(asString(m["family"]))})
		}
		return out
	}
	free := []string{}
	out := []any{}
	for _, m := range entries {
		id := asString(m["id"])
		if id == "opencode/big-pickle" || strings.HasPrefix(id, "opencode/") && strings.HasSuffix(id, "-free") {
			free = append(free, id)
		} else {
			out = append(out, map[string]any{"label": id, "models": []string{id}, "family": vendorFamily(asString(m["family"]))})
		}
	}
	if len(free) > 0 {
		out = append([]any{map[string]any{"label": "Free OpenCode models (Purser races them)", "models": free, "family": "opencode"}}, out...)
	}
	return out
}
func asString(v any) string { s, _ := v.(string); return s }
func vendorFamily(f string) string {
	if f == "" {
		return ""
	}
	if strings.HasPrefix(f, "claude") || f == "anthropic" {
		return "anthropic"
	}
	if strings.HasPrefix(f, "gpt") || f == "openai" {
		return "openai"
	}
	if strings.HasPrefix(f, "gemini") || f == "google" {
		return "google"
	}
	return f
}
func selectedFamily(data map[string]any, o options, providers map[string]map[string]map[string]any) string {
	roles, _ := data["roles"].(map[string]any)
	coding, _ := data["coding"].(map[string]any)
	coder := o.coder
	if coder == "" {
		coder = asString(roles["coder"])
	}
	if coder == "claude" {
		return "anthropic"
	}
	if !oneOf(coder, "codex", "agy", "opencode") {
		return ""
	}
	model := o.model
	if model == "" && roles["coder"] == coder {
		routes, _ := coding["routes"].(map[string]any)
		route := routes["normal"]
		if m, ok := route.(map[string]any); ok {
			model = asString(m["model"])
		} else {
			model = asString(route)
		}
	}
	if model == "" && coder == "codex" {
		var cfg map[string]any
		raw, _ := os.ReadFile(obsidian.ConfigPath())
		_ = json.Unmarshal(raw, &cfg)
		r, _ := cfg["routes"].(map[string]any)
		m, _ := r["normal"].(map[string]any)
		model = asString(m["model"])
	}
	return vendorFamily(asString(modelFact(model, nil, providers, "")["family"]))
}
func validateSelection(o options, home string) error {
	if o.mode == "balanced" && o.coder != "codex" {
		return errors.New("balanced mode has no reviewed policy for this coder")
	}
	if o.mode == "maximum" && o.coder != "codex" {
		return fmt.Errorf("maximum mode has no reviewed model policy for %s", o.coder)
	}
	if o.mode == "manual" || o.mode == "maximum" || o.coder == "opencode" || o.coder == "agy" {
		listing := discoverModels(o.coder, loadProviders(home))
		if len(listing.models) == 0 {
			return fmt.Errorf("%s model listing unavailable: %s", o.coder, listing.reason)
		}
		want := o.model
		if o.mode == "maximum" {
			want = "gpt-6-astra"
		}
		var selected map[string]any
		for _, m := range listing.models {
			if m["id"] == want {
				selected = m
			}
		}
		if selected == nil && o.mode != "balanced" {
			return fmt.Errorf("model %q is absent from the live %s listing", want, o.coder)
		}
		if selected != nil {
			levels, _ := selected["efforts"].([]string)
			if o.mode == "maximum" {
				if len(levels) == 0 {
					return fmt.Errorf("%s has no listed effort levels", want)
				}
			} else if o.coder == "codex" {
				if !modelPattern.MatchString(want) || !oneOf(o.effort, "low", "medium", "high", "xhigh", "max", "ultra") {
					return errors.New("listed Codex model or effort is invalid for Purser routes")
				}
				if len(levels) > 0 && !contains(levels, o.effort) {
					return fmt.Errorf("effort %q is absent from the live listing for %s", o.effort, want)
				}
			} else if o.coder == "opencode" && o.effort != "" {
				return errors.New("listed OpenCode model or effort is invalid for Purser routes")
			} else if o.coder == "agy" && o.effort != "" && !contains(levels, o.effort) {
				return fmt.Errorf("effort %q is absent from the live listing for %s", o.effort, want)
			}
		}
	}
	if o.fallbackAgent != "" {
		if o.fallbackAgent == "claude" {
			if executable("claude") == "" || !strings.HasPrefix(o.fallbackModel, "claude-") {
				return errors.New("fallback coder or model is not detected as working")
			}
		} else {
			listing := discoverModels(o.fallbackAgent, loadProviders(home))
			found := false
			for _, m := range listing.models {
				if m["id"] == o.fallbackModel {
					found = true
				}
			}
			if !found {
				return errors.New("fallback coder or model is not detected as working")
			}
		}
		if o.fallbackModel == o.model {
			return errors.New("fallback model must differ from the primary coder model")
		}
		if fallbackFamily(o.fallbackAgent, o.fallbackModel) == "" {
			return errors.New("fallback model family is unavailable")
		}
	}
	if o.reviewerModel != "" {
		if o.reviewer == "claude" {
			if executable("claude") == "" || !strings.HasPrefix(o.reviewerModel, "claude-") {
				return errors.New("selected Reviewer-1 model is absent from claude models")
			}
		} else {
			listing := discoverModels(o.reviewer, loadProviders(home))
			found := false
			for _, m := range listing.models {
				if m["id"] == o.reviewerModel {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("selected Reviewer-1 model is absent from %s models", o.reviewer)
			}
		}
	}
	return nil
}
func contains(items []string, value string) bool {
	for _, x := range items {
		if x == value {
			return true
		}
	}
	return false
}

func setup(out, errOut io.Writer, home, root, configPath string, o options) int {
	original, e := os.ReadFile(configPath)
	if e != nil && !os.IsNotExist(e) {
		return printError(errOut, e)
	}
	if os.IsNotExist(e) {
		original = nil
	}
	text := string(original)
	if text == "" {
		text = "schema_version = 1\n"
	}
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	values := map[string]any{}
	if len(original) > 0 {
		var er error
		values, _, er = profile.DecodeTOML(original)
		if er != nil {
			return printError(errOut, er)
		}
	}
	if e = editConfig(&text, values, root, o, newline); e != nil {
		return printError(errOut, e)
	}
	changed := []byte(text)
	profilePath := filepath.Join(root, ".orchestrator", "profile.toml")
	var scaffold []byte
	if o.scaffold && !exists(profilePath) {
		scaffold = []byte(profileText(root))
	}
	handoff := filepath.Join(stateRootPath(), "projects", contextio.ProjectKey(root), "telemetry", "purser-handoff.md")
	if o.plan {
		planned := map[string]any{}
		showRoutes := o.mode != "" || oneOf(o.coder, "opencode", "agy", "claude")
		if showRoutes {
			if o.coder == "claude" {
				planned = map[string]any{
					"mechanical":    map[string]any{"model": "haiku", "effort": "none"},
					"operation":     map[string]any{"model": "haiku", "effort": "none"},
					"subtle":        map[string]any{"model": "sonnet", "effort": "medium"},
					"normal":        map[string]any{"model": "sonnet", "effort": "medium"},
					"consequential": map[string]any{"model": "opus", "effort": "high"},
					"plan_standard": map[string]any{"model": "opus", "effort": "high"},
					"escalated":     map[string]any{"model": "opus", "effort": "xhigh"},
				}
			} else {
				parsed, _, er := profile.DecodeTOML(changed)
				if er == nil {
					coding, _ := parsed["coding"].(map[string]any)
					planned, _ = coding["routes"].(map[string]any)
				}
			}
		}
		if showRoutes && len(planned) > 0 {
			fmt.Fprintln(out, "Resolved routes:")
			for _, r := range routeNames(o.coder) {
				v, ok := planned[r]
				if !ok {
					continue
				}
				model, effort := "", "none"
				if m, ok := v.(map[string]any); ok {
					model = fmt.Sprint(m["model"])
					if e, ok := m["effort"].(string); ok {
						effort = e
					}
				} else {
					model = fmt.Sprint(v)
				}
				fmt.Fprintf(out, "  %s -> %s / %s\n", r, model, effort)
			}
		}
		if o.maintenance != nil {
			if *o.maintenance {
				fmt.Fprintln(out, "Maintenance reports: enabled for this project")
			} else {
				fmt.Fprintln(out, "Maintenance reports: disabled for this project")
			}
		}
		if o.autoCompact != nil {
			if *o.autoCompact {
				fmt.Fprintln(out, "Automatic /compact: enabled for this user")
			} else {
				fmt.Fprintln(out, "Automatic /compact: disabled for this user")
			}
		}
		if !bytes.Equal(original, changed) {
			fmt.Fprint(out, unifiedInitialDiff(configPath, string(original), string(changed)))
		}
		if scaffold != nil {
			fmt.Fprintf(out, "Create %s:\n%s", profilePath, scaffold)
		}
		if !exists(handoff) {
			fmt.Fprintf(out, "Create %s:\n%s", handoff, handoffTemplate)
		}
		if bytes.Equal(original, changed) && scaffold == nil && exists(handoff) {
			fmt.Fprintln(out, "No changes.")
		}
		return 0
	}
	if !bytes.Equal(original, changed) {
		if now, e := os.ReadFile(configPath); e == nil && !bytes.Equal(now, original) {
			return printError(errOut, errors.New("config changed during setup; run the plan again"))
		}
		if len(original) > 0 {
			if e = backup(configPath, original); e != nil {
				return printError(errOut, e)
			}
		}
		if e = atomicWrite(configPath, changed, false); e != nil {
			return printError(errOut, e)
		}
	}
	if scaffold != nil {
		if e = atomicWrite(profilePath, scaffold, true); e != nil && !os.IsExist(e) {
			return printError(errOut, e)
		}
	}
	handoffCreated := false
	if !exists(handoff) {
		if e = atomicWrite(handoff, []byte(handoffTemplate), true); e == nil {
			handoffCreated = true
		} else if !os.IsExist(e) {
			return printError(errOut, e)
		}
	}
	if o.maintenance != nil {
		canonical, e := gitProjectRoot(root)
		if e != nil {
			return printError(errOut, e)
		}
		key := contextio.ProjectKey(canonical)
		marker := filepath.Join(stateRootPath(), "projects", key, "maintenance", "opt-in.json")
		rootJSON, _ := json.Marshal(canonical)
		b := []byte(fmt.Sprintf("{\"project_root\": %s, \"enabled\": %t}", rootJSON, *o.maintenance))
		if e = atomicWrite(marker, b, false); e != nil {
			return printError(errOut, e)
		}
	}
	if o.autoCompact != nil {
		marker := filepath.Join(stateRootPath(), "auto-compact.json")
		if e = atomicWrite(marker, []byte(fmt.Sprintf("{\"enabled\": %t}", *o.autoCompact)), false); e != nil {
			return printError(errOut, e)
		}
	}
	offerDue := claimConcise(home, root)
	if o.jsonOutput {
		return writeIndented(out, map[string]any{"status": "applied", "handoff": handoff, "concise_style_offer_due": offerDue})
	}
	if !bytes.Equal(original, changed) || scaffold != nil || handoffCreated {
		fmt.Fprintln(out, "Applied.")
	} else {
		fmt.Fprintln(out, "No changes.")
	}
	fmt.Fprintf(out, "Handoff: %s\n", handoff)
	if offerDue {
		fmt.Fprintln(out, "Concise reply style offer due.")
	}
	return 0
}

func routeNames(coder string) []string {
	return []string{"mechanical", "subtle", "normal", "consequential", "escalated", "operation", "plan_standard"}
}
func routeModel(o options) string {
	if o.model != "" {
		return o.model
	}
	return "gpt-6.1-sol"
}
func routeEffort(o options) string {
	if o.effort != "" {
		return o.effort
	}
	return "medium"
}

const handoffTemplate = "# Purser handoff\n## Goal\nUser language: <the language the user writes in>\n## State\n## Decisions\n## Next\n## Open risks\n## Files to load\n<!-- PURSER:STATE:START -->\n<!-- PURSER:STATE:END -->\n"

func stateRootPath() string {
	path := ""
	if p := os.Getenv("PURSER_STATE_DIR"); p != "" {
		path = p
	} else if p := os.Getenv("CLAUDE_CONFIG_DIR"); p != "" {
		path = filepath.Join(p, "purser")
	} else {
		h, _ := os.UserHomeDir()
		path = filepath.Join(h, ".claude", "purser")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if h, e := os.UserHomeDir(); e == nil {
			if path == "~" {
				path = h
			} else {
				path = filepath.Join(h, path[2:])
			}
		}
	}
	absolute, e := filepath.Abs(filepath.Clean(path))
	if e != nil {
		return path
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if real, e := filepath.EvalSymlinks(current); e == nil {
			rel, _ := filepath.Rel(current, absolute)
			return filepath.Join(real, rel)
		}
		if filepath.Dir(current) == current {
			return filepath.Clean(absolute)
		}
	}
}
func unifiedInitialDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	a := strings.SplitAfter(before, "\n")
	b := strings.SplitAfter(after, "\n")
	if len(a) > 0 && a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	if len(b) > 0 && b[len(b)-1] == "" {
		b = b[:len(b)-1]
	}
	if before == "" {
		a = nil
	}
	if after == "" {
		b = nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", path, path)
	if len(a) == 0 {
		if len(b) == 1 {
			fmt.Fprintln(&out, "@@ -0,0 +1 @@")
		} else {
			fmt.Fprintf(&out, "@@ -0,0 +1,%d @@\n", len(b))
		}
		for _, line := range b {
			out.WriteByte('+')
			out.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				out.WriteByte('\n')
			}
		}
		return out.String()
	}
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, line := range a {
		out.WriteByte('-')
		out.WriteString(line)
	}
	for _, line := range b {
		out.WriteByte('+')
		out.WriteString(line)
	}
	return out.String()
}
func gitProjectKey(root string) (string, error) {
	canonical, e := gitProjectRoot(root)
	if e != nil {
		return "", e
	}
	return contextio.ProjectKey(canonical), nil
}
func gitProjectRoot(root string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	proc.Hide(cmd)
	cmd.Dir = root
	out, e := cmd.Output()
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(out)), nil
}
func claimConcise(home, project string) bool {
	style, _ := effectiveStyle(home, project)
	if style == "Concise" {
		return false
	}
	marker := filepath.Join(home, ".orchestrator", "reply-style-suggestion-shown")
	if e := os.MkdirAll(filepath.Dir(marker), 0700); e != nil {
		return false
	}
	f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return false
	}
	_ = f.Close()
	return true
}
func effectiveStyle(home, project string) (string, bool) {
	for _, p := range []string{filepath.Join(project, ".claude", "settings.local.json"), filepath.Join(project, ".claude", "settings.json")} {
		if raw, e := os.ReadFile(p); e == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				return "", true
			}
			if v, ok := m["outputStyle"]; ok {
				s, _ := v.(string)
				return s, true
			}
		}
	}
	raw, e := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if e != nil {
		return "", false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	s, _ := m["outputStyle"].(string)
	return s, false
}

var sectionRE = regexp.MustCompile(`(?m)^\s*\[([^\[\]]+)\]\s*(?:#.*)?\r?$`)
var keyRE = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=`)

func editConfig(text *string, values map[string]any, root string, o options, nl string) error {
	before := map[string]any{}
	copyJSON(values, &before)
	roles, ok := before["roles"].(map[string]any)
	if !ok {
		if before["roles"] != nil {
			return errors.New("[roles] must be a table")
		}
		roles = map[string]any{}
		before["roles"] = roles
	}
	for _, k := range []string{"orchestrator", "coder", "reviewer"} {
		v := map[string]any{"orchestrator": "claude-code", "coder": o.coder, "reviewer": o.reviewer}[k]
		roles[k] = v
		*text = upsert(*text, "roles", k, tomlString(fmt.Sprint(v)), nl)
	}
	if o.reviewer != "none" && o.reviewerModel != "" {
		reviewer, ok := before["reviewer"].(map[string]any)
		if !ok {
			reviewer = map[string]any{}
			before["reviewer"] = reviewer
		}
		reviewer["selected"] = map[string]any{"agent": o.reviewer, "model": o.reviewerModel}
		*text = upsert(*text, "reviewer", "selected", fmt.Sprintf("{ agent = %s, model = %s }", tomlString(o.reviewer), tomlString(o.reviewerModel)), nl)
		if exe := reviewerExecutable(o.reviewer); exe != "" {
			reviewer["executable"] = exe
			*text = upsert(*text, "reviewer", "executable", tomlString(exe), nl)
			if o.reviewer == "claude" {
				c, ok := before["coding"].(map[string]any)
				if !ok {
					c = map[string]any{}
					before["coding"] = c
				}
				c["reviewer1_executable"] = exe
				*text = upsert(*text, "coding", "reviewer1_executable", tomlString(exe), nl)
			}
		}
		if o.reviewer == "claude" {
			c, ok := before["coding"].(map[string]any)
			if !ok {
				c = map[string]any{}
				before["coding"] = c
			}
			c["reviewer1_agent"] = "claude"
			c["reviewer1_model_requested"] = o.reviewerModel
			c["reviewer1_effort"] = "medium"
			*text = upsert(*text, "coding", "reviewer1_agent", tomlString("claude"), nl)
			*text = upsert(*text, "coding", "reviewer1_model_requested", tomlString(o.reviewerModel), nl)
			*text = upsert(*text, "coding", "reviewer1_effort", tomlString("medium"), nl)
		}
	}
	if o.mechanical != nil {
		m, ok := before["mechanical"].(map[string]any)
		if !ok {
			m = map[string]any{}
			before["mechanical"] = m
		}
		m["opencode_enabled"] = *o.mechanical
		v := "false"
		if *o.mechanical {
			v = "true"
		}
		*text = upsert(*text, "mechanical", "opencode_enabled", v, nl)
	}
	if o.fallbackAgent != "" {
		fam := fallbackFamily(o.fallbackAgent, o.fallbackModel)
		f := map[string]any{"agent": o.fallbackAgent, "model": o.fallbackModel, "family": fam}
		if o.fallbackAgent == "claude" {
			f["effort"] = "medium"
		}
		before["fallback"] = f
		for _, k := range []string{"agent", "model", "family", "effort"} {
			if v, ok := f[k]; ok {
				*text = upsert(*text, "fallback", k, tomlString(fmt.Sprint(v)), nl)
			}
		}
	}
	if o.coder == "opencode" || o.coder == "agy" || o.coder == "claude" || o.mode != "" || before["coding"] != nil {
		c, ok := before["coding"].(map[string]any)
		if !ok {
			if before["coding"] != nil {
				return errors.New("[coding] must be a table")
			}
			c = map[string]any{}
			before["coding"] = c
		}
		agentName := o.coder
		if !oneOf(agentName, "opencode", "agy", "claude") {
			agentName = "codex"
		}
		c["coder_agent"] = agentName
		*text = upsert(*text, "coding", "coder_agent", tomlString(agentName), nl)
		if o.coder == "claude" {
			delete(c, "routes")
			*text = removeCoderRoutes(*text)
		}
		routes := map[string]any{}
		if o.mode == "balanced" || o.mode == "" && o.coder == "codex" {
			var cfg map[string]any
			raw, _ := os.ReadFile(obsidian.ConfigPath())
			_ = json.Unmarshal(raw, &cfg)
			routes, _ = cfg["routes"].(map[string]any)
		} else if o.mode == "maximum" {
			models := discoverModels("codex", loadProviders(mustHome())).models
			effort := ""
			order := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
			for _, m := range models {
				if m["id"] == "gpt-6-astra" {
					if levels, ok := m["efforts"].([]string); ok {
						for _, allowed := range order {
							if contains(levels, allowed) {
								effort = allowed
							}
						}
					}
				}
			}
			for _, name := range routeNames(o.coder) {
				routes[name] = map[string]any{"model": "gpt-6-astra", "effort": effort}
			}
		} else if o.mode != "" || o.model != "" {
			for _, name := range routeNames(o.coder) {
				if o.coder == "agy" {
					routes[name] = map[string]any{"model": o.model, "effort": "medium"}
				} else if o.coder == "opencode" {
					routes[name] = o.model
				} else {
					routes[name] = map[string]any{"model": o.model, "effort": o.effort}
				}
			}
		}
		if routes != nil && o.coder != "claude" {
			c["routes"] = routes
			*text = upsert(*text, "coding", "routes", tomlRoutes(routes), nl)
		}
	}
	if o.trust {
		t, ok := before["trust"].(map[string]any)
		if !ok {
			t = map[string]any{}
			before["trust"] = t
		}
		paths, ok := t["paths"].([]any)
		if !ok && t["paths"] != nil {
			return errors.New("[trust] paths must be an array of strings")
		}
		for _, p := range paths {
			s, ok := p.(string)
			if !ok {
				return errors.New("[trust] paths must be an array of strings")
			}
			if samePath(s, root) {
				return nil
			}
		}
		t["paths"] = append(paths, root)
		rendered := make([]string, 0, len(paths)+1)
		for _, p := range append(paths, root) {
			s := fmt.Sprint(p)
			if !strings.Contains(s, "'") {
				rendered = append(rendered, "'"+s+"'")
			} else {
				rendered = append(rendered, tomlString(s))
			}
		}
		*text = upsert(*text, "trust", "paths", "["+strings.Join(rendered, ", ")+"]", nl)
	}
	if _, _, e := profile.DecodeTOML([]byte(*text)); e != nil {
		return e
	}
	return nil
}
func copyJSON(src map[string]any, dst *map[string]any) {
	b, _ := json.Marshal(src)
	_ = json.Unmarshal(b, dst)
}
func tomlString(s string) string { b, _ := json.Marshal(s); return string(b) }
func tomlRoutes(routes map[string]any) string {
	parts := []string{}
	for _, name := range routeNames("") {
		if v, ok := routes[name]; ok {
			if m, ok := v.(map[string]any); ok {
				parts = append(parts, fmt.Sprintf("%s = { model = %s, effort = %s }", name, tomlString(fmt.Sprint(m["model"])), tomlString(fmt.Sprint(m["effort"]))))
			} else {
				parts = append(parts, fmt.Sprintf("%s = %s", name, tomlString(fmt.Sprint(v))))
			}
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
func upsert(text, section, key, value, nl string) string {
	lines := strings.SplitAfter(text, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		m := sectionRE.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m != nil {
			if start >= 0 {
				end = i
				break
			}
			if m[1] == section {
				start = i
			}
		}
	}
	if start < 0 {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += nl
		}
		if strings.TrimSpace(text) != "" {
			text += nl
		}
		return text + "[" + section + "]" + nl + key + " = " + value + nl
	}
	hits := []int{}
	for i := start + 1; i < end; i++ {
		m := keyRE.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m != nil && m[1] == key {
			hits = append(hits, i)
		}
	}
	if len(hits) > 1 {
		return text
	}
	if len(hits) == 1 {
		i, j := hits[0], hits[0]+1
		if section == "trust" && key == "paths" {
			if _, rhs, ok := strings.Cut(lines[i], "="); ok && strings.HasPrefix(strings.TrimSpace(rhs), "[") {
				if endArray, valid := tomlArrayEnd(lines, i, end); valid {
					j = endArray
				}
			}
		}
		lines = append(lines[:i], append([]string{key + " = " + value + nl}, lines[j:]...)...)
	} else {
		lines = append(lines[:end], append([]string{key + " = " + value + nl}, lines[end:]...)...)
	}
	return strings.Join(lines, "")
}
func removeCoderRoutes(text string) string {
	lines := strings.SplitAfter(text, "\n")
	kept := lines[:0]
	section, inRoutes := "", false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if match := sectionRE.FindStringSubmatch(trimmed); match != nil {
			section = match[1]
			inRoutes = section == "coding.routes" || strings.HasPrefix(section, "coding.routes.")
			if inRoutes {
				continue
			}
		}
		if inRoutes {
			continue
		}
		if section == "coding" {
			if match := keyRE.FindStringSubmatch(trimmed); match != nil && match[1] == "routes" {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}
func tomlArrayEnd(lines []string, start, limit int) (int, bool) {
	depth := 0
	var quote rune
	escaped := false
	for i := start; i < limit; i++ {
		part := lines[i]
		if i == start {
			_, rhs, _ := strings.Cut(part, "=")
			part = rhs
		}
		for _, ch := range part {
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
				continue
			}
			if ch == '"' || ch == '\'' {
				quote = ch
				continue
			}
			if ch == '#' {
				break
			}
			if ch == '[' {
				depth++
			}
			if ch == ']' {
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
	}
	return 0, false
}
func profileText(root string) string {
	base := "schema_version = 1\n[project]\nid = " + tomlString(filepath.Base(root)) + "\n[paths]\nprotected = []\n"
	if exists(filepath.Join(root, "AGENTS.md")) {
		base += "[authority]\nsources = [{ path = \"AGENTS.md\", headings = [\"Authority\", \"Working scope\", \"Proportional validation\", \"Test and comment quality\"] }]\n"
	}
	return base
}
func atomicWrite(path string, content []byte, exclusive bool) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(content); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if exclusive {
		if e = os.Link(tmp, path); e != nil {
			return e
		}
		return nil
	}
	return os.Rename(tmp, path)
}
func backup(path string, raw []byte) error {
	stamp := time.Now().UTC().Format("20060102T150405000000Z")
	f, e := os.OpenFile(path+".bak-"+stamp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(raw); e != nil {
		return e
	}
	return f.Sync()
}
func fallbackFamily(agent, model string) string {
	if agent == "claude" {
		return "anthropic"
	}
	if agent == "agy" {
		return "google"
	}
	if agent == "codex" {
		return "openai"
	}
	for _, m := range discoverModels(agent, loadProviders(mustHome())).models {
		if m["id"] == model {
			return vendorFamily(asString(m["family"]))
		}
	}
	return ""
}
func reviewerExecutable(agent string) string {
	if agent == "opencode" {
		if prefix, e := jobs.ResolveAgentPrefix(agent, nil); e == nil && len(prefix) > 0 {
			return prefix[0]
		}
		return ""
	}
	if oneOf(agent, "agy", "codex", "claude") {
		return executable(agent)
	}
	return ""
}
func enableConcise(out, errOut io.Writer) int {
	home, e := os.UserHomeDir()
	if e != nil {
		return printError(errOut, e)
	}
	path := filepath.Join(contextio.ClaudeConfigDir(), "settings.json")
	old, _ := os.ReadFile(path)
	data := map[string]any{}
	if len(old) > 0 {
		if json.Unmarshal(old, &data) != nil {
			return printError(errOut, errors.New("Claude settings must contain a JSON object"))
		}
		if data == nil {
			return printError(errOut, errors.New("Claude settings must contain a JSON object"))
		}
	}
	data["outputStyle"] = "Concise"
	now, _ := os.ReadFile(path)
	if !bytes.Equal(old, now) {
		return printError(errOut, errors.New("Claude settings changed during the update; try again"))
	}
	encoded, _ := json.MarshalIndent(data, "", "  ")
	if e = atomicWrite(path, append(encoded, '\n'), false); e != nil {
		return printError(errOut, e)
	}
	fmt.Fprintln(out, "Enabled Claude Code Concise reply style.")
	if _, override := effectiveStyle(home, mustCwd()); override {
		fmt.Fprintln(out, "A project-level outputStyle override still takes precedence for this project.")
	}
	return 0
}
func mustCwd() string { p, _ := os.Getwd(); return p }
