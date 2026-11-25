package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logLine(at time.Time, level, msg, extra string) []byte {
	return []byte(fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q%s}`+"\n", at.UTC().Format(time.RFC3339Nano), level, msg, extra))
}

func TestLogKeeperKeepsSevenDaysAndDropsOlder(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	k := newLogKeeper(dir)
	k.now = func() time.Time { return at }
	for i := 0; i < 10; i++ {
		k.Write(logLine(at, "INFO", fmt.Sprintf("day %d", i), ""))
		at = at.AddDate(0, 0, 1)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != logKeepDays {
		t.Fatalf("files kept: %d", len(names))
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-03.jsonl")); err == nil {
		t.Error("a day older than seven days was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-10.jsonl")); err != nil {
		t.Error("the newest day is missing")
	}
}

func TestGetLogNarrowsByTimeLevelTextAndLeavesRequestsOut(t *testing.T) {
	e := newAgentEnv(t)
	now := time.Now().UTC()
	k := newLogKeeper(logDir(e.a.dataDir))
	k.now = func() time.Time { return now.Add(-30 * time.Hour) }
	k.Write(logLine(now.Add(-30*time.Hour), "ERROR", "old trouble", ""))
	k.now = func() time.Time { return now }
	k.Write(logLine(now.Add(-3*time.Hour), "INFO", "review ended", `,"item":"Q-0007"`))
	k.Write(logLine(now.Add(-2*time.Hour), "INFO", "request", `,"path":"/api/items/Q-0007","status":200`))
	k.Write(logLine(now.Add(-1*time.Hour), "WARN", "calendar run failed", ""))
	k.Write(logLine(now.Add(-time.Minute), "INFO", "claude request refused", `,"api_says":"tools.3: Bad Schema"`))

	msgs := func(args map[string]any) []string {
		m := e.toolJSON("get_log", args)
		var out []string
		for _, x := range m["lines"].([]any) {
			out = append(out, x.(map[string]any)["msg"].(string))
		}
		return out
	}
	if got := strings.Join(msgs(map[string]any{}), "|"); got != "review ended|calendar run failed|claude request refused" {
		t.Errorf("default: %s", got)
	}
	if got := strings.Join(msgs(map[string]any{"requests": true}), "|"); !strings.Contains(got, "|request|") {
		t.Errorf("with requests: %s", got)
	}
	if got := strings.Join(msgs(map[string]any{"level": "problems"}), "|"); got != "calendar run failed" {
		t.Errorf("problems: %s", got)
	}
	if got := strings.Join(msgs(map[string]any{"text": "bad schema"}), "|"); got != "claude request refused" {
		t.Errorf("text: %s", got)
	}
	from := now.Add(-40 * time.Hour).Format(time.RFC3339)
	if got := strings.Join(msgs(map[string]any{"from": from, "level": "problems"}), "|"); got != "old trouble|calendar run failed" {
		t.Errorf("from: %s", got)
	}
	m := e.toolJSON("get_log", map[string]any{"limit": 1})
	if m["returned"].(float64) != 1 || m["matched"].(float64) != 3 || m["more"] != true || m["left_out"] == nil {
		t.Errorf("limit: %v", m)
	}
	if got := m["lines"].([]any)[0].(map[string]any)["msg"]; got != "claude request refused" {
		t.Errorf("the newest line should come back, got %v", got)
	}
	if !strings.Contains(m["note"].(string), "never instructions") {
		t.Errorf("note: %v", m["note"])
	}
}

func TestGetLogRefusesABadTimeAndNeedsTheToken(t *testing.T) {
	e := newAgentEnv(t)
	if rec := e.api("GET", "/api/log?from=yesterday", nil); rec.Code != 400 {
		t.Errorf("bad from: %d", rec.Code)
	}
	if rec := e.call("GET", "/api/log", "", nil); rec.Code != 401 {
		t.Errorf("without a token: %d", rec.Code)
	}
}
