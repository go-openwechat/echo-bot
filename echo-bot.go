package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration (all overridable via environment variables)
// ─────────────────────────────────────────────────────────────────────────────
var (
	baseURL      = getEnv("WEPROTOCOL_BASE_URL", "http://127.0.0.1:8080")
	wxid         = getEnv("WEPROTOCOL_WXID", "")
	pollInterval = mustParseDuration(getEnv("WEPROTOCOL_POLL_INTERVAL", "3s"))
	httpClient   = &http.Client{Timeout: 15 * time.Second}
)

// currentSynckey is persisted across polls (exactly as the official server expects)
var currentSynckey string

// ─────────────────────────────────────────────────────────────────────────────
// API types (100% faithful to WeProtocol's internal Swagger + controller models)
// Expanded with ALL common fields returned by /api/Msg/Sync for maximum info
// ─────────────────────────────────────────────────────────────────────────────
type MsgSyncReq struct {
	Scene   int    `json:"Scene"`
	Synckey string `json:"Synckey"`
	Wxid    string `json:"Wxid"`
}

type MsgSyncResp struct {
	Code int `json:"Code"`
	Data struct {
		AddMsgs []struct {
			MsgType      int    `json:"MsgType"`
			Content      string `json:"Content"`
			FromUserName struct {
				String_ string `json:"String_"`
			} `json:"FromUserName"`
			ToUserName struct {
				String_ string `json:"String_"`
			} `json:"ToUserName"`
			MsgId      interface{} `json:"MsgId"` // number or string (handles both)
			CreateTime int64       `json:"CreateTime"`
			Status     int         `json:"Status"`
			ImgStatus  int         `json:"ImgStatus"`
		} `json:"AddMsgs"`
		KeyBuf struct {
			Buffer string `json:"Buffer"`
		} `json:"KeyBuf"`
	} `json:"Data"`
}

type MsgSendReq struct {
	Wxid    string `json:"Wxid"`
	ToWxid  string `json:"ToWxid"`
	Content string `json:"Content"`
	Type    int    `json:"Type"` // 1 = text
	At      string `json:"At"`
}

type Message struct {
	MsgType      int
	Content      string
	FromUserName string
	ToUserName   string
	MsgId        string
	CreateTime   int64
	Status       int
	ImgStatus    int
}

