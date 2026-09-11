package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/chat"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// The code explorer: browse any branch, tag or commit of a project's
// repository, independent of any task, approval or feature. See
// docs/design/code-explorer.md.

// repoMaxFile bounds a single file read, the same cap the diff endpoints use
// -- see hatchery.LoadFile's callers.
const repoMaxFile = 256 * 1024

// repoRefs lists a project's branches and tags, for the ref picker.
func (s *Server) repoRefs(w http.ResponseWriter, r *http.Request) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	refs, err := hatchery.New(project.Path).Refs(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(refs))
}

// repoResolve is the one place a client-supplied ref becomes a commit sha,
// shared by repoTree and repoFile so a directory listing and the file opened
// from it answer against the same ref resolution rather than two separate
// ones a push in between could disagree on. See decision 4.
func (s *Server) repoResolve(w http.ResponseWriter, r *http.Request) (hat *hatchery.Hatchery, sha string, ok bool) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return nil, "", false
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		badRequest(w, "ref is required")
		return nil, "", false
	}
	hat = hatchery.New(project.Path)
	sha, err = hat.ResolveCommit(r.Context(), ref)
	if err != nil {
		s.repoError(w, r, err)
		return nil, "", false
	}
	return hat, sha, true
}

// repoTree lists one directory's immediate children at a ref, pinned to the
// commit sha it resolved. Not recursive -- a directory is opened because a
// person clicked it, not read whole up front.
func (s *Server) repoTree(w http.ResponseWriter, r *http.Request) {
	hat, sha, ok := s.repoResolve(w, r)
	if !ok {
		return
	}
	entries, err := hat.Tree(r.Context(), sha, r.URL.Query().Get("path"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolvedSha": sha, "entries": orEmpty(entries)})
}

// repoFile reads one file's content at a ref, pinned to the commit sha it
// resolved.
func (s *Server) repoFile(w http.ResponseWriter, r *http.Request) {
	hat, sha, ok := s.repoResolve(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		badRequest(w, "path is required")
		return
	}
	blob, err := hat.Blob(r.Context(), sha, path, repoMaxFile)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolvedSha": sha, "blob": blob})
}

// repoError is the same client/server split every other git-reading handler
// in this package draws: a ref or path the repository does not have is the
// operator's to fix, not this daemon's fault.
func (s *Server) repoError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, hatchery.ErrNoSuchRevision) {
		badRequest(w, err.Error())
		return
	}
	s.fail(w, r, err)
}

// ── explain ──────────────────────────────────────────────────────────────
//
// "Explain this" while browsing, independent of any task -- see
// docs/design/code-explorer.md decisions 8-11.

// explainRequest is a file, a folder, or a selection inside one, to explain.
//
// Path names a file or a directory; ref:path is enough for the agent to read
// either with its own tools. Selection, when present, is embedded directly
// instead -- a person reading a specific excerpt wants an answer about
// exactly what is highlighted, not a possibly-different read of a ref that
// may have moved since the page loaded. See decision 10.
type explainRequest struct {
	Ref       string `json:"ref"`
	Path      string `json:"path"`
	Selection string `json:"selection,omitempty"`
	Question  string `json:"question,omitempty"`
}

// noNarration is appended to both prompts below.
//
// AskAndWait used to return on the first turn boundary that had any message
// in it, not the one that finished the answer -- a narrating first turn like
// "I'll look at the docs directory at that commit." with no tool call yet was
// itself enough to end the wait, cutting off the real reading and explanation
// that followed. Fixed at the source (AskAndWait now waits for EventDone, the
// harness's own "nothing further is coming" signal -- see
// docs/design/code-explorer.md decision 8), so this is no longer covering for
// a known gap. Kept anyway: an answer that opens with a throwaway sentence
// before the substance is still worse to read than one that does not.
const noNarration = " Do not say what you are about to do. Read everything you need first, in as " +
	"many tool calls as it takes, and send exactly one message: the finished answer."

// explainPrompt is the whole-file or whole-folder shape: name ref:path and
// let the agent read it with its own tools, the same reasoning guidePrompt
// already uses -- a large file is wasteful to paste, and reading it fresh
// answers about what is actually there.
func explainPrompt(ref, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A person is browsing this repository at %s and wants %s explained.\n\n", ref, path)
	fmt.Fprintf(&b, "Read %s at %s in this repository (git show %s:%s for a file; list and read its "+
		"contents if it is a directory) before answering.\n", path, ref, ref, path)
	b.WriteString(`
Explain what it is and what it does, in a few sentences to a short paragraph.
If it is a directory, say what its pieces are for and how they relate rather
than listing every file. You are explaining, not reviewing: do not suggest
changes or point out problems unless asked.`)
	b.WriteString(noNarration)
	return b.String()
}

