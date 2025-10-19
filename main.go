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

	httpClient := &http.Client{
		Timeout: 35 * time.Second, // Slightly longer than getUpdates timeout
	}

	var lastUpdateID int

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
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			lastUpdateID = update.UpdateID

			if update.Message.Chat.ID != 0 && update.Message.MessageID != 0 {
				if err := sendChatAction(ctx, httpClient, update.Message.Chat.ID, "upload_document"); err != nil {
					log.Printf("Error sending chat action: %v", err)
				}
				filename := fmt.Sprintf("telegram_update_%d.json", update.UpdateID)
				if err := sendDocument(ctx, httpClient, update.Message.Chat.ID, filename, update.RawJSON, update.Message.MessageID); err != nil {
					log.Printf("Error sending file: %v", err)
				}
			} else if update.EditedMessage.Chat.ID != 0 && update.EditedMessage.MessageID != 0 {
				if err := sendChatAction(ctx, httpClient, update.EditedMessage.Chat.ID, "upload_document"); err != nil {
					log.Printf("Error sending chat action: %v", err)
				}
				filename := fmt.Sprintf("telegram_edited_update_%d.json", update.UpdateID)
				if err := sendDocument(ctx, httpClient, update.EditedMessage.Chat.ID, filename, update.RawJSON, update.EditedMessage.MessageID); err != nil {
					log.Printf("Error sending file: %v", err)
				}
			}
		}
	}
}

type Message struct {
	MessageID int `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type EditedMessage struct {
	MessageID int `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type BotInfo struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"first_name"`
}

type Update struct {
	UpdateID      int           `json:"update_id"`
	Message       Message       `json:"message"`
	EditedMessage EditedMessage `json:"edited_message"`
	RawJSON       string
}

func getBotInfo(botToken string) (*BotInfo, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", botToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("API request error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	var result struct {
		OK          bool    `json:"ok"`
		Result      BotInfo `json:"result"`
		Description string  `json:"description"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %v", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", result.Description)
	}

	return &result.Result, nil
}

func sendChatAction(parentCtx context.Context, client *http.Client, chatID int64, action string) error {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", botToken)

	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling payload: %v", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxWithTimeout, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
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

func getUpdates(parentCtx context.Context, client *http.Client, botToken string, offset int) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", botToken, offset)

	ctx, cancel := context.WithTimeout(parentCtx, 35*time.Second)
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

	var result struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("error parsing JSON response: %v", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}

	var updates []Update
	for _, rawUpdate := range result.Result {
		var update struct {
			UpdateID      int           `json:"update_id"`
			Message       Message       `json:"message"`
			EditedMessage EditedMessage `json:"edited_message"`
		}

		if err := json.Unmarshal(rawUpdate, &update); err != nil {
			log.Printf("Error parsing update: %v", err)
			continue
		}

		updates = append(updates, Update{
			UpdateID:      update.UpdateID,
			Message:       update.Message,
			EditedMessage: update.EditedMessage,
			RawJSON:       string(rawUpdate),
		})
	}

	return updates, nil
}

func sendDocument(parentCtx context.Context, client *http.Client, chatID int64, filename, content string, messageID int) error {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("error writing chat_id field: %v", err)
	}

	replyParams := struct {
		MessageID int `json:"message_id"`
	}{
		MessageID: messageID,
	}
	replyParamsJSON, err := json.Marshal(replyParams)
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

	formattedJSON := formatJSON(content)
	if _, err := io.Copy(part, strings.NewReader(formattedJSON)); err != nil {
		return fmt.Errorf("error writing file data: %v", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("error closing multipart writer: %v", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", botToken)

	ctxWithTimeout, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxWithTimeout, "POST", url, &requestBody)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
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

func formatJSON(jsonStr string) string {
	var data any
	if err := json.Unmarshal([]byte(jsonStr), &data); err == nil {
		if formatted, err := json.MarshalIndent(data, "", "  "); err == nil {
			return string(formatted)
		}
	}
	return jsonStr
}
