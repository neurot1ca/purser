package retention

import (
	"sync"
	"testing"
)

func TestWorktreeOwnershipRejectsAttachmentAfterRetirement(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	if ok, err := AttachWorktree(root, worktree, "job-a", "thread-a"); err != nil || !ok {
		t.Fatalf("initial attachment = %v, %v", ok, err)
	}
	owned, err := MarkWorktreeRetiring(root, worktree)
	if err != nil || !owned.Retiring {
		t.Fatalf("retirement = %#v, %v", owned, err)
	}
	if ok, err := AttachWorktree(root, worktree, "job-b", "thread-a"); err != nil || ok {
		t.Fatalf("attachment after retirement = %v, %v; want rejected", ok, err)
	}
	if len(owned.Jobs) != 1 || owned.Jobs[0] != "job-a" {
		t.Fatalf("retirement owners = %#v, want only job-a", owned.Jobs)
	}
}

func TestWorktreeOwnershipSerializesConcurrentAttachmentAndRetirement(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var attached bool
	var attachErr error
	var retired WorktreeOwnership
	var retireErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		attached, attachErr = AttachWorktree(root, worktree, "job-race", "thread-race")
	}()
	go func() {
		defer wg.Done()
		<-start
		retired, retireErr = MarkWorktreeRetiring(root, worktree)
	}()
	close(start)
	wg.Wait()
	if attachErr != nil || retireErr != nil || !retired.Retiring {
		t.Fatalf("attach err=%v retirement=%#v err=%v", attachErr, retired, retireErr)
	}
	if attached && (len(retired.Jobs) != 1 || retired.Jobs[0] != "job-race") {
		t.Fatalf("retirement omitted winning concurrent attachment: %#v", retired)
	}
	if ok, err := AttachWorktree(root, worktree, "job-after", "thread-race"); err != nil || ok {
		t.Fatalf("post-retirement attachment = %v, %v; want rejected", ok, err)
	}
}
