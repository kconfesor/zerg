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
	// one — the create endpoint rejects anything that is not.
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
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
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
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		// ponytail: hidden directories are dropped as noise — a git repository
		// is almost never named .something. Show them if a real repo turns up
		// dotted and someone complains.
		if strings.HasPrefix(it.Name(), ".") {
			continue
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
		"path":    path,
		"parent":  parent,
		"entries": entries,
	})
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
