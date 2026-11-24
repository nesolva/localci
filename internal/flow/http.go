package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }
func notFound(msg string) error   { return &httpError{http.StatusNotFound, msg} }

func decode(w http.ResponseWriter, req *http.Request, v any) bool {
	if err := json.NewDecoder(req.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "the body is not the JSON this hook takes: %v", err)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		fail(w, he.status, "%s", he.msg)
	case err != nil:
		fail(w, http.StatusInternalServerError, "%v", err)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}
