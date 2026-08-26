package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A project's mark comes out of the project.
//
// The first version of this took an emoji, which is a way of asking someone to
// invent an identity for a repository that already has one: almost every
// project on disk carries a favicon, a logo or an app icon, drawn deliberately
// and already the thing its people recognise it by. Reading that file is both
// less work for the operator and a better answer than anything they would pick
// from a grid.

// iconExts are the image types worth serving, and their content types.
//
// An allowlist, not a guess: this endpoint reads files out of a directory the
// operator pointed at, so what it will hand back has to be decided here rather
// than inferred from whatever happens to be on disk.
var iconExts = map[string]string{
	".ico":  "image/x-icon",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".avif": "image/avif",
}

// skipDirs are the directories a mark is never in, and that a walk must not
// pay for. node_modules and target are the expensive ones; dist and build hold
// copies of files that also exist in source, and offering both would ask
// someone to choose between two identical images.
//
// .worktrees is zerg's own: a project's role checkouts live there, so without
// this every project would offer its own logo once per role.
var skipDirs = map[string]bool{
	".git": true, ".worktrees": true, "node_modules": true, "vendor": true,
	"target": true, "dist": true, "build": true, "out": true, ".next": true,
	".nuxt": true, ".output": true, ".svelte-kit": true, ".cache": true,
	"__pycache__": true, ".venv": true, "venv": true, "coverage": true,
	".idea": true, ".vscode": true, "tmp": true, ".terraform": true,
}

// iconWords are what an icon file tends to be called.
var iconWords = []string{"favicon", "logo", "icon", "monogram", "lockup", "mark", "brand", "avatar", "android-chrome", "apple-touch"}

// iconDirNames are directories whose contents are marks whatever the files are
// called. A file named monogram_teal.svg says what it is; one named
// lockup_black.svg in a directory called logos is equally clear, and half the
// brand directories in the world are full of files named neither.
var iconDirNames = map[string]bool{
	"logo": true, "logos": true, "icon": true, "icons": true,
	"brand": true, "branding": true, "favicon": true, "favicons": true,
}

const (
	// maxIconDepth bounds the walk. A monorepo keeps its marks in
	// frontend/<app>/public/, which is three; five leaves room without
	// descending into a fixtures tree.
	maxIconDepth = 5

	// maxIconVisits bounds the work regardless of shape. A repository that
	// defeats the skip list should cost a bounded amount of filesystem and then
	// stop, rather than holding an HTTP handler open while it enumerates.
	maxIconVisits = 20000

	// maxIconBytes bounds what will be read and served. A mark is kilobytes;
	// anything larger is a screenshot that happens to be called logo.png.
	maxIconBytes = 2 << 20

	// maxIconCandidates bounds the list offered. A repository with a hundred
	// images in public/ is not offering a hundred identities.
	maxIconCandidates = 40
)

// iconCandidate is one image the project could be marked with.
type iconCandidate struct {
	// Path is relative to the project root, and is what gets stored.
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// projectIcons lists the images in a project that look like its mark.
func (s *Server) projectIcons(w http.ResponseWriter, r *http.Request) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidates": orEmpty(findIcons(project.Path)),
	})
}

