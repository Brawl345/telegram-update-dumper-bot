package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	longPollTimeout  = 30
	httpClientTTL    = 35 * time.Second
	apiActionTimeout = 10 * time.Second
	apiSendTimeout   = 30 * time.Second
	getUpdatesCtxTTL = 35 * time.Second
	retryBaseDelay   = 5 * time.Second
	retryMaxDelay    = 5 * time.Minute
	maxBodyBytes     = 10 << 20 // 10 MB
	// maxRetryAfter is the longest retry_after we will honour automatically.
	// Requests whose retry_after exceeds this value are failed immediately.
	maxRetryAfter    = 60 * time.Second
)

// tokenURLPattern matches /bot<token>/ segments in URLs so they can be redacted
// from error messages before those messages are logged.
var tokenURLPattern = regexp.MustCompile(`/bot[^/?#\s]+/`)

// BotToken is an opaque wrapper for the Telegram bot token.
// Its String and GoString methods return a redacted placeholder so the token
// value is never exposed in logs, fmt output, or panic stack traces.
type BotToken struct{ value string }

func newBotToken(s string) BotToken { return BotToken{value: s} }

// String returns a redacted placeholder.
func (t BotToken) String() string { return "<REDACTED>" }

// GoString returns a redacted placeholder.
func (t BotToken) GoString() string { return `BotToken("<REDACTED>")` }

// sanitizeError replaces any bot token embedded in a URL inside an error
// message with a redacted placeholder.
func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(tokenURLPattern.ReplaceAllString(err.Error(), "/bot<REDACTED>/"))
}

