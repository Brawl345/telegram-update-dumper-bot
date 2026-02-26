package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	longPollTimeout  = 30
	httpClientTTL    = 35 * time.Second
	apiActionTimeout = 10 * time.Second
	apiSendTimeout   = 30 * time.Second
	getUpdatesCtxTTL = 35 * time.Second
	retryDelay       = 5 * time.Second
)

var client = &http.Client{Timeout: httpClientTTL}

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

	apiResponse struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}

	ReplyParameters struct {
		MessageID int `json:"message_id"`
	}
)

func main() {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	userInfo, err := getBotInfo(botToken)
	if err != nil {
		log.Fatalf("Error validating bot token: %v", err)
	}
	log.Printf("Started %s (@%s, %d)!", userInfo.Name, userInfo.Username, userInfo.ID)

	var lastUpdateID int

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutdown signal received, exiting")
			return
		default:
		}

		updates, err := getUpdates(ctx, botToken, lastUpdateID+1)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("Shutdown signal received, exiting")
				return
			}

			log.Printf("Error fetching updates: %v", err)
			time.Sleep(retryDelay)
			continue
		}

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

			if err := sendChatAction(ctx, botToken, chatID, "upload_document"); err != nil {
				log.Printf("Error sending chat action: %v", err)
			}
			if err := sendDocument(ctx, botToken, chatID, filename, update.RawJSON, messageID); err != nil {
				log.Printf("Error sending file: %v", err)
			}
		}
	}
}

func getBotInfo(botToken string) (*BotInfo, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", botToken)

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("API request error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	var result botInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %v", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", result.Description)
	}

	return &result.Result, nil
}

func sendChatAction(parentCtx context.Context, botToken string, chatID int64, action string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", botToken)

	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling payload: %v", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, apiActionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return doAPIRequest(req)
}

func getUpdates(parentCtx context.Context, botToken string, offset int) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d", botToken, offset, longPollTimeout)

	ctx, cancel := context.WithTimeout(parentCtx, getUpdatesCtxTTL)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result updatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %v", err)
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

func sendDocument(parentCtx context.Context, botToken string, chatID int64, filename, content string, messageID int) error {
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("error writing chat_id field: %v", err)
	}

	replyParamsJSON, err := json.Marshal(ReplyParameters{MessageID: messageID})
	if err != nil {
		return fmt.Errorf("error marshaling reply parameters: %v", err)
	}
	if err := writer.WriteField("reply_parameters", string(replyParamsJSON)); err != nil {
		return fmt.Errorf("error writing reply_parameters field: %v", err)
	}

	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return fmt.Errorf("error creating form file field: %v", err)
	}

	if _, err := io.Copy(part, strings.NewReader(formatJSON(content))); err != nil {
		return fmt.Errorf("error writing file data: %v", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("error closing multipart writer: %v", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", botToken)

	ctx, cancel := context.WithTimeout(parentCtx, apiSendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, &requestBody)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return doAPIRequest(req)
}

func doAPIRequest(req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fmt.Errorf("error parsing response: %v", err)
	}

	if !apiResp.OK {
		if apiResp.Description != "" {
			return fmt.Errorf("telegram API error: %s", apiResp.Description)
		}
		return fmt.Errorf("telegram API returned ok=false without description")
	}

	return nil
}

func formatJSON(src string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(src), "", "  "); err == nil {
		return buf.String()
	}
	return src
}
