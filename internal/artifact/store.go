// Package artifact keeps the bytes an agent produced.
//
// Content-addressed: a file is stored under its own sha256, so the same output
// produced by three tasks costs one copy, and a worktree pruned tomorrow does
// not take yesterday's screenshot with it. The database row names the digest;
// this package owns the file.
//
// Deliberately not in the database. A four megabyte screenshot in a SQLite row
// is read into memory to be served, competes with the write lock the whole
// daemon shares, and makes every backup of the transcripts carry the images
// too. On disk it is an ordinary file that http.ServeContent can range-request
// and the operating system can cache.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MaxBytes is the largest file this will take.
//
// A bound rather than trust: an agent that redirects a log into `artifact add`
// would otherwise fill the disk the daemon and its database live on, and the
// first symptom would be SQLite failing to write a transcript. Large enough
// for a build output or a video of a UI, small enough that a mistake is
// survivable.
const MaxBytes = 256 << 20 // 256 MiB

// ErrTooLarge is returned when a file is over MaxBytes. It is the operator's
// problem to fix, so the API renders it as a 400 rather than a fault.
var ErrTooLarge = errors.New("file is too large to store as an artifact")

// Store is a directory of blobs named by their digest.
type Store struct{ dir string }

func New(dir string) *Store { return &Store{dir: dir} }

// Dir is where the blobs live, for a daemon that wants to report it.
func (s *Store) Dir() string { return s.dir }

// Put copies a file in and returns its digest, type and size.
//
// Written to a temporary name and renamed into place, so a crash halfway
// through leaves no half file under a digest that promises the whole one. The
// rename is atomic within a directory, and two agents storing identical bytes
// race harmlessly: they write the same content and the last rename wins.
func (s *Store) Put(src string) (digest, mimeType string, size int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", "", 0, err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return "", "", 0, err
	}
	if info.IsDir() {
		return "", "", 0, fmt.Errorf("%s is a directory; artifacts are files", src)
	}
	if info.Size() > MaxBytes {
		return "", "", 0, fmt.Errorf("%w: %s is %d bytes, the limit is %d",
			ErrTooLarge, filepath.Base(src), info.Size(), MaxBytes)
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(s.dir, ".incoming-*")
	if err != nil {
		return "", "", 0, err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened

	// Hashed while copying rather than in a second pass: one read of the file,
	// and no window in which the bytes could change between the two.
	sum := sha256.New()
	// LimitReader bounds a file that grew after the stat, which a log being
	// written to does.
	written, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(in, MaxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", 0, err
	}
	if written > MaxBytes {
		return "", "", 0, fmt.Errorf("%w: %s exceeded %d bytes while being read",
			ErrTooLarge, filepath.Base(src), MaxBytes)
	}

	digest = hex.EncodeToString(sum.Sum(nil))
	if err := s.mkdirFor(digest); err != nil {
		return "", "", 0, err
	}
	if err := os.Rename(tmp.Name(), s.Path(digest)); err != nil {
		return "", "", 0, err
	}
	return digest, typeOf(src, s.Path(digest)), written, nil
}

// Path is where a digest's bytes live.
//
// Two levels of fan-out on the first two characters: one directory with tens
// of thousands of entries is slow to list and slow on some filesystems to open
// from, and the split costs nothing.
func (s *Store) Path(digest string) string {
	if len(digest) < 4 {
		return filepath.Join(s.dir, digest)
	}
	return filepath.Join(s.dir, digest[:2], digest[2:])
}

// Open reads a stored blob.
func (s *Store) Open(digest string) (*os.File, error) {
	if !validDigest(digest) {
		return nil, fmt.Errorf("not a digest: %q", digest)
	}
	return os.Open(s.Path(digest))
}

// Remove deletes a blob. Missing is not an error: the point of calling this is
// that the bytes should be gone.
func (s *Store) Remove(digest string) error {
	if !validDigest(digest) {
		return fmt.Errorf("not a digest: %q", digest)
	}
	err := os.Remove(s.Path(digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// validDigest guards the path arithmetic above. A digest arrives from the
// database rather than from a request, but Path joins it into a filename and
// "../.." must never be a thing that reaches os.Open.
func validDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for _, c := range d {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// known is what this project actually produces, spelled out.
//
// mime.TypeByExtension consults the machine's own database -- /etc/mime.types
// and friends -- so the same file becomes text/markdown on one developer's
// laptop and text/plain in CI, and the cockpit renders it differently for no
// reason either of them can see. The types below are fixed here so the answer
// does not depend on what is installed; anything else still falls through to
// the system table and then to sniffing.
var known = map[string]string{
	".md": "text/markdown; charset=utf-8", ".markdown": "text/markdown; charset=utf-8",
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
	".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8",
	".js": "text/javascript; charset=utf-8", ".json": "application/json",
	".svg": "image/svg+xml", ".png": "image/png", ".gif": "image/gif",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".pdf": "application/pdf", ".mp4": "video/mp4", ".webm": "video/webm",
}

// typeOf decides what a file is: by extension first, and by sniffing its first
// bytes when the extension says nothing.
//
// The extension wins because it carries intent that content cannot: a .svg and
// an .html both sniff as text, and only one of them should be rendered as a
// picture.
func typeOf(originalName, stored string) string {
	ext := strings.ToLower(filepath.Ext(originalName))
	if t, ok := known[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	f, err := os.Open(stored)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return http.DetectContentType(head[:n])
}

// mkdirFor makes the fan-out directory a digest needs, since Rename will not
// create it and the digest is only known once the bytes have been read.
func (s *Store) mkdirFor(digest string) error {
	return os.MkdirAll(filepath.Dir(s.Path(digest)), 0o700)
}
