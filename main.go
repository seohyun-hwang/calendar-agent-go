package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/ollama"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

var redisClient *redis.Client
var mongoCollection *mongo.Collection

func fetchCalendarData(srv *calendar.Service, calendarID string) string {
	fmt.Println("[System] Fetching all accessible data from Google Calendar...")
	t := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)

	var allEvents []MiniEvent
	pageToken := ""

	for {
		req := srv.Events.List(calendarID).
			ShowDeleted(false).
			SingleEvents(true).
			TimeMin(t).
			MaxResults(250). // Max allowed per page by Google
			OrderBy("startTime")

		if pageToken != "" {
			req.PageToken(pageToken)
		}

		events, err := req.Do()
		if err != nil {
			log.Printf("[Error] Unable to retrieve calendar events: %v", err)
			break
		}

		for _, item := range events.Items {
			date := item.Start.DateTime
			if date == "" {
				date = item.Start.Date
			}
			allEvents = append(allEvents, MiniEvent{ID: item.Id, Summary: item.Summary, Date: date})
		}

		pageToken = events.NextPageToken
		if pageToken == "" {
			break
		}
	}

	data, _ := json.Marshal(allEvents)
	return string(data)
}

func syncToMemory(ctx context.Context, calendarData string) {
	redisClient.Set(ctx, "all_events", calendarData, 0)
	redisClient.Set(ctx, "latest_state", calendarData, 0) // For MongoDB persistence checks
	redisClient.Set(ctx, "current_page", 1, 0)

	fmt.Println("[System] Synced entire Google Calendar to short-term memory.")
	fmt.Println(getPageFromRedis(ctx, 1, 10)) // Print Page 1 on startup
}

func compareWithPersistent(ctx context.Context) string {
	memState, _ := redisClient.Get(ctx, "latest_state").Result()

	var result CalendarState
	err := mongoCollection.FindOne(ctx, bson.M{"_id": "latest_state"}).Decode(&result)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "No previous state found in MongoDB. This must be the first run!"
		}
		return fmt.Sprintf("[Error] Failed to fetch persistent state: %v", err)
	}

	persistState := result.State
	if memState == persistState {
		return "No changes since last session."
	}
	log.Printf("State Differences Found:\nPersistent: %s\n\nCurrent: %s", persistState, memState)
	return "Background sync complete. Changes detected since last session."
}

func classifyIntent(ctx context.Context, llm *ollama.LLM, userPrompt string) string {
	systemPrompt := `You are an intent classifier for a Google Calendar agent.
Categorize the user prompt into exactly ONE of these categories:
- "stop": User wants to exit, close, stop, or terminate the app.
- "modify": User wants to schedule, add, cancel, move, or update events.
- "summarize": User wants to know what is on their calendar, or wants an explanation.
- "navigate": User wants to view the next page, previous page, first page, or a specific page number of their calendar.
- "search": User is looking for a specific event by name, keyword, or topic across their entire calendar.
- "invalid": The prompt makes no sense or doesn't fit the above.

Respond ONLY with valid JSON in this exact format: {"intent": "category"}`

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		llms.TextParts(llms.ChatMessageTypeHuman, userPrompt),
	}

	fmt.Print("[Agent] Thinking...")
	// 30-second LLM response timeout
	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := llm.GenerateContent(llmCtx, messages, llms.WithJSONMode())
	cancel()
	if err != nil {
		log.Printf("[Error] LLM failed to generate intent: %v", err)
		return "invalid"
	}

	llmOutput := resp.Choices[0].Content

	var result IntentResponse
	err = json.Unmarshal([]byte(llmOutput), &result)
	if err != nil {
		log.Printf("[Error] Failed to parse LLM JSON: %v. Raw Output: %s", err, llmOutput)
		return "invalid"
	}

	return result.Intent
}

func initMongoDB(ctx context.Context) {
	clientOptions := options.Client().ApplyURI("mongodb://localhost:27017")
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	err = client.Ping(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to ping MongoDB: %v", err)
	}

	mongoCollection = client.Database("agent_db").Collection("calendar_states")
	fmt.Println("[System] Connected to MongoDB.")
}
func initRedis(ctx context.Context) {
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})

	_, err := redisClient.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	fmt.Println("[System] Connected to Redis.")
}

func main() {
	logFile, err := os.OpenFile("agent.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		return
	}
	defer logFile.Close()

	log.SetOutput(logFile)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	log.Println("=== NEW AGENT SESSION STARTED ===")

	fmt.Println("=== INITIALIZING AGENTIC WORKFLOW ===")

	ctx := context.Background()

	initMongoDB(ctx)
	defer mongoCollection.Database().Client().Disconnect(ctx)
	initRedis(ctx)
	defer redisClient.Close()

	llm, err := ollama.New(ollama.WithModel("llama3"))
	if err != nil {
		log.Fatalf("Error initializing LLM: %v\n", err)
	}

	calendarService, err := calendar.NewService(ctx, option.WithCredentialsFile("credentials.json"))
	if err != nil {
		log.Fatalf("Unable to retrieve Calendar client: %v", err)
	}

	myCalendarEmail := os.Getenv("CALENDAR_EMAIL")
	if myCalendarEmail == "" {
		log.Fatal("CALENDAR_EMAIL environment variable is not set")
	}

	calendarData := fetchCalendarData(calendarService, myCalendarEmail)
	syncToMemory(ctx, calendarData)

	fmt.Println("\n=== RECONCILING STATES ===")
	diff := compareWithPersistent(ctx)
	fmt.Println("[Notification]", diff)

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("\n[UI] How can I help you with your calendar today? (or type 'stop' to close)\n>> ")
		scanner.Scan()
		prompt := scanner.Text()

		category := classifyIntent(ctx, llm, prompt)

		switch category {
		case "invalid":
			fmt.Println("[Agent] I didn't quite catch that. Are you looking to modify your calendar, summarize it, or stop?")
		case "modify":
			handleModification(ctx, llm, scanner, prompt, calendarService, myCalendarEmail)
		case "summarize":
			handleExplanation(ctx, llm, scanner, prompt)
		case "navigate":
			handleNavigation(ctx, llm, prompt)
		case "search":
			handleSearch(ctx, llm, prompt)
		case "stop":
			if handleStop(ctx) {
				return
			}
		default:
			fmt.Println("[Agent] Unrecognized intent parsed from LLM.")
		}
	}
}