// findIcons walks the project for images that look like its mark, best guesses
// first.
//
// A walk rather than a list of well-known directories, which is what this was
// first and which found nothing in either real project it was pointed at: the
// marks were in assets/logos/ and frontend/<app>/public/, neither of which any
// reasonable fixed list contains. What keeps a walk affordable is refusing to
// enter the directories that make repositories large.
func findIcons(root string) []iconCandidate {
	var out []iconCandidate
	seen := map[string]bool{}
	visits := 0

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}

	_ = filepath.WalkDir(realRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable directory is skipped, not fatal
		}
		if visits++; visits > maxIconVisits {
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(realRoot, path)
		if relErr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(filepath.ToSlash(rel), "/") + 1
		}

		if d.IsDir() {
			if path == realRoot {
				return nil
			}
			// Hidden directories are configuration and history, not identity —
			// with .github excepted, which is where a repository puts the mark
			// it shows on its own front page.
			name := d.Name()
			if skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".github") {
				return filepath.SkipDir
			}
			if depth >= maxIconDepth {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if _, ok := iconExts[ext]; !ok {
			return nil
		}
		// Named like a mark, or sitting in a directory that only holds marks.
		stem := strings.ToLower(strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())))
		if !named(stem) && !iconDirNames[strings.ToLower(filepath.Base(filepath.Dir(path)))] {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Size() == 0 || info.Size() > maxIconBytes {
			return nil
		}

		slash := filepath.ToSlash(rel)
		if seen[slash] {
			return nil
		}
		seen[slash] = true
		out = append(out, iconCandidate{Path: slash, Bytes: info.Size()})
		return nil
	})

	// Ordered so the first one offered is the one most likely wanted: a
	// hand-drawn logo over a generated favicon, a shallow path over a deep one,
	// and a stable tie-break so the grid does not reshuffle between visits.
	sort.Slice(out, func(i, j int) bool {
		if a, b := rank(out[i].Path), rank(out[j].Path); a != b {
			return a < b
		}
		di := strings.Count(out[i].Path, "/")
		dj := strings.Count(out[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > maxIconCandidates {
		out = out[:maxIconCandidates]
	}
	return out
}

func named(stem string) bool {
	for _, w := range iconWords {
		if strings.Contains(stem, w) {
			return true
		}
	}
	return false
}

// rank orders the kinds of mark by how much deliberate design they usually
// carry. A logo was drawn; a favicon was often generated from it; an
// apple-touch or android-chrome file is a platform's cropped copy.
func rank(path string) int {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "logo"), strings.Contains(p, "monogram"),
		strings.Contains(p, "brand"), strings.Contains(p, "mark"):
		return 0
	case strings.Contains(p, "favicon"):
		return 1
	case strings.Contains(p, "icon") && !strings.Contains(p, "apple") && !strings.Contains(p, "android"):
		return 2
	default:
		return 3
	}
}

// projectIcon serves the image a project is marked with, or one it could be.
//
// The path is stored, not supplied by the caller, but it is still resolved and
// checked here: this reads a file out of a directory chosen by whoever added
// the project, over a cockpit that has no authentication (§17), so the
// containment has to be enforced at the point of reading rather than assumed
// from the point of writing.
func (s *Server) projectIcon(w http.ResponseWriter, r *http.Request) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The picker previews candidates the project has not chosen, so it may name
	// one. It goes through the same resolution as the stored path — a query
	// parameter is the least trusted input there is, and the containment check
	// is the same check either way.
	want := project.Icon
	if preview := r.URL.Query().Get("preview"); preview != "" {
		want = preview
	}
	if want == "" {
		writeError(w, http.StatusNotFound, "this project has no icon set")
		return
	}

	full, err := resolveInside(project.Path, want)
	if err != nil {
		// A mark that has been deleted or moved is not a server fault, and the
		// avatar falls back to initials on a 404 without saying anything.
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxIconBytes {
		writeError(w, http.StatusNotFound, "this project's icon is no longer readable")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "this project's icon is no longer readable")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", iconExts[strings.ToLower(filepath.Ext(full))])
	// nosniff and a null CSP together: an SVG opened as a document rather than
	// through an <img> is a document, and a document from this origin could
	// otherwise run script against a cockpit that has no authentication.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	// Revalidated rather than cached blind: a project's logo changes when
	// someone edits the file, and a stale mark in the switcher is confusing in
	// a way a stale stylesheet is not.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", fmt.Sprintf(`"%d-%d"`, info.ModTime().UnixNano(), info.Size()))
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

// resolveInside turns a stored relative path into an absolute one, and refuses
// anything that leaves the project.
//
// Symlinks are resolved before the check, not after: a link inside the
// repository pointing at ~/.ssh/id_rsa passes every string test there is, and
// only the resolved path tells the truth about what would be read.
func resolveInside(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("no icon is set")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("an icon is a path inside the project")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("an icon is a path inside the project")
	}
	if _, ok := iconExts[strings.ToLower(filepath.Ext(clean))]; !ok {
		return "", fmt.Errorf("%s is not an image this can serve", rel)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("this project's directory is not readable")
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, clean))
	if err != nil {
		return "", fmt.Errorf("%s is not in this project any more", rel)
	}
	inside, err := filepath.Rel(realRoot, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("an icon is a path inside the project")
	}
	return full, nil
}
