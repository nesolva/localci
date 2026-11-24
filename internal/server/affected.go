package server

import (
	"net/http"
)

// affected answers GET /api/runs/{id}/affected with the choice an affected run made: every test with the reason,
// or the tests with the changed files that selected them.
func (s *Server) affected(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	ev, found, err := s.d.Store.FirstEvent(r.Context(), run.ID, "run.affected")
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "%v", err)
	case !found:
		fail(w, http.StatusNotFound, "run %s has no affected choice (yet): it was not asked for with --affected, or has not got past listing its tests", run.ID)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Write(ev.Data)
	}
}
