package util_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/util"
)

func lastLoggedIndex(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	index, _ := record["index"].(float64)
	return int(index)
}

func TestLoggerRotatesAndKeepsConfiguredCopies(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "literouter.log")
	logger := util.NewLogger(model.LogInfo, file, 400, 2)
	for index := 0; index < 40; index++ {
		logger.Info("line", map[string]any{"index": index})
	}
	if err := logger.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "literouter.log,literouter.log.1,literouter.log.2" {
		t.Fatalf("files: %v", names)
	}
	current, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if len(current) > 400 || len(strings.TrimSpace(string(current))) == 0 {
		t.Fatalf("current size: %d", len(current))
	}
	newest := lastLoggedIndex(t, file+".1")
	oldest := lastLoggedIndex(t, file+".2")
	if newest <= oldest {
		t.Fatalf("rotation order: newest=%d oldest=%d", newest, oldest)
	}
	if lastLoggedIndex(t, file) <= newest {
		t.Fatal("current log should hold the newest line")
	}
}

func TestLoggerRotationCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "literouter.log")
	logger := util.NewLogger(model.LogInfo, file, 0, 2)
	for index := 0; index < 50; index++ {
		logger.Info("line", map[string]any{"index": index})
	}
	_ = logger.Flush()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "literouter.log" {
		t.Fatalf("files: %v", entries)
	}
	info, _ := os.Stat(file)
	if info.Size() <= 400 {
		t.Fatalf("expected a large file, got %d", info.Size())
	}
}

func TestLoggerResumesExistingFileSize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "literouter.log")
	first := util.NewLogger(model.LogInfo, file, 300, 1)
	for index := 0; index < 5; index++ {
		first.Info("line", map[string]any{"index": index})
	}
	_ = first.Flush()

	second := util.NewLogger(model.LogInfo, file, 300, 1)
	for index := 5; index < 20; index++ {
		second.Info("line", map[string]any{"index": index})
	}
	_ = second.Flush()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "literouter.log,literouter.log.1" {
		t.Fatalf("files: %v", names)
	}
	info, _ := os.Stat(file)
	if info.Size() > 300 {
		t.Fatalf("current file too large: %d", info.Size())
	}
}

func TestLoggerLevelAndAccessLines(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "literouter.log")
	logger := util.NewLogger(model.LogInfo, file, 0, 0)
	logger.Debug("debug line", map[string]any{"index": 1})
	logger.Info("info line", map[string]any{"index": 2})
	logger.Warn("warn line", map[string]any{"index": 3})
	logger.Access("GET", "/v1/models", 200, 4)
	_ = logger.Flush()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "debug line") {
		t.Fatalf("debug should be filtered: %s", text)
	}
	if !strings.Contains(text, "info line") || !strings.Contains(text, "warn line") || !strings.Contains(text, "api request") {
		t.Fatalf("expected info, warn and access lines: %s", text)
	}
}
