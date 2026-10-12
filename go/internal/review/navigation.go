package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var navigationHunk = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+[0-9]+(?:,[0-9]+)? @@`)

// NavigationIndex uses half-open byte ranges into the unmodified raw diff.
func NavigationIndex(diff string) (string, bool) {
	sum := sha256.Sum256([]byte(diff))
	digest := hex.EncodeToString(sum[:])
	var index strings.Builder
	fmt.Fprintf(&index, "sha256=%s bytes=%d\n", digest, len(diff))
	start, offset, ordinal := -1, 0, 0
	label := ""
	emit := func(end int) {
		if start >= 0 {
			ordinal++
			fmt.Fprintf(&index, "R%s-%04d %s bytes=%d:%d\n", digest[:16], ordinal, label, start, end)
		}
	}
	file := ""
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			emit(offset)
			file = strings.TrimSpace(strings.TrimPrefix(line, "diff --git "))
			label, start = file, offset
		} else if strings.HasPrefix(line, "@@") {
			header := navigationHunk.FindString(line)
			if header == "" || file == "" {
				return "", false
			}
			emit(offset)
			label, start = file+" "+header, offset
		}
		offset += len(line)
	}
	emit(offset)
	return index.String(), ordinal > 0
}

// FindingReferences binds ordinals to a host-owned review revision.
func FindingReferences(revision string, count int) []string {
	sum := sha256.Sum256([]byte(revision))
	refs := make([]string, count)
	for i := range refs {
		refs[i] = fmt.Sprintf("F%x-%04d", sum[:8], i+1)
	}
	return refs
}
