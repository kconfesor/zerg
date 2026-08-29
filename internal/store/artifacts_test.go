package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Artifacts age with the transcripts they belong to, and the bytes go with
// them -- except that two rows can name one file, so what is safe to delete is
// a question about the whole table rather than about the row being dropped.
func TestPruningKeepsBytesTwoArtifactsShare(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	old, err := db.CreateTask(ctx, p.ID, "Old", "", "")
	if err != nil {
		t.Fatal(err)
	}
	recent, err := db.CreateTask(ctx, p.ID, "Recent", "", "")
	if err != nil {
		t.Fatal(err)
	}

	shared := strings.Repeat("a", 64)
	lonely := strings.Repeat("b", 64)
	for _, a := range []*Artifact{
		{ProjectID: p.ID, TaskID: &old.ID, Kind: ArtifactImage, SHA256: shared, Name: "shot.png"},
		{ProjectID: p.ID, TaskID: &old.ID, Kind: ArtifactFile, SHA256: lonely, Name: "report.txt"},
		{ProjectID: p.ID, TaskID: &recent.ID, Kind: ArtifactImage, SHA256: shared, Name: "same.png"},
	} {
		if _, err := db.AddArtifact(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// Age the first card's artifacts past the window.
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE artifacts SET created_at = ? WHERE task_id = ?`,
		time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}

	dropped, orphans, err := db.PruneArtifacts(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneArtifacts: %v", err)
	}
	if dropped != 2 {
		t.Errorf("dropped %d rows, want the two that aged out", dropped)
	}
	// The lonely digest is unreferenced now; the shared one is still named by
	// the recent card and its bytes must stay.
	if len(orphans) != 1 || orphans[0] != lonely {
		t.Errorf("orphans = %v, want only the digest nothing else names", orphans)
	}

	left, err := db.ArtifactsForTask(ctx, recent.ID)
	if err != nil || len(left) != 1 {
		t.Errorf("the recent card kept %d artifacts (%v)", len(left), err)
	}
}

// Pinning is what keeps the one worth keeping, and a pinned card keeps its
// artifacts too: the task worth reading in six months is usually the one that
// went wrong, and its screenshot is most of why.
func TestPinningExemptsAnArtifactAndSoDoesAPinnedTask(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	kept, err := db.CreateTask(ctx, p.ID, "Kept", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := db.CreateTask(ctx, p.ID, "Ordinary", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskPinned(ctx, kept.ID, true); err != nil {
		t.Fatal(err)
	}

	onPinnedTask, err := db.AddArtifact(ctx, &Artifact{
		ProjectID: p.ID, TaskID: &kept.ID, Kind: ArtifactFile, SHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	pinnedItself, err := db.AddArtifact(ctx, &Artifact{
		ProjectID: p.ID, TaskID: &ordinary.ID, Kind: ArtifactFile, SHA256: strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArtifactPinned(ctx, pinnedItself.ID, true); err != nil {
		t.Fatal(err)
	}
	doomed, err := db.AddArtifact(ctx, &Artifact{
		ProjectID: p.ID, TaskID: &ordinary.ID, Kind: ArtifactFile, SHA256: strings.Repeat("e", 64)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.sql.ExecContext(ctx, `UPDATE artifacts SET created_at = ?`,
		time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	dropped, _, err := db.PruneArtifacts(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped %d, want only the one nothing exempts", dropped)
	}
	for _, id := range []string{onPinnedTask.ID, pinnedItself.ID} {
		if _, err := db.GetArtifact(ctx, id); err != nil {
			t.Errorf("an exempt artifact was pruned: %v", err)
		}
	}
	if _, err := db.GetArtifact(ctx, doomed.ID); err == nil {
		t.Error("the ordinary artifact survived its window")
	}
}
