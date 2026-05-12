package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
)

const maxAuditQueryLimit = 500

type AuditHandler struct {
	querySvc *audit.QueryService
	writer   audit.Writer
}

func NewAuditHandler(querySvc *audit.QueryService, writer audit.Writer) *AuditHandler {
	return &AuditHandler{querySvc: querySvc, writer: writer}
}

func (h *AuditHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	params := audit.QueryParams{
		Action:   q.Get("action"),
		Actor:    q.Get("actor"),
		ObjectID: q.Get("objectId"),
		Result:   q.Get("result"),
		Cursor:   q.Get("cursor"),
	}

	if a := q.Get("after"); a != "" {
		t, err := time.Parse(time.RFC3339, a)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid after timestamp", err)
			return
		}
		params.After = t
	}
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid before timestamp", err)
			return
		}
		params.Before = t
	}

	if params.Actor != "" && h.writer != nil {
		if err := h.writer.Write(r.Context(), audit.Event{
			EventID:    "audit-query-" + strconv.FormatInt(time.Now().UnixNano(), 10),
			Actor:      "system",
			Action:     "query_audit_logs",
			ObjectType: "audit_log",
			ObjectID:   "system",
			Result:     "success",
			Details:    map[string]any{"queried_actor": params.Actor},
			OccurredAt: time.Now().UTC(),
		}); err != nil {
			log.Printf("AUDIT WRITE FAILURE: %v (event: %s/%s)", err, "query_audit_logs", "system")
		}
	}

	if limitStr := q.Get("limit"); limitStr != "" {
		l, err := strconv.Atoi(limitStr)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid limit", err)
			return
		}
		if l <= 0 || l > maxAuditQueryLimit {
			respondError(w, http.StatusBadRequest, "invalid limit", errors.New("audit limit out of range"))
			return
		}
		params.Limit = l
	}

	res, err := h.querySvc.Query(r.Context(), params)
	if err != nil {
		if errors.Is(err, audit.ErrInvalidCursor) {
			respondError(w, http.StatusBadRequest, "invalid cursor", err)
			return
		}
		respondError(w, http.StatusInternalServerError, "internal server error", err)
		return
	}
	respondJSON(w, http.StatusOK, res)
}