// explainSelectionPrompt embeds the selected text directly, the same
// reasoning askPrompt already uses for a diff hunk.
func explainSelectionPrompt(ref, path, selection, question string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A person is reading %s at %s in this repository and has a question about the "+
		"lines below.\n\n", path, ref)
	b.WriteString("What they selected:\n\n```\n")
	b.WriteString(strings.TrimRight(selection, "\n"))
	b.WriteString("\n```\n")
	q := strings.TrimSpace(question)
	if q == "" {
		q = "Explain what this does."
	}
	fmt.Fprintf(&b, "\nTheir question: %s\n", q)
	b.WriteString(`
Answer that question and nothing else. You are explaining, not reviewing: do
not suggest changes or point out problems unless asked. Keep it to a few
sentences.`)
	b.WriteString(noNarration)
	return b.String()
}

type explainStatus string

const (
	explainReading explainStatus = "reading"
	explainDone    explainStatus = "done"
	explainFailed  explainStatus = "error"
)

// explainJobTTL is how long a finished job's answer stays pollable. Generous,
// not permanent: a phone that locks mid-poll still finds its answer on wake.
const explainJobTTL = 10 * time.Minute

type explainJob struct {
	projectID string
	status    explainStatus
	answer    string
	err       string
	at        time.Time
}

// explainJobView is what a poll actually returns -- never projectID or at,
// which are this daemon's business, not the client's.
type explainJobView struct {
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
}

// explainJobs holds explain answers in memory only, ephemeral rather than a
// ReviewThread: that type hard-requires a task (OpenReviewThread calls
// GetTask and fails the whole open if it does not resolve), and a card
// browsed at an arbitrary ref has none. Inventing a fake task per browsing
// session, or loosening that check, would both be bigger and riskier than
// the value of persisting a browse-time answer. See decision 9.
type explainJobs struct {
	mu   sync.Mutex
	byID map[string]*explainJob
}

func newExplainJobs() *explainJobs {
	return &explainJobs{byID: map[string]*explainJob{}}
}

func (j *explainJobs) start(projectID string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	cutoff := time.Now().Add(-explainJobTTL)
	for id, job := range j.byID {
		if job.at.Before(cutoff) {
			delete(j.byID, id)
		}
	}
	id := store.NewID()
	j.byID[id] = &explainJob{projectID: projectID, status: explainReading, at: time.Now()}
	return id
}

func (j *explainJobs) finish(id, answer string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.byID[id]; ok {
		job.status, job.answer, job.at = explainDone, answer, time.Now()
	}
}

func (j *explainJobs) fail(id, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.byID[id]; ok {
		job.status, job.err, job.at = explainFailed, msg, time.Now()
	}
}

// get answers only for the project that started the job -- a mismatch, like
// an unknown id, is "not found": there is no third answer to give either way.
func (j *explainJobs) get(projectID, id string) (explainJobView, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok || job.projectID != projectID {
		return explainJobView{}, false
	}
	return explainJobView{Status: string(job.status), Answer: job.answer, Error: job.err}, true
}

// repoExplain starts a background answer and returns immediately with a job
// id to poll -- an agent turn is tens of seconds, and a request held open for
// that is a request that dies on a phone, the same reasoning
// askAboutTheChange already uses.
func (s *Server) repoExplain(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		writeError(w, http.StatusNotImplemented, "this build has no agent to ask")
		return
	}
	var req explainRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Ref == "" || req.Path == "" {
		badRequest(w, "ref and path are required")
		return
	}
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Being second in line is not a fault; say so rather than half-starting.
	if s.chatMgr.Busy(chat.ReviewChat(project.ID)) {
		writeError(w, http.StatusConflict,
			"the agent is in the middle of an answer; ask again when it finishes")
		return
	}

	prompt := explainPrompt(req.Ref, req.Path)
	if strings.TrimSpace(req.Selection) != "" {
		prompt = explainSelectionPrompt(req.Ref, req.Path, req.Selection, req.Question)
	}

	id := s.explain.start(project.ID)
	go s.answerExplain(id, project.ID, prompt)
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": id})
}

func (s *Server) answerExplain(jobID, projectID, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	answer, err := s.chatMgr.AskAndWait(ctx, projectID, prompt)
	if err != nil {
		s.explain.fail(jobID, err.Error())
		return
	}
	s.explain.finish(jobID, answer)
}

// repoExplainStatus polls one explain job.
func (s *Server) repoExplainStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.explain.get(r.PathValue("id"), r.PathValue("jobId"))
	if !ok {
		writeError(w, http.StatusNotFound,
			"that explain job is gone -- it does not survive a daemon restart, ask again")
		return
	}
	writeJSON(w, http.StatusOK, job)
}
