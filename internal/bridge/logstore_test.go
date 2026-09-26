package bridge

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func logFileLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, logFile))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

func TestLogRetentionAcceptsLargeLimits(t *testing.T) {
	cfg := defaultConfig()
	for _, n := range []int{minLogRetention, 10001, maxLogRetention} {
		cfg.LogRetention = n
		if err := cfg.validate(); err != nil {
			t.Fatalf("log_retention %d rejected: %v", n, err)
		}
	}
	for _, n := range []int{minLogRetention - 1, maxLogRetention + 1} {
		cfg.LogRetention = n
		if err := cfg.validate(); err == nil {
			t.Fatalf("log_retention %d accepted", n)
		}
	}
	s := registeredService(t, "")
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/config", Body: []byte(`{"log_retention":99999999}`)}))
	if err != nil || result.(ManagementResponse).StatusCode != 200 || s.config().LogRetention != maxLogRetention {
		t.Fatalf("saving the maximum retention failed: %v %s", err, result.(ManagementResponse).Body)
	}
}

func TestLogsAppendAsJSONLinesAndCompact(t *testing.T) {
	dir := t.TempDir()
	s := stickyService(t, dir, fmt.Sprintf("log_retention: %d\n", minLogRetention))
	slack := logCompactSlack(minLogRetention)
	for i := 0; i < minLogRetention+slack; i++ {
		s.appendLog(LogEntry{ID: fmt.Sprint(i)})
	}
	if got := logFileLines(t, dir); got != minLogRetention+slack || len(s.logs) != minLogRetention {
		t.Fatalf("appends should not rewrite the file: %d lines, %d logs", got, len(s.logs))
	}
	s.appendLog(LogEntry{ID: "last"})
	s.appendLog(LogEntry{ID: "after"})
	if got := logFileLines(t, dir); got > minLogRetention+1 {
		t.Fatalf("trimmed lines were not compacted: %d lines", got)
	}
	// A crash mid-append leaves a partial line; loading skips it.
	f, err := os.OpenFile(filepath.Join(dir, logFile), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"partial`)
	_ = f.Close()
	reloaded := stickyService(t, dir, fmt.Sprintf("log_retention: %d\n", minLogRetention))
	if len(reloaded.logs) != minLogRetention || reloaded.logs[len(reloaded.logs)-1].ID != "after" || reloaded.logs[len(reloaded.logs)-2].ID != "last" {
		t.Fatalf("reloaded logs are wrong: %d entries, last %#v", len(reloaded.logs), reloaded.logs[len(reloaded.logs)-1])
	}
	reloaded.appendLog(LogEntry{ID: "repaired"})
	again := stickyService(t, dir, fmt.Sprintf("log_retention: %d\n", minLogRetention))
	if again.logs[len(again.logs)-1].ID != "repaired" {
		t.Fatalf("append after a partial line was lost: %#v", again.logs[len(again.logs)-1])
	}
}

func TestLegacyLogArrayMigratesToJSONLines(t *testing.T) {
	dir := t.TempDir()
	if err := atomicJSON(filepath.Join(dir, legacyLogFile), []LogEntry{{ID: "a"}, {ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	s := stickyService(t, dir, "")
	if len(s.logs) != 2 || s.logs[1].ID != "b" {
		t.Fatalf("legacy logs not loaded: %#v", s.logs)
	}
	s.appendLog(LogEntry{ID: "c"})
	if got := logFileLines(t, dir); got != 3 {
		t.Fatalf("migration should write every entry, got %d lines", got)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyLogFile)); !os.IsNotExist(err) {
		t.Fatalf("legacy log file should be removed after migration: %v", err)
	}
	if reloaded := stickyService(t, dir, ""); len(reloaded.logs) != 3 || reloaded.logs[0].ID != "a" {
		t.Fatalf("migrated logs not reloaded: %#v", reloaded.logs)
	}
}
