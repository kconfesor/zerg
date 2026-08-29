package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/store"
)

// Conversations, one per tab.
//
// Chat was one thread per project, so a second subject was either an
// interruption of the first or a reason to delete it. A conversation is a thing
// now: it has a name, a transcript, its own attachments and its own worktree,
// and closing it takes all four.

// listChats returns a project's conversations, most recently used first.
func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.db.ListChats(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, chats)
}

// newChat opens one.
func (s *Server) newChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	// A body is optional: opening a tab is one click, and naming it is
	// something people do afterwards if at all.
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}
	made, err := s.db.CreateChat(r.Context(), r.PathValue("id"), req.Title)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, made)
}

// renameChat sets what the tab says.
func (s *Server) renameChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		badRequest(w, "a conversation needs a name; leave it alone to keep the one it took from your first message")
		return
	}
	if err := s.db.RenameChat(r.Context(), r.PathValue("id"), r.PathValue("chat"), req.Title); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// endChat closes one and removes everything that was only ever part of it.
func (s *Server) endChat(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "chat is not available")
		return
	}
	if err := s.chatMgr.End(r.Context(), r.PathValue("id"), r.PathValue("chat")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatFor resolves the conversation a request names, and refuses one belonging
// to another project.
//
// A conversation id arrives from a browser, and reading or writing somebody
// else's thread by naming its id is not a thing this should make possible.
func (s *Server) chatFor(r *http.Request) (*store.Chat, error) {
	id := r.PathValue("chat")
	if id == "" {
		return nil, errors.New("which conversation?")
	}
	return s.db.GetChat(r.Context(), r.PathValue("id"), id)
}

// interruptChat stops the answer being written, keeping the conversation.
func (s *Server) interruptChat(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "chat is not available")
		return
	}
	c, err := s.chatFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.chatMgr.Interrupt(c.ID); err != nil {
		// A harness with no way to stop mid-turn is a fact about the harness,
		// and the operator can act on it: they chose the model behind this
		// chat and can choose another.
		if errors.Is(err, adapter.ErrNoInterrupt) {
			badRequest(w, "this chat's harness cannot be stopped mid-answer; it will finish on its own")
			return
		}
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
