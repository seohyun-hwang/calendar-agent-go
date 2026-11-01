package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/ollama"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/api/calendar/v3"
)

func handleSearch(ctx context.Context, llm *ollama.LLM, userPrompt string) {
	systemPrompt := `You are a search query extractor.
Extract the core keyword, title, or phrase the user is looking for in their calendar.
Respond ONLY with valid JSON in this exact format: {"query": "keyword"}`

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		llms.TextParts(llms.ChatMessageTypeHuman, userPrompt),
	}

	llmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	resp, err := llm.GenerateContent(llmCtx, messages, llms.WithJSONMode())
	cancel()

	if err != nil {
		fmt.Println("[Error] Failed to extract search query.")
		return
	}

	var extraction SearchExtraction
	if err := json.Unmarshal([]byte(resp.Choices[0].Content), &extraction); err != nil {
		fmt.Println("[Error] Failed to parse search JSON.")
		return
	}

	raw, err := redisClient.Get(ctx, "all_events").Result()
	if err != nil {
		fmt.Println("[Error] No calendar data available in memory.")
		return
	}

	var allEvents []MiniEvent
	json.Unmarshal([]byte(raw), &allEvents)

	query := strings.ToLower(extraction.Query)
	groupedEvents := make(map[string][]MiniEvent)

	for _, ev := range allEvents {
		if strings.Contains(strings.ToLower(ev.Summary), query) {
			groupedEvents[ev.Summary] = append(groupedEvents[ev.Summary], ev)
		}
	}

	if len(groupedEvents) == 0 {
		fmt.Printf("\n[Agent] No events found matching '%s'.\n", extraction.Query)
		return
	}

	fmt.Printf("\n--- SEARCH RESULTS FOR '%s' ---\n", extraction.Query)
	for _, events := range groupedEvents {
		if len(events) == 1 {
			// Single standalone event
			ev := events[0]
			fmt.Printf("- [ID: %s] %s at %s\n", ev.ID, ev.Summary, ev.Date)
		} else {
			firstEv := events[0]
			fmt.Printf("- [ID: %s] %s at %s (+ %d more upcoming instances)\n", firstEv.ID, firstEv.Summary, firstEv.Date, len(events)-1)
		}
	}
}

func handleNavigation(ctx context.Context, llm *ollama.LLM, userPrompt string) {
	currentPageStr, _ := redisClient.Get(ctx, "current_page").Result()

	systemPrompt := fmt.Sprintf(`You are a pagination calculator.
The user is currently viewing Page %s of their calendar.
Based on the user's request, calculate the exact integer of the page they want to view.
- If they say "next", add 1 to the current page.
- If they say "previous", subtract 1.
- If they specify a number (e.g. "page 4"), output that number.
- If they say "first", output 1.

Respond ONLY with valid JSON in this exact format: {"target_page": integer}`, currentPageStr)

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		llms.TextParts(llms.ChatMessageTypeHuman, userPrompt),
	}

	llmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	resp, err := llm.GenerateContent(llmCtx, messages, llms.WithJSONMode())
	cancel()

	if err != nil {
		fmt.Println("[Error] Failed to calculate navigation page.")
		return
	}

	var result NavExtraction
	if err := json.Unmarshal([]byte(resp.Choices[0].Content), &result); err != nil {
		fmt.Println("[Error] Failed to parse navigation JSON.")
		return
	}

	// Update Redis and print the requested page
	redisClient.Set(ctx, "current_page", result.TargetPage, 0)
	fmt.Println("\n" + getPageFromRedis(ctx, result.TargetPage, 10))
}

