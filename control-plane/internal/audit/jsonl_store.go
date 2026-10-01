package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type JournalStore struct {
	path        string
	maxBytes    int64
	maxArchives int
	parseErrors int64
	mu          sync.Mutex
	file        *os.File
}

func NewJournalStore(path string) (*JournalStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create audit log dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit jsonl: %w", err)
	}

	store := &JournalStore{
		path:        path,
		maxBytes:    loadAuditMaxBytes(),
		maxArchives: loadAuditMaxArchives(),
		file:        f,
	}
	if err := store.pruneArchives(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("prune audit archives: %w", err)
	}
	return store, nil
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
	line := 0
	for scanner.Scan() {
		line++
		var evt Event
		if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
			events = append(events, evt)
		} else {
			atomic.AddInt64(&s.parseErrors, 1)
			log.Printf("audit journal parse failure path=%s line=%d err=%v", s.path, line, err)
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

func (s *JournalStore) ParseErrors() int64 {
	if s == nil {
		return 0
	}
	return atomic.LoadInt64(&s.parseErrors)
}

func (s *JournalStore) SizeBytes() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		if stat, err := s.file.Stat(); err == nil {
			return stat.Size()
		}
	}
	stat, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return stat.Size()
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
	if err := s.pruneArchives(); err != nil {
		return fmt.Errorf("prune audit archives: %w", err)
	}
	return nil
}

func (s *JournalStore) pruneArchives() error {
	if s.maxArchives <= 0 {
		return nil
	}

	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	prefix := filepath.Base(s.path) + "."
	type archive struct {
		path      string
		createdAt int64
	}
	archives := make([]archive, 0)
	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		createdAt, ok := parseAuditArchiveTime(strings.TrimPrefix(entry.Name(), prefix))
		if !ok {
			continue
		}
		archives = append(archives, archive{
			path:      filepath.Join(filepath.Dir(s.path), entry.Name()),
			createdAt: createdAt,
		})
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].createdAt < archives[j].createdAt
	})
	for len(archives) > s.maxArchives {
		if err := os.Remove(archives[0].path); err != nil {
			return err
		}
		archives = archives[1:]
	}
	return nil
}

func parseAuditArchiveTime(name string) (int64, bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return 0, false
	}
	if _, err := time.Parse("20060102T150405Z", name[:dot]); err != nil {
		return 0, false
	}
	createdAt, err := strconv.ParseInt(name[dot+1:], 10, 64)
	return createdAt, err == nil && createdAt > 0
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

func loadAuditMaxArchives() int {
	const defaultMaxArchives = 10
	raw := os.Getenv("HOLO_AUDIT_MAX_ARCHIVES")
	if raw == "" {
		return defaultMaxArchives
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMaxArchives
	}
	return n
}
