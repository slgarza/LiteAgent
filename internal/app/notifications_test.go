package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAlertsIncludeFinishedWorkAcrossChats(t *testing.T) {
	a := newTestApp(t)
	_ = a.store.Update(func(st *State) error {
		st.Schedules = []Schedule{{ID: "scheduled", Title: "Daily report"}}
		st.Runs = []Run{
			{ID: "reminder", ChatID: "other", Origin: "reminder", Status: "completed", Result: "Meeting time"},
			{ID: "finished", ChatID: "other", Origin: "scheduled", ScheduleID: "scheduled", Status: "completed", Result: "Report saved", FinishedAt: "2026-11-15T17:03:00Z"},
			{ID: "failed", ChatID: "other", Origin: "scheduled", Status: "failed", Result: "Cannot connect"},
			{ID: "reply", ChatID: "selected", Origin: "user", Status: "completed", Result: "Hello"},
			{ID: "running", Origin: "scheduled", Status: "running"},
			{ID: "cancelled", Origin: "user", Status: "cancelled"},
			{ID: "command", Origin: "command", Status: "completed"},
		}
		return nil
	})
	w := callAPI(t, a, "GET", "/api/state?chat_id=selected", nil, nil)
	var result struct {
		Alerts  []map[string]string `json:"alerts"`
		Notices []map[string]string `json:"notices"`
		Runs    []Run               `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatal("state failed", err)
	}
	if len(result.Alerts) != 4 || len(result.Runs) != 1 || len(result.Notices) != 1 {
		t.Fatalf("alerts were filtered to the selected chat or CLI notices changed: %+v", result)
	}
	if result.Alerts[1]["title"] != "Daily report" || result.Alerts[1]["created_at"] != "2026-11-15T17:03:00Z" || result.Alerts[3]["kind"] != "reply" || !strings.HasPrefix(result.Alerts[2]["title"], "Needs attention") {
		t.Fatal("missing notification context", result.Alerts)
	}
}