func handleModification(ctx context.Context, llm *ollama.LLM, scanner *bufio.Scanner, initialPrompt string, srv *calendar.Service, calendarID string) {
	fmt.Println("\n[Agent] Analyzing request and checking for conflicts...")

	currentPageStr, _ := redisClient.Get(ctx, "current_page").Result()
	var pageNum int
	fmt.Sscanf(currentPageStr, "%d", &pageNum)
	currentState := getPageFromRedis(ctx, pageNum, 10)
	conversationHistory := fmt.Sprintf("User's initial request: %s\n", initialPrompt)

	now := time.Now()
	localTZ := now.Location().String()

	timeContext := fmt.Sprintf("Today is %s. The user's timezone is %s. All times must be evaluated in this timezone.", now.Format("2006-01-02"), localTZ)

	for {
		systemPrompt := fmt.Sprintf(`You are a calendar clarification engine. 
			Today is %s. The user's timezone is %s. All times must be evaluated in this timezone.
			Current Calendar State: %s
			Conversation so far: %s

			Evaluate the user's request:
			1. CONFLICTS: ONLY flag a time conflict if two events overlap on the EXACT SAME DATE and TIME.
			2. MISSING INFO: Set status to "missing_info" and provide a "message" asking for clarification if ANY of the following are true:
			- The user asks to schedule a NEW event but DOES NOT specify a time of day (UNLESS they specify it is an "all day" event).
			- The user asks to schedule a NEW event but DOES NOT specify a time of day (e.g., "tomorrow" without a time).
			- The user asks to schedule a NEW event but DOES NOT specify a duration, and it cannot be reasonably inferred.
			*NOTE: If the user does not specify a duration, ASSUME 1 HOUR. Do NOT set missing_info for a missing duration.*
			3. CLEAR: If there is no time conflict and no missing info, set status to "clear" and summarize the final action.

			Respond ONLY with valid JSON in this exact format: {"status": "conflict|missing_info|clear", "message": "..."}`,
			timeContext,
			localTZ,
			currentState,
			conversationHistory)

		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		}

		// 30-second LLM response timeout
		llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

		fmt.Print("[Agent] ")
		var isMessage bool

		// LangchainGo streaming interceptor
		streamFn := llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			text := string(chunk)
			if strings.Contains(text, `"message"`) {
				isMessage = true
				return nil
			}

			if isMessage {
				clean := strings.ReplaceAll(text, `"`, "")
				clean = strings.ReplaceAll(clean, `}`, "")
				clean = strings.ReplaceAll(clean, `:`, "")
				clean = strings.ReplaceAll(clean, "\n", "")

				if clean != "" {
					fmt.Print(clean)
				}
			}
			return nil
		})

		resp, err := llm.GenerateContent(llmCtx, messages, llms.WithJSONMode(), streamFn)
		cancel()
		if err != nil {
			log.Printf("\n[Error] LLM failed to evaluate conflict: %v", err)
			return
		}

		var result ConflictResponse
		err = json.Unmarshal([]byte(resp.Choices[0].Content), &result)
		if err != nil {
			log.Printf("[Error] Failed to parse LLM JSON: %v", err)
			return
		}

		if result.Status == "conflict" || result.Status == "missing_info" {
			fmt.Print("\n>> ")
			scanner.Scan()
			userReply := scanner.Text()

			lowerReply := strings.ToLower(strings.TrimSpace(userReply))
			if lowerReply == "stop" || lowerReply == "exit" || lowerReply == "close now" {
				fmt.Println("[System] Aborting current task...")
				return
			}

			conversationHistory += fmt.Sprintf("Agent: %s\nUser: %s\n", result.Message, userReply)
			continue
		} else {
			fmt.Println("[System] Extracting final event parameters for Google Calendar...")

			var calendarRef strings.Builder
			calendarRef.WriteString("Date Reference Sheet (Next 14 Days):\n")
			for i := 0; i < 14; i++ {
				targetDate := now.AddDate(0, 0, i)
				prefix := ""
				if i == 0 {
					prefix = "Today, "
				} else if i == 1 {
					prefix = "Tomorrow, "
				}
				calendarRef.WriteString(fmt.Sprintf("- %s%s: %s\n", prefix, targetDate.Format("Monday"), targetDate.Format("2006-01-02")))
			}

			calendarRef.WriteString("\nFuture Month Reference (Base YYYY-MM):\n")
			for i := 0; i <= 11; i++ {
				// time.Date handles year rollovers automatically (e.g., Dec + 1 month = Jan of next year)
				t := time.Date(now.Year(), now.Month()+time.Month(i), 1, 0, 0, 0, 0, now.Location())
				calendarRef.WriteString(fmt.Sprintf("- %s %d: %s\n", t.Format("January"), t.Year(), t.Format("2006-01")))
			}

			extractPrompt := fmt.Sprintf(`You are an event extraction engine.
Based on this conversation, extract the finalized event details to be executed on the calendar.
Today is %s. The local timezone offset is %s.

Conversation:
%s

CRITICAL RULES:
- Use the 'Date Reference Sheet' to translate natural language (like "tomorrow" or "next Tuesday") into the exact YYYY-MM-DD date.
- For dates beyond the 14-day sheet, use the 'Future Month Reference' to find the correct YYYY-MM prefix, then append the specific day (e.g., if the user asks for "December 15", combine the "December" base prefix with "15" to get "YYYY-12-15").
- Start and End times MUST be formatted precisely as: YYYY-MM-DDTHH:MM:SS-07:00 (matching the provided timezone offset).
- TIMEZONES & TRAVEL: By default, format Start and End times using the user's current timezone offset (%s). HOWEVER, if the user explicitly states an event or flight starts/ends in a different timezone (e.g., "lands in PST", "London time"), adjust the UTC offset for that specific 'start_time' or 'end_time' accordingly (e.g., PST is -08:00, GMT is +00:00).
- DEFAULT DURATION: If the user does not specify an end time or duration, calculate the 'end_time' to be exactly 1 hour after the 'start_time'.
- RECURRENCE: If the user asks for the event to repeat, output an RRULE string (e.g., "RRULE:FREQ=DAILY"). If it does not repeat, leave the field as "".
- ALL-DAY EVENTS: If the user requests an all-day event, set 'is_all_day' to true. Format 'start_time' and 'end_time' strictly as YYYY-MM-DD (omit the time). 
  - *Crucial API Rule*: Google Calendar requires the 'end_time' date to be the day AFTER the event ends (e.g., a 1-day all-day event on 2026-11-05 has start_time "2026-11-05" and end_time "2026-11-06").
- DESCRIPTION: If the user provides extra context, notes, agendas, or URLs (like a Zoom or Google Meet link), put them in the 'description' field. Otherwise, leave it as "".
- LOCATION: If the user specifies a physical place, city, address, or venue, put it in the 'location' field. Otherwise, leave it as "".
- REMINDERS: If the user asks for an alert or reminder, add it to the 'reminders' array specifying the 'method' ("popup" or "email") and 'minutes' before the event. Otherwise, leave it as [].

Identify the 'action': "insert" (new event), "update" (moving/renaming), or "delete" (canceling).
If updating or deleting, provide the 'original_title' of the event being targeted.
CRITICAL: If updating or deleting, you MUST extract the exact ID from the 'Current Calendar State' (e.g., the string inside [ID: <the_id>]) and put it in the 'event_id' field. 
- If deleting multiple events, put ALL of their IDs in the 'event_id' field separated by commas. 
- NEVER output literal periods, ellipses ("..."), or placeholders. You MUST insert the actual alphanumeric IDs.

If the user mentions inviting attendees and provides their emails, disregard.

CRITICAL RULE FOR DELETIONS AND DUPLICATES:
If the user asks to delete "duplicate" event(s), look at the Current Calendar State. If you see multiple events with the exact same Title and Start Time, they will have different EventIDs. 
You must output the "delete" action for MOST of those EventIDs but KEEP ONE REMAINING (e.g., preserve the last one you see). DO NOT output a delete action for all EventIDs unless the user explicitly says to delete "all" of them. If the user explicitly says to delete "one" of them or refers to singular noun or article (e.g. "a" and/or "duplicate"), you must output the "delete" action for ONLY ONE of those EventIDs (e.g., pick the first one you see).

Respond ONLY with valid JSON in this exact format:
{"action": "insert|update|delete", "event_id": "", "original_title": "<ORIGINAL_TITLE>", "title": "Event Name", "start_time": "YYYY-MM-DDTHH:MM:SS-07:00", "end_time": "YYYY-MM-DDTHH:MM:SS-07:00", "recurrence": "", "is_all_day": true, "description": "", "location": "", "reminders": [{"method": "popup", "minutes": 30}]}`,
				now.Format("2006-01-02"), now.Format("-07:00"), conversationHistory, now.Format("-07:00"))

			extractMsgs := []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, extractPrompt),
			}

			// 30-second LLM response timeout for the final extraction
			llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

			extractResp, err := llm.GenerateContent(llmCtx, extractMsgs, llms.WithJSONMode())
			cancel()

			if err != nil {
				log.Printf("LLM failed to extract event details: %v", err)
				fmt.Println("\n[Agent] I ran into an issue compiling the calendar event. Let's try again.")
				return
			}

			rawJSON := extractResp.Choices[0].Content
			log.Printf("Generated API Payload:\n%s", rawJSON)

			var eventDetails EventExtraction
			err = json.Unmarshal([]byte(rawJSON), &eventDetails)
			if err != nil {
				log.Printf("[Error] Failed to parse extraction JSON: %v", err)
				return
			}

			if eventDetails.Action == "insert" || eventDetails.Action == "update" {
				if !eventDetails.IsAllDay {
					validStart, err := validateAndFormatTime(eventDetails.StartTime)
					if err != nil {
						log.Printf("Invalid start time '%s' provided by LLM: %v", eventDetails.StartTime, err)
						fmt.Println("[Agent] I had trouble understanding that exact time. Could you rephrase it?")
						return
					}
					eventDetails.StartTime = validStart

					validEnd, err := validateAndFormatTime(eventDetails.EndTime)
					if err != nil {
						fmt.Printf("[Error] LLM provided invalid end time '%s'. Aborting.\n", eventDetails.EndTime)
						return
					}
					eventDetails.EndTime = validEnd
				}
			}

			var startDateTime, endDateTime *calendar.EventDateTime
			if eventDetails.IsAllDay {
				startDateTime = &calendar.EventDateTime{Date: eventDetails.StartTime}
				endDateTime = &calendar.EventDateTime{Date: eventDetails.EndTime}
			} else {
				startDateTime = &calendar.EventDateTime{DateTime: eventDetails.StartTime}
				endDateTime = &calendar.EventDateTime{DateTime: eventDetails.EndTime}
			}

			re := regexp.MustCompile(`[a-zA-Z0-9_]{15,}`)
			validIDs := re.FindAllString(eventDetails.EventID, -1)
			eventDetails.EventID = strings.Join(validIDs, " ")

			if (eventDetails.Action == "update" || eventDetails.Action == "delete") && eventDetails.EventID == "" {
				fmt.Printf("[Agent] Event ID for '%s' not in current view. Searching full calendar memory...\n", eventDetails.Title)

				raw, err := redisClient.Get(ctx, "all_events").Result()
				if err == nil {
					var allEvents []MiniEvent
					json.Unmarshal([]byte(raw), &allEvents)

					query := strings.ToLower(eventDetails.Title)
					var matchedIDs []string

					for _, ev := range allEvents {
						if strings.Contains(strings.ToLower(ev.Summary), query) {
							matchedIDs = append(matchedIDs, ev.ID)
						}
					}

					if len(matchedIDs) == 1 {
						eventDetails.EventID = matchedIDs[0]
						fmt.Println("[System] Found exact match. Proceeding with modification...")
					} else if len(matchedIDs) > 1 {
						promptLower := strings.ToLower(initialPrompt)

						if eventDetails.Action == "delete" {
							isDuplicateRequest := strings.Contains(promptLower, "duplicate") || strings.Contains(promptLower, "redundant")
							isAllRequest := strings.Contains(promptLower, "all")

							if isDuplicateRequest {

								eventDetails.EventID = strings.Join(matchedIDs[1:], " ")
								fmt.Printf("[System] Found %d matching instances. Preserving one and deleting %d duplicates...\n", len(matchedIDs), len(matchedIDs)-1)
							} else if isAllRequest {
								eventDetails.EventID = strings.Join(matchedIDs, " ")
								fmt.Printf("[System] Found %d matches. Proceeding with total bulk deletion...\n", len(matchedIDs))
							} else {
								fmt.Printf("[Agent] I found %d events matching '%s'. Please specify if you want to delete 'all' of them or just 'duplicates'.\n", len(matchedIDs), eventDetails.Title)
								return
							}
						} else {
							fmt.Printf("[Agent] I found %d events matching '%s'. Please run a search first to find the exact one you want to %s.\n", len(matchedIDs), eventDetails.Title, eventDetails.Action)
							return
						}
					} else {
						fmt.Printf("[Agent] I couldn't find any event matching '%s' on your calendar.\n", eventDetails.Title)
						return
					}
				}
			}

			switch eventDetails.Action {
			case "insert":
				event := &calendar.Event{
					Summary:     eventDetails.Title,
					Description: eventDetails.Description,
					Location:    eventDetails.Location,
					Start:       startDateTime,
					End:         endDateTime,
				}

				if len(eventDetails.Reminders) > 0 {
					var overrides []*calendar.EventReminder
					for _, r := range eventDetails.Reminders {
						overrides = append(overrides, &calendar.EventReminder{
							Method:  r.Method,
							Minutes: int64(r.Minutes),
						})
					}
					event.Reminders = &calendar.EventReminders{
						UseDefault:      false,
						ForceSendFields: []string{"UseDefault"}, // Forces Go to send 'false' in the JSON payload
						Overrides:       overrides,
					}
				}

				if eventDetails.Recurrence != "" {
					event.Recurrence = []string{eventDetails.Recurrence}
				}

				createdEvent, err := srv.Events.Insert(calendarID, event).SendUpdates("all").Do()
				if err != nil {
					fmt.Printf("[Error] Failed to insert: %v\n", err)
					return
				}
				fmt.Printf("[System] Successfully scheduled '%s'!\n", eventDetails.Title)
				redisClient.Set(ctx, "new_state", currentState+fmt.Sprintf("\n- [Added] %s at %s", eventDetails.Title, eventDetails.StartTime), 0)

				raw, redisErr := redisClient.Get(ctx, "all_events").Result()
				if redisErr == nil {
					var allEvents []MiniEvent
					json.Unmarshal([]byte(raw), &allEvents)

					dateStr := createdEvent.Start.DateTime
					if dateStr == "" {
						dateStr = createdEvent.Start.Date
					}

					allEvents = append(allEvents, MiniEvent{
						ID:      createdEvent.Id,
						Summary: createdEvent.Summary,
						Date:    dateStr,
					})

					sort.Slice(allEvents, func(i, j int) bool {
						return allEvents[i].Date < allEvents[j].Date
					})

					newData, _ := json.Marshal(allEvents)
					redisClient.Set(ctx, "all_events", string(newData), 0)
				}

			case "delete":
				if eventDetails.EventID == "" {
					fmt.Println("[Error] No EventID provided for deletion.")
					return
				}

				re := regexp.MustCompile(`[a-zA-Z0-9_]{15,}`)
				targetIDs := re.FindAllString(eventDetails.EventID, -1)

				if len(targetIDs) == 0 {
					fmt.Printf("[Error] Could not parse a valid ID from: '%s'\n", eventDetails.EventID)
					return
				}

				for _, id := range targetIDs {
					fmt.Printf("[System] Sending delete request to Google for ID: %s...\n", id)
					err = srv.Events.Delete(calendarID, id).Do()
					if err != nil {
						if strings.Contains(err.Error(), "404") {
							fmt.Printf("[System] Event (ID: %s) was already deleted on Google Calendar.\n", id)
						} else {
							fmt.Printf("[Error] Google API failed to delete %s: %v\n", id, err)
							continue
						}
					} else {
						fmt.Printf("[System] Successfully deleted event ID: %s from Google!\n", id)
					}

					raw, redisErr := redisClient.Get(ctx, "all_events").Result()
					if redisErr == nil {
						var allEvents []MiniEvent
						json.Unmarshal([]byte(raw), &allEvents)

						var updatedEvents []MiniEvent
						for _, ev := range allEvents {
							if ev.ID != id {
								updatedEvents = append(updatedEvents, ev)
							}
						}

						newData, _ := json.Marshal(updatedEvents)
						redisClient.Set(ctx, "all_events", string(newData), 0)
						fmt.Println("[System] Local Redis cache successfully updated.")
					} else {
						fmt.Printf("[Error] Failed to update Redis cache: %v\n", redisErr)
					}
				}
			case "update":
				if eventDetails.EventID == "" {
					fmt.Println("[Error] No EventID provided by LLM for update.")
					return
				}

				event := &calendar.Event{
					Summary:     eventDetails.Title,
					Description: eventDetails.Description,
					Location:    eventDetails.Location,
					Start:       startDateTime,
					End:         endDateTime,
				}

				if len(eventDetails.Reminders) > 0 {
					var overrides []*calendar.EventReminder
					for _, r := range eventDetails.Reminders {
						overrides = append(overrides, &calendar.EventReminder{
							Method:  r.Method,
							Minutes: int64(r.Minutes),
						})
					}
					event.Reminders = &calendar.EventReminders{
						UseDefault:      false,
						ForceSendFields: []string{"UseDefault"},
						Overrides:       overrides,
					}
				}

				if eventDetails.Recurrence != "" {
					event.Recurrence = []string{eventDetails.Recurrence}
				}

				_, err = srv.Events.Update(calendarID, eventDetails.EventID, event).SendUpdates("all").Do()
				re := regexp.MustCompile(`[a-zA-Z0-9_]{15,}`)
				targetIDs := re.FindAllString(eventDetails.EventID, -1)

				if len(targetIDs) == 0 {
					fmt.Printf("[Error] Could not parse a valid ID for update.\n")
					return
				}

				for _, id := range targetIDs {
					existingEvent, err := srv.Events.Get(calendarID, id).Do()
					if err != nil {
						fmt.Printf("[Error] Failed to fetch event %s for update: %v\n", id, err)
						continue
					}

					if eventDetails.Title != "" {
						existingEvent.Summary = eventDetails.Title
					}
					if eventDetails.Description != "" {
						existingEvent.Description = eventDetails.Description
					}
					if eventDetails.Location != "" {
						existingEvent.Location = eventDetails.Location
					}
					if eventDetails.StartTime != "" && eventDetails.EndTime != "" {
						existingEvent.Start = startDateTime
						existingEvent.End = endDateTime
					}

					if len(eventDetails.Reminders) > 0 {
						var overrides []*calendar.EventReminder
						for _, r := range eventDetails.Reminders {
							overrides = append(overrides, &calendar.EventReminder{
								Method:  r.Method,
								Minutes: int64(r.Minutes),
							})
						}
						existingEvent.Reminders = &calendar.EventReminders{
							UseDefault:      false,
							ForceSendFields: []string{"UseDefault"},
							Overrides:       overrides,
						}
					}

					updatedEvent, err := srv.Events.Update(calendarID, id, existingEvent).SendUpdates("all").Do()
					if err != nil {
						fmt.Printf("[Error] Failed to update %s: %v\n", id, err)
						continue
					}
					fmt.Printf("[System] Successfully updated '%s'!\n", updatedEvent.Summary)

					raw, redisErr := redisClient.Get(ctx, "all_events").Result()
					if redisErr == nil {
						var allEvents []MiniEvent
						json.Unmarshal([]byte(raw), &allEvents)

						dateStr := updatedEvent.Start.DateTime
						if dateStr == "" {
							dateStr = updatedEvent.Start.Date
						}

						for i, ev := range allEvents {
							if ev.ID == id {
								allEvents[i].Summary = updatedEvent.Summary
								allEvents[i].Date = dateStr
								break // Stop searching once we've updated it
							}
						}

						sort.Slice(allEvents, func(i, j int) bool {
							return allEvents[i].Date < allEvents[j].Date
						})

						newData, _ := json.Marshal(allEvents)
						redisClient.Set(ctx, "all_events", string(newData), 0)
					}
				}
			}

			break
		}
	}
}

