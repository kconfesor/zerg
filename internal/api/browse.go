package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Browsing the daemon's filesystem, so adding a project is picking a folder
// rather than typing its path.
//
// The path field is the only way in otherwise, and the browser cannot help
// fill it: the cockpit is served from the daemon and read over the tailnet, so
// the machine with the repositories is the daemon's, not the one running the
// browser. A native directory picker would list the viewer's disk and hand
// back no usable server path. The daemon is the side that can see the folders,
// so it is the side that lists them.
//
// This reads directories and returns their names. It exposes nothing the model
// in §17 does not already grant: the port has no authentication, and starting
// a task on a repository the operator chose is already arbitrary code
// execution on this machine, so which directories exist is not the secret.

// browseEntry is one subdirectory the operator could descend into or pick.
type browseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// IsRepo is true when the directory holds a .git, so the picker can show
	// which folders are actually a repository and which are only the way to
	// one. The create endpoint rejects anything that is not.
	IsRepo bool `json:"isRepo"`
}

// browse lists the subdirectories of one directory, for the Add-a-project
// picker. Empty path means the operator's home, which is where their source
// almost always lives and a better start than the filesystem root.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// No home to start from is rare enough to be worth naming rather
			// than falling back to "/", which drops the operator at the root.
			writeError(w, http.StatusInternalServerError, "could not find your home directory")
			return
		}
		path = home
	}
	// "~" and "~/..." only. `~bob` names another user's home, which this does
	// not resolve, and treating it as a prefix produced "/Users/mebob/src": a
	// path nobody typed, reported back as missing.
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	path = filepath.Clean(path)

	// A relative path has no meaning to a daemon whose working directory is not
	// the operator's: name it rather than resolve it against a directory they
	// cannot see. The picker only ever sends absolute paths.
	if !filepath.IsAbs(path) {
		badRequest(w, "a folder path must be absolute: "+path)
		return
	}

	items, err := os.ReadDir(path)
	if err != nil {
		// Wrong path, no such folder, not readable: all things the operator can
		// fix, so a 400 that names the folder rather than a 500.
		badRequest(w, "could not read "+path+": "+cleanFSError(err))
		return
	}

	entries := []browseEntry{}
	truncated := false
	for _, it := range items {
		// The client is gone, or the daemon is shutting down. Every entry costs
		// a stat, so a directory big enough to matter is a directory worth
		// stopping in the middle of.
		if err := r.Context().Err(); err != nil {
			return
		}
		// Hidden directories are dropped as noise: a git repository is almost
		// never named .something. Show them if a real one turns up dotted and
		// someone complains.
		if strings.HasPrefix(it.Name(), ".") {
			continue
		}
		if !isDir(path, it) {
			continue
		}
		if len(entries) >= maxEntries {
			// Said out loud rather than silently cut. A picker that shows the
			// first two thousand of six thousand folders, with no sign it did,
			// is a picker that has hidden the one being looked for.
			truncated = true
			break
		}
		full := filepath.Join(path, it.Name())
		entries = append(entries, browseEntry{
			Name:   it.Name(),
			Path:   full,
			IsRepo: isRepo(full),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	// Root is its own parent; report no way up rather than a link back to
	// itself, so the picker can hide the Up row there.
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":      path,
		"parent":    parent,
		"entries":   entries,
		"truncated": truncated,
	})
}

// maxEntries bounds one listing.
//
// A stat per entry is cheap and not free: twenty thousand subdirectories
// measured twelve milliseconds to read and sixty to stat, and produced about
// two megabytes of JSON. Nobody picks a folder out of twenty thousand by
// scrolling, so the cost buys nothing. On a network mount the same loop is
// bounded by the filesystem rather than by this number, which is why the
// context is checked as well.
const maxEntries = 2000

// isDir reports whether an entry is a directory, following symlinks.
//
// ReadDir describes the link rather than its target, so a plain IsDir() check
// says false for `~/src -> /Volumes/Work/src` and the folder simply does not
// appear in the picker, with nothing on screen to say why. That layout is
// common enough that the extra stat is worth paying: it only runs for the
// entries that are links.
func isDir(parent string, it os.DirEntry) bool {
	if it.IsDir() {
		return true
	}
	if it.Type()&os.ModeSymlink == 0 {
		return false
	}
	// Stat follows the link. A broken one, or a link to a file, is not a
	// directory and is left out.
	info, err := os.Stat(filepath.Join(parent, it.Name()))
	return err == nil && info.IsDir()
}

// isRepo reports whether dir holds a .git. A worktree's .git is a file rather
// than a directory, so this stats the name and does not care which it is.
func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// cleanFSError strips the leading "open <path>:" or "readdirent <path>:" the os
// package prepends, since the message already names the path once.
func cleanFSError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}
