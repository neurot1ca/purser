package review

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestNavigationIndexResolvesMultipleHunksAndFiles(t *testing.T) {
	parts := []string{
		"diff --git a/a.go b/a.go\r\n--- a/a.go\r\n+++ b/a.go\r\n",
		"@@ -1 +1 @@\r\n-old\r\n+Türkçe\r\n",
		"@@ -10,2 +10,2 @@ context\r\n-same\r\n+new\r\n context\r\n",
		"diff --git a/b.go b/b.go\nnew file mode 100644\n--- /dev/null\n+++ b/b.go\n",
		"@@ -0,0 +1 @@\n+last",
	}
	diff := strings.Join(parts, "")
	index, ok := NavigationIndex(diff)
	if !ok || !strings.Contains(index, fmt.Sprintf("sha256=%x bytes=%d", sha256.Sum256([]byte(diff)), len(diff))) {
		t.Fatalf("unbound index: %s", index)
	}
	rows := strings.Split(strings.TrimSpace(index), "\n")[1:]
	if len(rows) != len(parts) {
		t.Fatalf("ranges: %s", index)
	}
	offset := 0
	for i, row := range rows {
		var start, end int
		if _, err := fmt.Sscanf(row[strings.LastIndex(row, "bytes="):], "bytes=%d:%d", &start, &end); err != nil {
			t.Fatal(err)
		}
		if start != offset || end != offset+len(parts[i]) || diff[start:end] != parts[i] {
			t.Fatalf("range %d is not exact: %s", i, row)
		}
		offset = end
	}
	refs := FindingReferences("job:revision", 2)
	if refs[0] == refs[1] || refs[0] != FindingReferences("job:revision", 2)[0] || refs[0] == FindingReferences("other:revision", 2)[0] {
		t.Fatalf("unstable or colliding refs: %v", refs)
	}
}

func TestNavigationIndexRejectsMalformedHunk(t *testing.T) {
	if _, ok := NavigationIndex("diff --git a/a b/a\n@@\n+x\n"); ok {
		t.Fatal("bare hunk header indexed")
	}
}