func handleExplanation(ctx context.Context, llm *ollama.LLM, scanner *bufio.Scanner, initialPrompt string) {
	fmt.Println("\n[Agent] Analyzing your calendar...")

	currentPageStr, _ := redisClient.Get(ctx, "current_page").Result()
	var pageNum int
	fmt.Sscanf(currentPageStr, "%d", &pageNum)
	currentState := getPageFromRedis(ctx, pageNum, 10)
	conversationHistory := fmt.Sprintf("User's request: %s\n", initialPrompt)

	now := time.Now()
	timeContext := fmt.Sprintf("Today is %s. Current time is %s.", now.Format("Monday, Jan 2, 2006"), now.Format("3:04 PM"))

	for {
		systemPrompt := fmt.Sprintf(`You are a calendar assistant.
%s
Current Calendar Data: %s
Conversation so far: %s

Task:
1. Determine if you have enough information to answer the user's request. If the user asked a vague question (e.g., "What's my schedule?"), set status to "needs_clarification" and provide a "message" asking them for a timeframe (e.g., "For today or tomorrow?").
2. If the user provided a timeframe, or clarified it in the conversation history, analyze the 'Current Calendar Data' and set status to "ready". Provide a "message" containing a helpful summary of their schedule. THIS SUMMARY MUST BE FORMATTED AS A BULLETED LIST (e.g., start items with '-' and use '\n' for line breaks).

Respond ONLY with valid JSON in this exact format: {"status": "needs_clarification|ready", "message": "..."}`, timeContext, currentState, conversationHistory)

		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		}

		// 30-second LLM response timeout
		llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := llm.GenerateContent(llmCtx, messages, llms.WithJSONMode())
		cancel()
		if err != nil {
			log.Printf("[Error] LLM failed to evaluate explanation request: %v", err)
			return
		}

		var result ExplanationResponse
		err = json.Unmarshal([]byte(resp.Choices[0].Content), &result)
		if err != nil {
			log.Printf("[Error] Failed to parse LLM JSON: %v. Raw: %s", err, resp.Choices[0].Content)
			return
		}

		if result.Status == "needs_clarification" {
			fmt.Printf("[Agent] %s\n>> ", result.Message)
			scanner.Scan()
			userReply := scanner.Text()
			conversationHistory += fmt.Sprintf("Agent: %s\nUser: %s\n", result.Message, userReply)
		} else {
			fmt.Printf("\n[Summary] %s\n", result.Message)
			break
		}
	}
}

func handleStop(ctx context.Context) bool {
	newState, _ := redisClient.Get(ctx, "new_state").Result()
	latestState, _ := redisClient.Get(ctx, "latest_state").Result()

	filter := bson.M{"_id": "latest_state"}
	update := bson.M{"$set": bson.M{"state": newState}}
	opts := options.Update().SetUpsert(true)

	_, err := mongoCollection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		log.Printf("[Error] Failed to save state to MongoDB: %v", err)
	} else {
		fmt.Println("\n[System] Persistent DB 'latest_state' overwritten with memory state.")
	}

	log.Println("[Summary] Session Evolution:\nStarted with: %s\nEnded with: %s\n", latestState, newState)
	fmt.Println("[System] Goodbye! Terminating application.")
	return true
}
