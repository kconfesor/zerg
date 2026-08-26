package nydus

import (
	"context"
	"strings"
	"testing"
)

// PR mode on a repository with no remote must say so, and say what to do
// instead. The failure otherwise surfaces from git as "No configured push
// destination", several steps from the setting that caused it.
func TestPublishWithoutARemoteExplainsItself(t *testing.T) {
	repo := newRepo(t)
	_, err := Git{}.Publish(context.Background(), repo, "main", "HEAD", "Calculator", "approved", false)
	if err == nil {
		t.Fatal("publishing succeeded with no remote configured")
	}
	for _, want := range []string{"no remote", "merge or branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestPRCreateArgsAddsDraftOnlyWhenRequested(t *testing.T) {
	plain := prCreateArgs("main", "zerg/task", "Task", "body", false)
	draft := prCreateArgs("main", "zerg/task", "Task", "body", true)
	if strings.Contains(strings.Join(plain, " "), "--draft") {
		t.Fatalf("ordinary PR args contain --draft: %v", plain)
	}
	if got := strings.Count(strings.Join(draft, " "), "--draft"); got != 1 {
		t.Fatalf("draft args contain --draft %d times: %v", got, draft)
	}
}
