package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrInvalidCursor = errors.New("invalid audit cursor")

type QueryService struct {
	writer    *MemoryWriter
	cursorKey []byte
}

func NewQueryService(writer *MemoryWriter) *QueryService {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		key = []byte(time.Now().UTC().Format(time.RFC3339Nano))
	}
	return &QueryService{writer: writer, cursorKey: key}
}

type QueryParams struct {
	Action   string
	Actor    string
	ObjectID string
	Result   string
	After    time.Time
	Before   time.Time
	Limit    int
	Cursor   string
}

type QueryResult struct {
	Records    []Event `json:"records"`
	NextCursor string  `json:"nextCursor,omitempty"`
	Total      int     `json:"total"`
}

func (s *QueryService) Query(ctx context.Context, params QueryParams) (QueryResult, error) {
	allEvents := s.writer.Events()

	// Filtering
	var matched []Event
	for _, e := range allEvents {
		if params.Action != "" && e.Action != params.Action {
			continue
		}
		if params.Actor != "" && e.Actor != params.Actor {
			continue
		}
		if params.ObjectID != "" && e.ObjectID != params.ObjectID {
			continue
		}
		if params.Result != "" && e.Result != params.Result {
			continue
		}
		if !params.After.IsZero() && e.OccurredAt.Before(params.After) {
			continue
		}
		if !params.Before.IsZero() && e.OccurredAt.After(params.Before) {
			continue
		}
		matched = append(matched, e)
	}

	total := len(matched)

	filterFingerprint := cursorFilterFingerprint(params)

	// Pagination using backward iteration
	startIndex := total - 1
	if params.Cursor != "" {
		cursorEventID, err := s.decodeCursor(params.Cursor, filterFingerprint)
		if err != nil || cursorEventID == "" {
			return QueryResult{}, ErrInvalidCursor
		}
		found := false
		for idx := len(matched) - 1; idx >= 0; idx-- {
			if matched[idx].EventID == cursorEventID {
				startIndex = idx - 1
				found = true
				break
			}
		}
		if !found {
			return QueryResult{}, ErrInvalidCursor
		}
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	var results []Event
	i := startIndex
	for ; i >= 0 && len(results) < limit; i-- {
		results = append(results, matched[i])
	}

	var nextCursorStr string
	if i >= 0 && len(results) > 0 {
		lastReturned := results[len(results)-1]
		nextCursorStr = s.encodeCursor(lastReturned.EventID, filterFingerprint)
	}

	return QueryResult{
		Records:    results,
		NextCursor: nextCursorStr,
		Total:      total,
	}, nil
}

type cursorPayload struct {
	EventID string `json:"eventId"`
	Filter  string `json:"filter"`
	MAC     string `json:"mac"`
}

func (s *QueryService) encodeCursor(eventID, filter string) string {
	payload := cursorPayload{
		EventID: eventID,
		Filter:  filter,
		MAC:     s.cursorMAC(eventID, filter),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func (s *QueryService) decodeCursor(cursor, filter string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", err
	}
	var payload cursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", err
	}
	if payload.EventID == "" || payload.Filter != filter {
		return "", ErrInvalidCursor
	}
	expected := s.cursorMAC(payload.EventID, payload.Filter)
	if !hmac.Equal([]byte(payload.MAC), []byte(expected)) {
		return "", ErrInvalidCursor
	}
	return payload.EventID, nil
}

func (s *QueryService) cursorMAC(eventID, filter string) string {
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write([]byte(eventID))
	mac.Write([]byte{0})
	mac.Write([]byte(filter))
	return hex.EncodeToString(mac.Sum(nil))
}

func cursorFilterFingerprint(params QueryParams) string {
	parts := []string{
		params.Action,
		params.Actor,
		params.ObjectID,
		params.Result,
		params.After.UTC().Format(time.RFC3339Nano),
		params.Before.UTC().Format(time.RFC3339Nano),
	}
	return strings.Join(parts, "\x00")
}