type (
	Chat struct {
		ID int64 `json:"id"`
	}

	Message struct {
		MessageID int  `json:"message_id"`
		Chat      Chat `json:"chat"`
	}

	BotInfo struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"first_name"`
	}

	Update struct {
		UpdateID      int     `json:"update_id"`
		Message       Message `json:"message"`
		EditedMessage Message `json:"edited_message"`
		RawJSON       string
	}

	botInfoResponse struct {
		OK          bool    `json:"ok"`
		Result      BotInfo `json:"result"`
		Description string  `json:"description"`
	}

	updatesResponse struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}

	// ResponseParameters mirrors the Telegram API ResponseParameters object.
	// It is present on error responses that carry extra context, e.g. retry_after.
	ResponseParameters struct {
		RetryAfter int `json:"retry_after,omitempty"`
	}

	apiResponse struct {
		OK          bool                `json:"ok"`
		Description string              `json:"description"`
		Parameters  *ResponseParameters `json:"parameters,omitempty"`
	}

	ReplyParameters struct {
		MessageID int `json:"message_id"`
	}
)

// rateLimiter enforces a sliding-window limit of maxEvents per window duration
// for each chat ID. Stale timestamps are evicted lazily on each call to Allow,
// so no background goroutine is required.
type rateLimiter struct {
	mu        sync.Mutex
	entries   map[int64][]time.Time
	maxEvents int
	window    time.Duration
}

func newRateLimiter(maxEvents int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		entries:   make(map[int64][]time.Time),
		maxEvents: maxEvents,
		window:    window,
	}
}

// Allow returns true if the chat is within its rate limit and records the
// current call. It returns false (without recording) when the limit is exceeded.
func (r *rateLimiter) Allow(chatID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-r.window)

	// Evict timestamps that have fallen outside the window.
	times := r.entries[chatID]
	start := 0
	for start < len(times) && times[start].Before(cutoff) {
		start++
	}
	times = times[start:]

	if len(times) >= r.maxEvents {
		r.entries[chatID] = times // store pruned slice even when rejecting
		return false
	}

	r.entries[chatID] = append(times, now)
	return true
}

func main() {
	rawToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if rawToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}
	botToken := newBotToken(rawToken)

	// Create the HTTP client here and pass it explicitly to every API function
	// to keep the scope of the client clear and prevent unintended reuse.
	httpClient := &http.Client{Timeout: httpClientTTL}

	// Allow each chat/group up to 60 responses per minute.
	limiter := newRateLimiter(60, time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	userInfo, err := getBotInfo(httpClient, botToken)
	if err != nil {
		log.Fatalf("Error validating bot token: %v", err)
	}
	log.Printf("Started %s (@%s, %d)!", userInfo.Name, userInfo.Username, userInfo.ID)

	var lastUpdateID int
	var attempt int

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutdown signal received, exiting")
			return
		default:
		}

		updates, err := getUpdates(ctx, httpClient, botToken, lastUpdateID+1)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("Shutdown signal received, exiting")
				return
			}

			log.Printf("Error fetching updates: %v", err)

			// Exponential back-off with up to 50% jitter, capped at retryMaxDelay.
			delay := retryBaseDelay * (1 << min(attempt, 6)) // 2^6 * 5s ≈ 5 min
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
			delay += time.Duration(rand.Int63n(int64(delay / 2)))
			time.Sleep(delay)
			attempt++
			continue
		}
		attempt = 0

		for _, update := range updates {
			lastUpdateID = update.UpdateID

			var chatID int64
			var messageID int
			var filename string

			if update.Message.Chat.ID != 0 && update.Message.MessageID != 0 {
				chatID = update.Message.Chat.ID
				messageID = update.Message.MessageID
				filename = fmt.Sprintf("telegram_update_%d.json", update.UpdateID)
			} else if update.EditedMessage.Chat.ID != 0 && update.EditedMessage.MessageID != 0 {
				chatID = update.EditedMessage.Chat.ID
				messageID = update.EditedMessage.MessageID
				filename = fmt.Sprintf("telegram_edited_update_%d.json", update.UpdateID)
			}

			if chatID == 0 {
				continue
			}

			if !limiter.Allow(chatID) {
				log.Printf("Rate limit exceeded for chat %d, dropping update %d", chatID, update.UpdateID)
				continue
			}

			if err := sendChatAction(ctx, httpClient, botToken, chatID, "upload_document"); err != nil {
				log.Printf("Error sending chat action: %v", err)
			}
			if err := sendDocument(ctx, httpClient, botToken, chatID, filename, update.RawJSON, messageID); err != nil {
				log.Printf("Error sending file: %v", err)
			}
		}
	}
}

func getBotInfo(httpClient *http.Client, botToken BotToken) (*BotInfo, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", botToken.value)

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("API request error: %w", sanitizeError(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	var result botInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %w", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", result.Description)
	}

	return &result.Result, nil
}

func sendChatAction(parentCtx context.Context, httpClient *http.Client, botToken BotToken, chatID int64, action string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", botToken.value)

	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, apiActionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %w", sanitizeError(err))
	}
	req.Header.Set("Content-Type", "application/json")

	return doAPIRequest(httpClient, req)
}

func getUpdates(parentCtx context.Context, httpClient *http.Client, botToken BotToken, offset int) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d", botToken.value, offset, longPollTimeout)

	ctx, cancel := context.WithTimeout(parentCtx, getUpdatesCtxTTL)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", sanitizeError(err))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request error: %w", sanitizeError(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result updatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %w", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}

	var updates []Update
	for _, raw := range result.Result {
		var update Update
		if err := json.Unmarshal(raw, &update); err != nil {
			log.Printf("Error parsing update: %v", err)
			continue
		}
		update.RawJSON = string(raw)
		updates = append(updates, update)
	}

	return updates, nil
}

func sendDocument(parentCtx context.Context, httpClient *http.Client, botToken BotToken, chatID int64, filename, content string, messageID int) error {
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("error writing chat_id field: %w", err)
	}

	replyParamsJSON, err := json.Marshal(ReplyParameters{MessageID: messageID})
	if err != nil {
		return fmt.Errorf("error marshaling reply parameters: %w", err)
	}
	if err := writer.WriteField("reply_parameters", string(replyParamsJSON)); err != nil {
		return fmt.Errorf("error writing reply_parameters field: %w", err)
	}

	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return fmt.Errorf("error creating form file field: %w", err)
	}

	if _, err := io.Copy(part, strings.NewReader(formatJSON(content))); err != nil {
		return fmt.Errorf("error writing file data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("error closing multipart writer: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", botToken.value)

	ctx, cancel := context.WithTimeout(parentCtx, apiSendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, &requestBody)
	if err != nil {
		return fmt.Errorf("error creating request: %w", sanitizeError(err))
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return doAPIRequest(httpClient, req)
}

func doAPIRequest(httpClient *http.Client, req *http.Request) error {
	for {
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("error sending request: %w", sanitizeError(err))
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("error reading response: %w", err)
		}

		var apiResp apiResponse
		if jsonErr := json.Unmarshal(body, &apiResp); jsonErr != nil {
			// Can't parse body — surface the HTTP status if it's informative.
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
			}
			return fmt.Errorf("error parsing response: %w", jsonErr)
		}

		// Handle Telegram flood control: HTTP 429 with a retry_after hint.
		if resp.StatusCode == http.StatusTooManyRequests &&
			apiResp.Parameters != nil && apiResp.Parameters.RetryAfter > 0 {

			wait := time.Duration(apiResp.Parameters.RetryAfter) * time.Second
			if wait > maxRetryAfter {
				return fmt.Errorf("telegram flood control: retry_after %ds exceeds maximum %.0fs, giving up",
					apiResp.Parameters.RetryAfter, maxRetryAfter.Seconds())
			}

			log.Printf("Telegram flood control: waiting %s before retrying", wait)
			select {
			case <-req.Context().Done():
				return req.Context().Err()
			case <-time.After(wait):
			}

			// Reset the request body so it can be re-read on the next attempt.
			// http.NewRequestWithContext sets GetBody automatically for bytes.Buffer
			// and bytes.NewBuffer bodies, so this works without caller changes.
			if req.GetBody != nil {
				newBody, err := req.GetBody()
				if err != nil {
					return fmt.Errorf("error rebuilding request body for retry: %w", err)
				}
				req.Body = newBody
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
		}

		if !apiResp.OK {
			if apiResp.Description != "" {
				return fmt.Errorf("telegram API error: %s", apiResp.Description)
			}
			return fmt.Errorf("telegram API returned ok=false without description")
		}

		return nil
	}
}

func formatJSON(src string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(src), "", "  "); err == nil {
		return buf.String()
	}
	return src
}
