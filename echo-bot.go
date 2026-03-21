package main

import (
	"bytes"
	"encoding/base64"
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
// Configuration – all values are read from environment variables with sensible defaults
// ─────────────────────────────────────────────────────────────────────────────
var (
	baseURL      = getEnv("WEPROTOCOL_BASE_URL", "http://127.0.0.1:8080")
	wxid         = getEnv("WEPROTOCOL_WXID", "")
	pollInterval = mustParseDuration(getEnv("WEPROTOCOL_POLL_INTERVAL", "3s"))
	httpClient   = &http.Client{Timeout: 15 * time.Second}
)

// currentSynckey is automatically maintained across poll cycles exactly as required by the WeProtocol server
var currentSynckey string

// ─────────────────────────────────────────────────────────────────────────────
// API data structures – precisely matching the WeProtocol Swagger and internal models
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

type LoginGetQRResp struct {
	Code int `json:"Code"`
	Data struct {
		Uuid    string `json:"Uuid"`
		QRCode  string `json:"QRCode"`
		Message string `json:"Message"`
	} `json:"Data"`
}

type LoginCheckResp struct {
	Code int `json:"Code"`
	Data struct {
		Wxid string `json:"Wxid"`
	} `json:"Data"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Main entry point
// ─────────────────────────────────────────────────────────────────────────────
func main() {
	// Validate required config
	if wxid == "" {
		log.Println("No WXID configured – starting automatic login sequence")
		doLogin()
	}

	if wxid == "" {
		log.Fatal("WEPROTOCOL_WXID environment variable is required")
	}

	log.Printf("WeProtocol Professional Client started")
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
// Message business logic – single place for all command handling
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

// ─────────────────────────────────────────────────────────────────────────────
// /info command – returns every piece of information derivable from the message payload
// ─────────────────────────────────────────────────────────────────────────────
func sendInfo(msg Message) {
	isGroup := strings.HasSuffix(msg.FromUserName, "@chatroom")
	talker := msg.FromUserName
	room := "Private chat (1:1)"
	if isGroup {
		room = msg.FromUserName
		talker = "Group member (sender WXID not exposed in basic payload)"
	}

	ts := "unknown"
	if msg.CreateTime != 0 {
		ts = time.Unix(msg.CreateTime, 0).Format(time.RFC3339)
	}

	preview := msg.Content
	if len(msg.Content) > 120 {
		preview = msg.Content[:120] + "..."
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
• Content preview: %s`,
		talker,
		room,
		msg.ToUserName,
		ts,
		msg.MsgId,
		msg.MsgType,
		msg.Status,
		msg.ImgStatus,
		preview,
	)

	sendMessage(msg.FromUserName, info)
}

// ─────────────────────────────────────────────────────────────────────────────
// Automatic login flow – runs only when no WXID is present
// ─────────────────────────────────────────────────────────────────────────────
func doLogin() {
	log.Println("Requesting QR code...")

	qrResp, err := loginGetQR()
	if err != nil {
		log.Fatal("Failed to get QR code:", err)
	}
	if qrResp.Code != 0 {
		log.Fatal("LoginGetQR API error:", qrResp.Data.Message)
	}

	uuid := qrResp.Data.Uuid
	if uuid == "" {
		log.Fatal("No UUID returned from server")
	}

	handled := false
	if qrResp.Data.QRCode != "" {
		if saveQRAsPNG(qrResp.Data.QRCode) {
			log.Println("QR code saved as login_qr.png")
			log.Println("   → Open login_qr.png and scan with WeChat")
			handled = true
		} else if strings.HasPrefix(qrResp.Data.QRCode, "http") {
			log.Println("QR code URL:", qrResp.Data.QRCode)
			log.Println("   → Open the URL in browser and scan with WeChat")
			handled = true
		}
	}

	if !handled {
		qrURL := "https://login.weixin.qq.com/l/" + uuid
		log.Println("QR code URL:", qrURL)
		log.Println("   → Open this URL in any browser and scan with WeChat")
	}

	log.Println("Waiting for scan and confirmation on your phone...")

	for {
		checkResp, err := loginCheckQR(uuid)
		if err != nil {
			log.Printf("Check error (retrying): %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if checkResp.Code == 0 && checkResp.Data.Wxid != "" {
			wxid = checkResp.Data.Wxid
			log.Printf("Login successful! WXID: %s", wxid)
			return
		}

		log.Printf("Login status: Code=%d (still waiting)", checkResp.Code)
		time.Sleep(2 * time.Second)
	}
}

func loginGetQR() (LoginGetQRResp, error) {
	resp, err := httpClient.Post(baseURL+"/api/Login/LoginGetQR", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return LoginGetQRResp{}, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var r LoginGetQRResp
	json.Unmarshal(raw, &r)
	return r, nil
}

func loginCheckQR(uuid string) (LoginCheckResp, error) {
	url := baseURL + "/api/Login/LoginCheckQR?uuid=" + uuid
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return LoginCheckResp{}, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var r LoginCheckResp
	json.Unmarshal(raw, &r)
	return r, nil
}

func saveQRAsPNG(qrData string) bool {
	b64 := qrData
	if idx := strings.Index(b64, ","); idx != -1 {
		b64 = b64[idx+1:]
	}

	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}

	return os.WriteFile("login_qr.png", data, 0644) == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// WeProtocol API layer – core sync and send operations with full error handling
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
// Configuration helpers
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
