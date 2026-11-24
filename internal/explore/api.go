package explore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// List answers GET /api/findings.
type List struct {
	Findings []store.Finding `json:"findings"`
	Total    int             `json:"total"`
	// Cursor is the newest finding's id: `--since <cursor>` lists what comes after it.
	Cursor int64 `json:"cursor"`
}

const defaultLimit, maxLimit = 10, 500

// APIHandler serves GET /api/findings?since=<id|30m>&run=<id>&limit=<n>, newest first. since is a finding id (only
// later findings) or a duration (only findings found within it).
func APIHandler(st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var after, notBefore int64
		if v := q.Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				after = n
			} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
				notBefore = time.Now().Add(-d).UnixMilli()
			} else {
				fail(w, http.StatusBadRequest, "since is a finding number or a duration like 12h, got %q", v)
				return
			}
		}
		limit := defaultLimit
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				fail(w, http.StatusBadRequest, "limit must be a positive number, got %q", v)
				return
			}
			limit = min(n, maxLimit)
		}
		cursor := st.LastFindingID(r.Context())
		all, err := st.Findings(r.Context(), after, notBefore, q.Get("run"))
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		out := List{Findings: all[:min(limit, len(all))], Total: len(all), Cursor: cursor}
		writeJSON(w, http.StatusOK, out)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string, args ...any) {
	b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf(msg, args...)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}
