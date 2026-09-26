package bridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Request logs are stored as JSON Lines: each request appends one line, and the
// file is rewritten only once trimmed lines pile up. Rewriting the whole history
// on every request would not scale to large log_retention values.
const logFile = "requests.jsonl"
const legacyLogFile = "requests.json"

// logCompactSlack is how many trimmed lines may stay on disk before a rewrite,
// keeping the amortised cost of compaction a few entries per request.
func logCompactSlack(retention int) int { return max(retention/4, 100) }

// loadLogs reads the JSON Lines log, or migrates the legacy JSON array.
// It returns the entries and the number of lines currently on disk; a negative
// line count means the file must be rewritten before appending.
func loadLogs(dataDir string) ([]LogEntry, int) {
	logs := []LogEntry{}
	f, err := os.Open(filepath.Join(dataDir, logFile))
	if errors.Is(err, os.ErrNotExist) {
		if b, e := os.ReadFile(filepath.Join(dataDir, legacyLogFile)); e == nil && json.Unmarshal(b, &logs) == nil {
			return logs, -1
		}
		return []LogEntry{}, 0
	}
	if err != nil {
		// Never schedule a rewrite that could replace a history we failed to read.
		return logs, 0
	}
	defer f.Close()
	lines, damaged := 0, false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		lines++
		var entry LogEntry
		// An interrupted append leaves a malformed line; skip it and rewrite the file.
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			logs = append(logs, entry)
		} else {
			damaged = true
		}
	}
	// Appending after an unterminated last line would merge two records.
	last := make([]byte, 1)
	if info, e := f.Stat(); e == nil && info.Size() > 0 {
		if _, e = f.ReadAt(last, info.Size()-1); e != nil || last[0] != '\n' {
			damaged = true
		}
	}
	if damaged || scanner.Err() != nil {
		lines = -1
	}
	return logs, lines
}

// persistLogLocked records the newest entry; s.mu must be held for writing.
func (s *Service) persistLogLocked(entry LogEntry) error {
	path := filepath.Join(s.cfg.DataDir, logFile)
	if s.logLines < 0 || s.logLines > len(s.logs)+logCompactSlack(s.cfg.LogRetention) {
		if err := s.rewriteLogsLocked(path); err != nil {
			s.logLines = -1
			return err
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		s.logLines = -1
		return err
	}
	line := append(jsonBytes(entry), '\n')
	_, err = f.Write(line)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// A partial line may remain; the next write rewrites the whole file.
		s.logLines = -1
		return err
	}
	s.logLines++
	return nil
}

func (s *Service) rewriteLogsLocked(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp-" + id()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriterSize(f, 1<<20)
	encoder := json.NewEncoder(w)
	for i := range s.logs {
		if err = encoder.Encode(s.logs[i]); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return err
	}
	s.logLines = len(s.logs)
	// The legacy array is superseded once the JSON Lines file is complete.
	_ = os.Remove(filepath.Join(filepath.Dir(path), legacyLogFile))
	return nil
}
