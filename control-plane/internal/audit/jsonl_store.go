package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type JournalStore struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
	file     *os.File
}

func NewJournalStore(path string) (*JournalStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create audit log dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit jsonl: %w", err)
	}

	return &JournalStore{
		path:     path,
		maxBytes: loadAuditMaxBytes(),
		file:     f,
	}, nil
}

func (s *JournalStore) Append(event Event) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		if err := s.reopenAppend(); err != nil {
			return err
		}
	}
	if err := s.rotateIfNeeded(int64(len(b))); err != nil {
		return err
	}

	if _, err := s.file.Write(b); err != nil {
		return err
	}
	return s.file.Sync()
}

func (s *JournalStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *JournalStore) ReadAll(cap int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No file yet, which is fine
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var evt Event
		if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
			events = append(events, evt)
		} else {
			log.Printf("audit journal parse failure path=%s err=%v", s.path, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// If we exceed capacity, retain the latest `cap` events.
	if len(events) > cap {
		events = events[len(events)-cap:]
	}

	return events, nil
}

func (s *JournalStore) rotateIfNeeded(nextWriteBytes int64) error {
	if s.maxBytes <= 0 {
		return nil
	}
	stat, err := s.file.Stat()
	if err != nil {
		return err
	}
	if stat.Size()+nextWriteBytes <= s.maxBytes {
		return nil
	}

	if err := s.file.Sync(); err != nil {
		return err
	}
	now := time.Now().UTC()
	rotatedPath := fmt.Sprintf("%s.%s.%d", s.path, now.Format("20060102T150405Z"), now.UnixNano())
	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	if err := os.Rename(s.path, rotatedPath); err != nil {
		reopenErr := s.reopenAppend()
		return errors.Join(err, reopenErr)
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		restoreErr := os.Rename(rotatedPath, s.path)
		reopenErr := s.reopenAppend()
		return errors.Join(fmt.Errorf("open rotated audit file: %w", err), restoreErr, reopenErr)
	}
	s.file = f
	return nil
}

func (s *JournalStore) reopenAppend() error {
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	s.file = f
	return nil
}

func loadAuditMaxBytes() int64 {
	const defaultMaxBytes = 10 * 1024 * 1024
	raw := os.Getenv("HOLO_AUDIT_MAX_BYTES")
	if raw == "" {
		return defaultMaxBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultMaxBytes
	}
	return n
}
