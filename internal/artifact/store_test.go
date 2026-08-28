package artifact

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The same output from two tasks is one file on disk. That is the whole reason
// for addressing by content, and it is what keeps a screenshot taken on every
// run of a pipeline from being stored on every run of a pipeline.
func TestIdenticalBytesAreStoredOnce(t *testing.T) {
	work := t.TempDir()
	s := New(t.TempDir())

	one, mimeOne, size, err := s.Put(write(t, work, "report.html", "<h1>same</h1>"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	two, _, _, err := s.Put(write(t, work, "copy-of-report.html", "<h1>same</h1>"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if one != two {
		t.Errorf("the same bytes stored under %s and %s", one, two)
	}
	if size != 13 {
		t.Errorf("size = %d, want 13", size)
	}
	if !strings.HasPrefix(mimeOne, "text/html") {
		t.Errorf("mime = %q, want text/html", mimeOne)
	}

	// One file, whichever name it arrived under.
	f, err := s.Open(one)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if string(body) != "<h1>same</h1>" {
		t.Errorf("stored bytes = %q", body)
	}
}

// The extension carries intent the content cannot: an .svg and an .html both
// sniff as text, and only one of them should be shown as a picture.
func TestTypeComesFromTheNameAndFallsBackToTheBytes(t *testing.T) {
	work := t.TempDir()
	s := New(t.TempDir())

	cases := []struct{ name, body, want string }{
		{"shot.png", "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 40), "image/png"},
		{"chart.svg", "<svg xmlns='http://www.w3.org/2000/svg'></svg>", "image/svg+xml"},
		{"notes.md", "# hello", "text/markdown"},
		// No extension at all: sniffed.
		{"logfile", "plain text and more of it", "text/plain"},
	}
	for _, tc := range cases {
		_, got, _, err := s.Put(write(t, work, tc.name, tc.body))
		if err != nil {
			t.Fatalf("Put %s: %v", tc.name, err)
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s came back as %q, want %s", tc.name, got, tc.want)
		}
	}
}

// An agent redirecting a log into `artifact add` must not fill the disk the
// database lives on. The first symptom of that would be SQLite failing to
// write a transcript, which says nothing about what caused it.
func TestAFileOverTheLimitIsRefused(t *testing.T) {
	work := t.TempDir()
	s := New(t.TempDir())

	big := filepath.Join(work, "huge.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxBytes + 1); err != nil {
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	f.Close()

	if _, _, _, err := s.Put(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Put of an oversized file returned %v, want ErrTooLarge", err)
	}
	// And nothing was left behind half-written.
	entries, _ := filepath.Glob(filepath.Join(s.Dir(), ".incoming-*"))
	if len(entries) != 0 {
		t.Errorf("%d temporary files left behind", len(entries))
	}
}

// A digest reaches Path from the database, and Path joins it into a filename.
// Nothing that is not a digest may take part in that arithmetic.
func TestOnlyADigestCanBeOpened(t *testing.T) {
	s := New(t.TempDir())
	for _, bad := range []string{"../../etc/passwd", "", "zz", strings.Repeat("g", 64)} {
		if _, err := s.Open(bad); err == nil {
			t.Errorf("Open(%q) was allowed", bad)
		}
		if err := s.Remove(bad); err == nil {
			t.Errorf("Remove(%q) was allowed", bad)
		}
	}
}

func TestRemovingWhatIsNotThereIsNotAnError(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Remove(strings.Repeat("a", 64)); err != nil {
		t.Errorf("removing a blob that does not exist: %v", err)
	}
}

// A directory is not a file, and the message should say so rather than
// failing later with something about a read.
func TestADirectoryIsRefused(t *testing.T) {
	s := New(t.TempDir())
	if _, _, _, err := s.Put(t.TempDir()); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("Put of a directory returned %v", err)
	}
}
