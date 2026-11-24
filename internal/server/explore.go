package server

import (
	"errors"
	"net/http"

	"github.com/nesolva/localci/internal/explore"
)

// ExploreStarted answers POST /api/explore.
type ExploreStarted struct {
	Run string `json:"run"`
}

func (s *Server) exploreRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/findings", explore.APIHandler(s.d.Store))
	mux.HandleFunc("POST /api/explore", s.startExplore)
}

// startExplore queues an exploratory run now, as the schedule would.
func (s *Server) startExplore(w http.ResponseWriter, r *http.Request) {
	if s.d.Explore == nil {
		fail(w, http.StatusConflict, "exploration is not configured: add an `explore` section (schedule, suite, base_url, goals) to the config")
		return
	}
	id, err := s.d.Explore.Start(s.d.ExploreCtx, "cli")
	var busy explore.ErrBusy
	switch {
	case errors.As(err, &busy):
		fail(w, http.StatusConflict, "%v", err)
	case err != nil:
		fail(w, http.StatusInternalServerError, "%v", err)
	default:
		writeJSON(w, http.StatusAccepted, ExploreStarted{Run: id})
	}
}
