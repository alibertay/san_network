// Package api is the Go port of the SAN Network FastAPI application:
// app/main.py, app/routes.py and app/limits.py. It exposes every REST route
// with the same paths, status codes and JSON response shapes, plus the
// Prometheus text exposition from app/metrics.py.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/alibertay/san_network/internal/canonical"
)

// writeJSON renders a Starlette-style JSON response (compact, no HTML
// escaping) with the given status code. Values go through the canonical
// encoder so floats keep Python's repr (json.dumps renders 3.0 as "3.0",
// encoding/json as "3"); clients re-verify fetched transaction signatures
// and block hashes, so this exactly matters.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, err := canonical.Marshal(value)
	if err != nil {
		fallback, _ := json.Marshal(value)
		data = fallback
	}
	_, _ = w.Write(data)
}

// writeError is FastAPI's HTTPException body: {"detail": "..."}.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"detail": detail})
}

// validationDetail matches one entry of FastAPI/Pydantic's 422 detail list.
type validationDetail struct {
	Type  string         `json:"type"`
	Loc   []any          `json:"loc"`
	Msg   string         `json:"msg"`
	Input any            `json:"input,omitempty"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

func writeValidationError(w http.ResponseWriter, detail validationDetail) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": []any{detail}})
}

// requestValidator collects Pydantic-style validation failures for one
// request so multi-parameter routes answer with a single 422 like FastAPI.
type requestValidator struct {
	errors []validationDetail
}

func (v *requestValidator) add(detail validationDetail) {
	v.errors = append(v.errors, detail)
}

// intQuery parses an integer query parameter (FastAPI Query semantics).
func (v *requestValidator) intQuery(r *http.Request, name string, fallback int64, min, max *int64) int64 {
	values, present := r.URL.Query()[name]
	if !present || len(values) == 0 {
		return fallback
	}
	raw := values[0]
	parsed, ok := v.parseInt(raw, []any{"query", name}, min, max)
	if !ok {
		return fallback
	}
	return parsed
}

// intPath parses an integer path parameter (FastAPI Path semantics).
func (v *requestValidator) intPath(raw, name string) (int64, bool) {
	return v.parseInt(raw, []any{"path", name}, nil, nil)
}

func (v *requestValidator) parseInt(raw string, loc []any, min, max *int64) (int64, bool) {
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		v.add(validationDetail{
			Type:  "int_parsing",
			Loc:   loc,
			Msg:   "Input should be a valid integer, unable to parse string as an integer",
			Input: raw,
		})
		return 0, false
	}
	if min != nil && parsed < *min {
		v.add(validationDetail{
			Type:  "greater_than_equal",
			Loc:   loc,
			Msg:   fmt.Sprintf("Input should be greater than or equal to %d", *min),
			Input: raw,
			Ctx:   map[string]any{"ge": *min},
		})
		return 0, false
	}
	if max != nil && parsed > *max {
		v.add(validationDetail{
			Type:  "less_than_equal",
			Loc:   loc,
			Msg:   fmt.Sprintf("Input should be less than or equal to %d", *max),
			Input: raw,
			Ctx:   map[string]any{"le": *max},
		})
		return 0, false
	}
	return parsed, true
}

// flush writes the collected 422 response; it returns false when any error
// was reported (the caller must stop handling).
func (v *requestValidator) flush(w http.ResponseWriter) bool {
	if len(v.errors) == 0 {
		return true
	}
	details := make([]any, len(v.errors))
	for i, detail := range v.errors {
		details[i] = detail
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": details})
	return false
}

func int64Ptr(value int64) *int64 { return &value }

// readObjectBody reads a JSON body and returns it as an object, replicating
// FastAPI's Body(...) validation errors (missing body, invalid JSON,
// non-object payload). The body is decoded with the canonical decoder so
// integers keep their Python semantics for signing and hashing.
func readObjectBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeValidationError(w, validationDetail{
			Type:  "json_invalid",
			Loc:   []any{"body", 0},
			Msg:   "JSON decode error",
			Input: map[string]any{},
			Ctx:   map[string]any{"error": err.Error()},
		})
		return nil, false
	}
	if len(raw) == 0 {
		writeValidationError(w, validationDetail{
			Type: "missing",
			Loc:  []any{"body"},
			Msg:  "Field required",
		})
		return nil, false
	}
	decoded, err := canonical.Decode(raw)
	if err != nil {
		writeValidationError(w, validationDetail{
			Type:  "json_invalid",
			Loc:   []any{"body", 0},
			Msg:   "JSON decode error",
			Input: map[string]any{},
			Ctx:   map[string]any{"error": "Expecting value"},
		})
		return nil, false
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		writeValidationError(w, validationDetail{
			Type:  "dict_type",
			Loc:   []any{"body"},
			Msg:   "Input should be a valid dictionary",
			Input: decoded,
		})
		return nil, false
	}
	return object, true
}
