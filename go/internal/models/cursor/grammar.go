package cursor

import (
	"regexp"
	"strconv"
	"strings"
)

var listingID = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
var versionToken = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)
var kimiVersionToken = regexp.MustCompile(`^k[0-9]+(?:\.[0-9]+)*$`)

// ValidModel accepts concrete Cursor listing IDs and explicit family selectors.
func ValidModel(model string) bool {
	if strings.HasPrefix(model, "latest:") {
		return strings.TrimPrefix(model, "latest:") != "" && listingID.MatchString(strings.TrimPrefix(model, "latest:"))
	}
	return model != "auto" && listingID.MatchString(model)
}

// VendorFamily returns the known vendor family for a Cursor model or selector.
func VendorFamily(model string) string {
	model = strings.TrimPrefix(model, "latest:")
	model = strings.TrimPrefix(model, "cursor-")
	first, _, _ := strings.Cut(model, "-")
	switch first {
	case "gpt", "codex":
		return "openai"
	case "claude":
		return "anthropic"
	case "gemini":
		return "google"
	case "grok":
		return "xai"
	case "composer":
		return "cursor"
	case "kimi":
		return "moonshot"
	case "glm":
		return "zhipu"
	default:
		return ""
	}
}

// Family returns the model family key after removing version tokens.
func Family(model string) string {
	model = strings.TrimPrefix(model, "latest:")
	model = strings.TrimPrefix(model, "cursor-")
	parts := strings.Split(model, "-")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if versionToken.MatchString(part) || (parts[0] == "kimi" && kimiVersionToken.MatchString(part)) {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "-")
}

type candidate struct {
	id      string
	version []int
}

// ResolveLatest selects the highest-version listed ID for a family key.
func ResolveLatest(ids []string, family string) string {
	if family == "" || family == "auto" {
		return ""
	}
	var best candidate
	found := false
	for _, id := range ids {
		if id == "auto" || Family(id) != family {
			continue
		}
		current := candidate{id: id, version: versionTokens(id)}
		if !found || compareVersions(current.version, best.version) > 0 {
			best, found = current, true
		}
	}
	if !found {
		return ""
	}
	return best.id
}

func versionTokens(id string) []int {
	id = strings.TrimPrefix(id, "cursor-")
	parts := strings.Split(id, "-")
	values := []int{}
	for _, part := range parts {
		if !versionToken.MatchString(part) && !(parts[0] == "kimi" && kimiVersionToken.MatchString(part)) {
			continue
		}
		if strings.HasPrefix(part, "k") {
			part = strings.TrimPrefix(part, "k")
		}
		for _, value := range strings.Split(part, ".") {
			n, _ := strconv.Atoi(value)
			values = append(values, n)
		}
	}
	return values
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] > b[i] {
			return 1
		}
		if a[i] < b[i] {
			return -1
		}
	}
	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return 0
}
