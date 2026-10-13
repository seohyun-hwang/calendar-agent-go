package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms/ollama"
	"github.com/tmc/langchaingo/tools"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

type CalendarCreateTool struct {
	Service    *calendar.Service
	CalendarID string
}

func (t CalendarCreateTool) Name() string {
	return "google_calendar_create_event"
}

// Explicit formatting example helps Llama 3 hit the required pattern on turn 1
func (t CalendarCreateTool) Description() string {
	return `Creates an event on Google Calendar. 
Input MUST be a comma-separated string with exactly 3 fields: Title, StartTime (RFC3339), EndTime (RFC3339).
Example: Sync Meeting, 2026-10-14T10:00:00-04:00, 2026-10-14T11:00:00-04:00`
}

func (t CalendarCreateTool) Call(ctx context.Context, input string) (string, error) {
	// Strip quotes or backticks Llama might wrap around input
	cleanInput := strings.Trim(strings.TrimSpace(input), "`\"'")
	parts := strings.Split(cleanInput, ",")
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid input format. Expected 3 comma-separated values")
	}

	event := &calendar.Event{
		Summary: strings.TrimSpace(parts[0]),
		Start: &calendar.EventDateTime{
			DateTime: strings.TrimSpace(parts[1]),
		},
		End: &calendar.EventDateTime{
			DateTime: strings.TrimSpace(parts[2]),
		},
	}

	_, err := t.Service.Events.Insert(t.CalendarID, event).Do()
	if err != nil {
		return fmt.Sprintf("Failed to create event: %v", err), nil
	}

	return "Successfully scheduled the event on Google Calendar.", nil
}

func main() {
	ctx := context.Background()

	llm, err := ollama.New(ollama.WithModel("llama3"))
	if err != nil {
		log.Fatalf("Error initializing LLM: %v\n", err)
	}

	calendarService, err := calendar.NewService(ctx, option.WithCredentialsFile("credentials.json"))
	if err != nil {
		log.Fatalf("Unable to retrieve Calendar client: %v", err)
	}

	calendarTool := CalendarCreateTool{
		Service:    calendarService,
		CalendarID: "name@gmail.com", // Replace with personal calendar email
	}

	agentTools := []tools.Tool{calendarTool}

	executor, err := agents.Initialize(
		llm,
		agentTools,
		agents.ZeroShotReactDescription,
		agents.WithMaxIterations(5),
	)
	if err != nil {
		log.Fatalf("Error initializing agent: %v", err)
	}

	// Calculate concrete dates in Go to avoid date calculation hallucinations
	now := time.Now()
	tomorrow := now.AddDate(0, 0, 1)

	prompt := fmt.Sprintf(
		"Today is %s. Tomorrow date is %s. Local timezone offset is %s. "+
			"Schedule a sync meeting with the engineering team for tomorrow from 10:00 AM to 11:00 AM.",
		now.Format("Monday, Jan 2, 2006"),
		tomorrow.Format("2006-01-02"),
		now.Format("-07:00"),
	)

	response, err := chains.Call(ctx, executor, map[string]any{
		"input": prompt,
	})
	if err != nil {
		log.Fatalf("Agent execution failed: %v", err)
	}

	fmt.Println("Agent Response:", response["output"])
}

