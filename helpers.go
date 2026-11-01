package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ConflictResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type ExplanationResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type EventReminder struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}

type MiniEvent struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Date    string `json:"date"`
}

type NavExtraction struct {
	TargetPage int `json:"target_page"`
}

type EventExtraction struct {
	Action        string          `json:"action"`
	OriginalTitle string          `json:"original_title"`
	Title         string          `json:"title"`
	StartTime     string          `json:"start_time"`
	EndTime       string          `json:"end_time"`
	Recurrence    string          `json:"recurrence"`
	IsAllDay      bool            `json:"is_all_day"`
	Description   string          `json:"description"`
	Location      string          `json:"location"`
	Reminders     []EventReminder `json:"reminders"`
	EventID       string          `json:"event_id"`
}

type CalendarState struct {
	ID    string `bson:"_id"`
	State string `bson:"state"`
}

type IntentResponse struct {
	Intent string `json:"intent"`
}

type SearchExtraction struct {
	Query string `json:"query"`
}

func getPageFromRedis(ctx context.Context, pageNum int, pageSize int) string {
	raw, err := redisClient.Get(ctx, "all_events").Result()
	if err != nil {
		return "No calendar data available in memory."
	}

	var events []MiniEvent
	json.Unmarshal([]byte(raw), &events)

	total := len(events)
	start := (pageNum - 1) * pageSize
	if start >= total || start < 0 {
		return fmt.Sprintf("Page %d is empty. Total events: %d.", pageNum, total)
	}

	end := start + pageSize
	if end > total {
		end = total
	}

	totalPages := (total + pageSize - 1) / pageSize

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- PAGE %d OF %d ---\n", pageNum, totalPages))
	for _, item := range events[start:end] {
		sb.WriteString(fmt.Sprintf("- [ID: %s] %s at %s\n", item.ID, item.Summary, item.Date))
	}
	return sb.String()
}

func validateAndFormatTime(timeStr string) (string, error) {
	t, err := time.Parse(time.RFC3339, timeStr)
	if err == nil {
		return t.Format(time.RFC3339), nil
	}

	t, err = time.ParseInLocation("2006-01-02T15:04:05", timeStr, time.Local)
	if err == nil {
		return t.Format(time.RFC3339), nil
	}

	t, err = time.ParseInLocation("2006-01-02", timeStr, time.Local)
	if err == nil {
		t = t.Add(9 * time.Hour)
		return t.Format(time.RFC3339), nil
	}

	return "", fmt.Errorf("unrecognizable date format")
}
