package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the agent is actually sent when something is attached.
//
// The path rather than the contents: an agent has a filesystem and the tools to
// read it, so one shape works for a screenshot, a log and a spreadsheet, and a
// large file does not have to survive being pasted into a prompt to be read.
func TestAnAttachmentReachesTheAgentAsAPath(t *testing.T) {
	got := prompt(Message{
		Text:  "what is wrong with this layout?",
		Files: []Attachment{{Name: "screenshot.png", Path: "attachments/screenshot.png"}},
	})
	if !strings.Contains(got, "what is wrong with this layout?") {
		t.Errorf("the question did not survive: %q", got)
	}
	if !strings.Contains(got, "attachments/screenshot.png") {
		t.Errorf("the agent was not told where the file is: %q", got)
	}

	// Several are listed one per line, so a path is never split across a line
	// break by whatever renders it.
	many := prompt(Message{
		Text: "compare these",
		Files: []Attachment{
			{Name: "before.png", Path: "attachments/before.png"},
			{Name: "after.png", Path: "attachments/after.png"},
		},
	})
	for _, want := range []string{"attachments/before.png", "attachments/after.png"} {
		if !strings.Contains(many, want) {
			t.Errorf("%s is not in the prompt: %q", want, many)
		}
	}

	// A file with nothing said about it is still a question worth sending.
	alone := prompt(Message{Files: []Attachment{{Name: "log.txt", Path: "attachments/log.txt"}}})
	if !strings.Contains(alone, "attachments/log.txt") {
		t.Errorf("an attachment on its own said nothing: %q", alone)
	}
	if strings.HasPrefix(alone, "\n") {
		t.Errorf("an empty message left the prompt starting with a blank line: %q", alone)
	}

	// And a message with no files is exactly what was typed, with no
	// preamble invented around it.
	if got := prompt(Message{Text: "why is the evaluator recursive?"}); got != "why is the evaluator recursive?" {
		t.Errorf("a plain question was rewritten: %q", got)
	}
}

// The agent reads the file from its own worktree, not from the daemon's store.
//
// Its tools are pointed at the worktree, and a path outside it is both awkward
// to explain and a route out of the only directory this agent should touch.
func TestAttachmentsAreCopiedIntoTheWorktree(t *testing.T) {
	store := t.TempDir()
	worktree := t.TempDir()

	source := filepath.Join(store, "abc123")
	if err := os.WriteFile(source, []byte("the bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{}
	files := []Attachment{{Name: "diagram.png", Source: source}}
	if err := m.materialise(worktree, files); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	if files[0].Path != filepath.Join(attachDir, "diagram.png") {
		t.Errorf("path = %q, want it relative to the worktree", files[0].Path)
	}
	landed := filepath.Join(worktree, files[0].Path)
	got, err := os.ReadFile(landed)
	if err != nil {
		t.Fatalf("the agent cannot read it: %v", err)
	}
	if string(got) != "the bytes" {
		t.Errorf("copied %q, want the file's contents", got)
	}

	// The same name twice in one conversation is ordinary -- screenshot.png,
	// then screenshot.png again -- and the newer one is the one being talked
	// about.
	if err := os.WriteFile(source, []byte("the newer bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.materialise(worktree, []Attachment{{Name: "diagram.png", Source: source}}); err != nil {
		t.Fatalf("materialise again: %v", err)
	}
	again, err := os.ReadFile(landed)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != "the newer bytes" {
		t.Errorf("the second copy did not replace the first: %q", again)
	}
}