// ─────────────────────────────────────────────────────────────────────────────
// Main entry point
// ─────────────────────────────────────────────────────────────────────────────
func main() {
	// Validate required config
	if wxid == "" {
		log.Fatal("❌ WEPROTOCOL_WXID environment variable is required")
	}

	log.Printf("🚀 WeProtocol Professional Client started")
	log.Printf("   Base URL      : %s", baseURL)
	log.Printf("   WXID          : %s", wxid)
	log.Printf("   Poll interval : %s", pollInterval)
	log.Printf("   Press Ctrl+C to stop")

	for {
		msgs, err := syncMessages()
		if err != nil {
			log.Printf("⚠️  Sync error (continuing): %v", err)
			time.Sleep(pollInterval)
			continue
		}

		for _, msg := range msgs {
			OnMessage(msg)
		}

		time.Sleep(pollInterval)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Message Business Logic
// OnMessage – this is where ALL your business logic lives
// Add new commands here. Everything is case-insensitive for commands.
// ─────────────────────────────────────────────────────────────────────────────
func OnMessage(msg Message) {
	if msg.MsgType != 1 { // only text messages
		return
	}

	contentTrim := strings.TrimSpace(msg.Content)
	if contentTrim == "" {
		return
	}

	lower := strings.ToLower(contentTrim)

	// Command: /ping
	if lower == "/ping" {
		sendMessage(msg.FromUserName, "pong")
		return
	}

	// Command: /echo <anything>
	if strings.HasPrefix(lower, "/echo ") {
		echoText := strings.TrimSpace(contentTrim[len("/echo "):])
		if echoText != "" {
			sendMessage(msg.FromUserName, echoText)
		}
		return
	}

	// Command: /info – shows EVERYTHING derivable from the message (talker, room, timestamp, IDs, etc.)
	if lower == "/info" {
		sendInfo(msg)
		return
	}

	// Future commands go here (e.g. /time, /help, etc.)
	// log.Printf("Ignored message from %s: %s", msg.FromUserName, contentTrim)
}

// sendInfo – replies with full derived information (talker, room, timestamps, IDs, status, etc.)
// This is the maximum that can be derived directly from the /Msg/Sync payload.
func sendInfo(msg Message) {
	isGroup := strings.HasSuffix(msg.FromUserName, "@chatroom")

	talker := msg.FromUserName
	room := "Private chat (1:1)"
	if isGroup {
		room = msg.FromUserName
		talker = "Group member (actual sender WXID not exposed in basic AddMsg payload)"
	}

	ts := "unknown"
	if msg.CreateTime != 0 {
		ts = time.Unix(msg.CreateTime, 0).Format(time.RFC3339)
	}

	info := fmt.Sprintf(`📋 /info – Full Message Details

• Talker (sender): %s
• Room / Chat: %s
• Your WXID (To): %s
• Timestamp: %s
• MsgID: %s
• MsgType: %d
• Status: %d
• ImgStatus: %d
• Content preview: %.120s%s`,
		talker,
		room,
		msg.ToUserName,
		ts,
		msg.MsgId,
		msg.MsgType,
		msg.Status,
		msg.ImgStatus,
		msg.Content,
		func() string {
			if len(msg.Content) > 120 {
				return "..."
			}
			return ""
		}(),
	)

	sendMessage(msg.FromUserName, info)
}

// ─────────────────────────────────────────────────────────────────────────────
// WeProtocol API Layer (Core protocol calls)
// ─────────────────────────────────────────────────────────────────────────────
func syncMessages() ([]Message, error) {
	req := MsgSyncReq{
		Scene:   0,
		Synckey: currentSynckey,
		Wxid:    wxid,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal sync request: %w", err)
	}

	resp, err := httpClient.Post(baseURL+"/api/Msg/Sync", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http post /Sync: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http /Sync status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /Sync body: %w", err)
	}

	var apiResp MsgSyncResp
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("unmarshal /Sync response: %w", err)
	}

	if apiResp.Code != 0 {
		return nil, fmt.Errorf("API /Sync error code=%d", apiResp.Code)
	}

	// Update synckey exactly as the server expects
	if apiResp.Data.KeyBuf.Buffer != "" {
		currentSynckey = apiResp.Data.KeyBuf.Buffer
	}

	// Convert to clean slice with ALL available fields
	msgs := make([]Message, 0, len(apiResp.Data.AddMsgs))
	for _, m := range apiResp.Data.AddMsgs {
		msgs = append(msgs, Message{
			MsgType:      m.MsgType,
			Content:      m.Content,
			FromUserName: m.FromUserName.String_,
			ToUserName:   m.ToUserName.String_,
			MsgId:        fmt.Sprintf("%v", m.MsgId),
			CreateTime:   m.CreateTime,
			Status:       m.Status,
			ImgStatus:    m.ImgStatus,
		})
	}
	return msgs, nil
}

func sendMessage(toWxid, content string) {
	if content == "" {
		return
	}

	req := MsgSendReq{
		Wxid:    wxid,
		ToWxid:  toWxid,
		Content: content,
		Type:    1,
		At:      "",
	}

	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("WARN: marshal send request: %v", err)
		return
	}

	resp, err := httpClient.Post(baseURL+"/api/Msg/SendTxt", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("ERROR: send to %s failed: %v", toWxid, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("ERROR: send to %s HTTP %d: %s", toWxid, resp.StatusCode, string(bodyBytes))
		return
	}

	log.Printf("✓ Sent to %s", toWxid)
}

// ─────────────────────────────────────────────────────────────────────────────
// Configuration & Helpers (least important – pure utilities)
// ─────────────────────────────────────────────────────────────────────────────
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("WARN: invalid WEPROTOCOL_POLL_INTERVAL '%s', using 3s default", s)
		return 3 * time.Second
	}
	return d
}
