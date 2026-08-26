package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"football-scraping/internal/db"
	"football-scraping/internal/queue"

	"github.com/dop251/goja"
	"github.com/mxschmitt/playwright-go"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func main() {
	ctx := context.Background()

	redisClient, err := queue.Connect(ctx)
	if err != nil {
		log.Fatalf("could not connect to redis: %v", err)
	}
	defer redisClient.Close()

	if err := queue.EnsureJobsStream(ctx, redisClient); err != nil {
		log.Fatalf("could not ensure jobs stream: %v", err)
	}

	mongoClient, err := db.Connect(ctx)
	if err != nil {
		log.Fatalf("could not connect to mongo: %v", err)
	}
	defer mongoClient.Disconnect(ctx) //nolint:errcheck

	consumerName, err := os.Hostname()
	if err != nil {
		consumerName = "worker"
	}

	pw, err := playwright.Run()
	if err != nil {
		log.Fatalf("could not start playwright: %v", err)
	}
	defer pw.Stop()

	// WhoScored blocks/challenges headless Chromium, so this runs "headed"
	// against a virtual display (see the xvfb-run wrapper in
	// deployments/worker.Dockerfile) rather than with Headless: true.
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(false),
	})
	if err != nil {
		log.Fatalf("could not launch browser: %v", err)
	}
	defer browser.Close()

	log.Printf("worker %q listening on stream %q (group %q)", consumerName, queue.JobsStream, queue.JobsConsumerGroup)

	for {
		streams, err := redisClient.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    queue.JobsConsumerGroup,
			Consumer: consumerName,
			Streams:  []string{queue.JobsStream, ">"},
			Count:    1,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			if err == redis.Nil {
				// Block timed out with no new jobs, poll again.
				continue
			}
			log.Printf("could not read from jobs stream: %v", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range streams {
			for _, message := range stream.Messages {
				processJob(ctx, browser, mongoClient, redisClient, message)
			}
		}
	}
}

// processJob looks up the job the stream message refers to, scrapes its
// match URL, and stores the parsed match data. The job's status in Mongo
// tracks progress: pending while it's being worked on, completed once the
// match data is saved, error if any step fails. The stream message is only
// XAck-ed on success, so a job that errors out stays in the consumer
// group's pending entries list and can be retried by picking it back up.
func processJob(ctx context.Context, browser playwright.Browser, mongoClient *mongo.Client, redisClient *redis.Client, message redis.XMessage) {
	jobIDHex, _ := message.Values["job_id"].(string)
	log.Printf("picked up job %s (stream message %s)", jobIDHex, message.ID)

	// A panic anywhere below (e.g. extractJson's panic on malformed JS)
	// must not take the whole worker process down with it.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("recovered from panic while processing job %s: %v", jobIDHex, r)
			if jobID, err := primitive.ObjectIDFromHex(jobIDHex); err == nil {
				markJobError(ctx, mongoClient, jobID)
			}
		}
	}()

	jobID, err := primitive.ObjectIDFromHex(jobIDHex)
	if err != nil {
		log.Printf("invalid job id %s: %v", jobIDHex, err)
		return
	}

	if err := db.UpdateJobStatus(ctx, mongoClient, jobID, db.JobStatusPending); err != nil {
		log.Printf("could not mark job %s pending: %v", jobIDHex, err)
	}

	job, err := db.GetJob(ctx, mongoClient, jobID)
	if err != nil {
		log.Printf("could not load job %s: %v", jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}

	// This only navigates straight to job.URL and extracts JSON embedded in
	// the page, never matching on text content - so it doesn't need (and
	// shouldn't force) English locale. WhoScored's geo-redirect to a
	// localized subdomain is a real, repeating server-side 302 for deep
	// URLs like this one, and blockLocaleRedirect fighting it just causes a
	// redirect loop, so plain NewContext is used.
	browserCtx, err := browser.NewContext()
	if err != nil {
		log.Printf("could not create browser context for job %s: %v", jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}
	defer browserCtx.Close()

	page, err := browserCtx.NewPage()
	if err != nil {
		log.Printf("could not open page for job %s: %v", jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}
	defer page.Close()

	if _, err := page.Goto(job.URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		log.Printf("could not navigate to %s for job %s: %v", job.URL, jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}

	html, err := waitForMatchCentreHTML(page)
	if err != nil {
		log.Printf("could not load match data for job %s: %v", jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}

	matchData := parseHTML(html)

	if err := db.SaveMatch(ctx, mongoClient, jobID, job.League, job.Season, job.URL, matchData); err != nil {
		log.Printf("could not save match data for job %s: %v", jobIDHex, err)
		markJobError(ctx, mongoClient, jobID)
		return
	}

	if err := db.UpdateJobStatus(ctx, mongoClient, jobID, db.JobStatusCompleted); err != nil {
		log.Printf("could not mark job %s completed: %v", jobIDHex, err)
	}

	if err := redisClient.XAck(ctx, queue.JobsStream, queue.JobsConsumerGroup, message.ID).Err(); err != nil {
		log.Printf("could not ack job %s: %v", jobIDHex, err)
	}

	log.Printf("completed job %s", jobIDHex)
}

// waitForMatchCentreHTML polls the page until the match-centre data has been
// embedded in the page source, or gives up after 30 seconds.
func waitForMatchCentreHTML(page playwright.Page) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		html, err := page.Content()
		if err != nil {
			return "", err
		}
		if strings.Contains(html, "require.config.params['matchheader'] = ") {
			return html, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for match-centre data")
		}
		time.Sleep(1 * time.Second)
	}
}

func markJobError(ctx context.Context, mongoClient *mongo.Client, jobID primitive.ObjectID) {
	if err := db.UpdateJobStatus(ctx, mongoClient, jobID, db.JobStatusError); err != nil {
		log.Printf("could not mark job %s as errored: %v", jobID.Hex(), err)
	}
}

func parseHTML(html string) map[string]interface{} {
	result := make(map[string]interface{})

	match_info := extractJson(html, "require.config.params['matchheader']")
	match_details := extractJson(html, "require.config.params[\"args\"]")

	match_id := matchIDString(match_info["matchId"])
	if match_id == "" {
		return result
	}

	result[match_id] = map[string]interface{}{
		"match_info":    match_info,
		"match_details": match_details,
	}

	return result
}

// matchIDString converts WhoScored's matchId field to a string key,
// whichever concrete type goja exported it as. It's a numeric JS literal,
// and goja exports JS numbers as int64 when they're integral or float64
// otherwise, never as a string.
func matchIDString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return ""
	}
}

// extractJson finds "target = {...}" directly in html and evaluates the JS
// object literal that follows. It searches the whole page rather than a
// pre-isolated <script> tag, because splitting the page into script tags
// with a single regex is fragile: any earlier, unrelated script containing
// a "</script>"-like sequence inside a string desyncs every match after it,
// which can make the real assignment impossible to isolate.
func extractJson(html, target string) map[string]interface{} {
	needle := target + " = "
	idx := strings.Index(html, needle)
	if idx == -1 {
		return nil
	}

	rest := html[idx+len(needle):]

	end := strings.Index(rest, "</script>")
	if end == -1 {
		return nil
	}

	contentParts := strings.TrimRight(rest[:end], " \n\r\t;")
	contentParts = fmt.Sprintf("(%s)", contentParts)

	vm := goja.New()

	// Evaluate JS object
	v, err := vm.RunString(contentParts)
	if err != nil {
		panic(err)
	}

	// Export to Go value
	exported, ok := v.Export().(map[string]interface{})
	if !ok {
		return nil
	}

	return exported
}
