package review

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTestBinaryDefaultPrefixRunsDirectly(t *testing.T) {
	previous := supervisorPrefix
	supervisorPrefix = append([]string(nil), defaultSupervisorPrefix...)
	t.Cleanup(func() { supervisorPrefix = previous })
	marker := filepath.Join(t.TempDir(), "direct.marker")
	env := append(os.Environ(), "PURSER_REVIEW_DIRECT_TEST=1", "PURSER_REVIEW_DIRECT_MARKER="+marker)
	var output bytes.Buffer
	code, err := runSupervised(context.Background(), []string{os.Args[0], "-test.run=^TestReviewDirectLaunchHelper$"}, "", env, nil, &output, nil)
	if err != nil || code != 0 {
		t.Fatalf("direct launch code=%d err=%v output=%q", code, err, output.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("direct helper did not run: %v", err)
	}
}

func TestReviewDirectLaunchHelper(t *testing.T) {
	if os.Getenv("PURSER_REVIEW_DIRECT_TEST") != "1" {
		return
	}
	if marker := os.Getenv("PURSER_REVIEW_DIRECT_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("direct"), 0600); err != nil {
			os.Exit(2)
		}
	}
	for _, arg := range os.Args[1:] {
		if arg == "supervisor" {
			os.Exit(3)
		}
	}
}

func TestTestBinarySuffixes(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{"review.test", true},
		{"review.test.exe", true},
		{"review.exe", false},
		{"review.testing", false},
	} {
		if got := isTestBinary(test.path); got != test.want {
			t.Errorf("isTestBinary(%q)=%v, want %v", test.path, got, test.want)
		}
	}
}
