package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const maxJSONBodyBytes = 1 << 20

func decodeRequiredJSONBody(r *http.Request, out any) error {
	if r == nil || r.Body == nil {
		return domain.ErrInvalidInput
	}
	return decodeJSONBodyWithPolicy(r.Body, out, true)
}

func decodeOptionalJSONBody(r *http.Request, out any) error {
	if r == nil || r.Body == nil {
		return nil
	}
	// Optional-body semantics are intentional for subresources that allow empty payload as
	// "no explicit override provided" (for example query-string actor + empty JSON body).
	return decodeJSONBodyWithPolicy(r.Body, out, false)
}

func decodeJSONBodyWithPolicy(body io.ReadCloser, out any, required bool) error {
	defer body.Close()

	payload, err := io.ReadAll(io.LimitReader(body, maxJSONBodyBytes+1))
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		if required {
			return domain.ErrInvalidInput
		}
		return nil
	}
	if len(payload) > maxJSONBodyBytes {
		return domain.ErrInvalidInput
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		// Keep EOF-as-success for optional bodies so callers can send an empty body intentionally.
		if errors.Is(err, io.EOF) && !required {
			return nil
		}
		return domain.ErrInvalidInput
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return domain.ErrInvalidInput
	}
	return nil
}

func respondError(w http.ResponseWriter, status int, publicMsg string, internalErr error) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if publicMsg == "" {
		publicMsg = "internal server error"
	}
	if internalErr != nil {
		log.Printf("api response error status=%d message=%q err=%v", status, publicMsg, internalErr)
	}
	http.Error(w, publicMsg, status)
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		respondError(w, http.StatusInternalServerError, "internal server error", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("api response write failure status=%d err=%v", status, err)
	}
}
