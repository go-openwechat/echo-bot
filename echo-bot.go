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
	baseURL      = getEnv("WEPROTOCOL_BASE_URL", "http://127.0.0.1:8058")
	wxid         = getEnv("WEPROTOCOL_WXID", "")
	deviceID     = getEnv("WEPROTOCOL_DEVICE_ID", "49c2a248031c98d023f212d289c07d85")
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
	Code    int    `json:"Code"`
	Success bool   `json:"Success"`
	Message string `json:"Message"`
	Data    struct {
		Uuid string `json:"Uuid"`
		//QrBase64    string `json:"QrBase64"`
		QrUrl       string `json:"QrUrl"`
		ExpiredTime string `json:"ExpiredTime"`
	} `json:"Data"`
	Data62   string `json:"Data62"`
	DeviceId string `json:"DeviceId"`
}

type LoginCheckResp struct {
	Code    int    `json:"Code"`
	Success bool   `json:"Success"`
	Message string `json:"Message"`
	Data    struct {
		Uuid   string `json:"uuid"`
		Status int    `json:"status"`
	} `json:"Data"`
	Data62 string `json:"Data62"`
	Debug  string `json:"Debug"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Main entry point
// ─────────────────────────────────────────────────────────────────────────────
func main() {
	// Validate required config
	if wxid == "" {
		log.Fatal("WEPROTOCOL_WXID environment variable is required")
	}
	log.Println("Starting automatic login sequence")
	doLogin()

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
// Automatic login flow – attempts silent resumption or push notification before falling back to QR
// ─────────────────────────────────────────────────────────────────────────────
func doLogin() {
	// Attempt silent session resumption first if the server still holds valid credentials
	if success, err := loginTwiceAutoAuth(); err == nil && success {
		log.Printf("Successfully resumed session for WXID: %s", wxid)
		return
	}

	var uuid string
	log.Println("Attempting push login notification...")
	if pushUuid, err := loginAwaken(); err == nil && pushUuid != "" {
		uuid = pushUuid
		log.Println("Push notification sent to your phone. Please confirm the login.")
	} else {
		log.Println("Push login unavailable, falling back to QR code...")
		qrResp, err := loginGetQR()
		if err != nil {
			log.Fatal("Failed to get QR code:", err)
		}
		if !qrResp.Success {
			log.Printf("]:  %+v\n \n", qrResp)
			log.Fatal("LoginGetQR API error:", qrResp.Message)
		}
		uuid = qrResp.Data.Uuid
		if uuid == "" {
			log.Printf("]:  %+v\n \n", qrResp)
			log.Fatal("No UUID returned from server")
		}

		if qrResp.Data.QrUrl != "" && strings.HasPrefix(qrResp.Data.QrUrl, "http") {
			log.Println("QR code URL:", qrResp.Data.QrUrl)
			log.Println("   → Open the URL in browser and scan with WeChat")
		}
		log.Println("Waiting for scan and confirmation on your phone...")
	}

	for {
		time.Sleep(12 * time.Second)
		checkResp, err := loginCheckQR(uuid)
		if err != nil {
			log.Printf("Check error (retrying): %v", err)
			continue
		}

		// Status 1 = scanned/confirmed, Status 2 = login completed (both mean success now)
		if checkResp.Code == 0 && (checkResp.Data.Status == 1 || checkResp.Data.Status == 2) {
			log.Printf("Login successful! WXID: %s", wxid)
			time.Sleep(8 * time.Second) // brief stabilization delay after login
			return
		}

		log.Printf("Login status: Code=%d, Status=%d (still waiting)", checkResp.Code, checkResp.Data.Status)
	}
}

// loginTwiceAutoAuth attempts a silent login using existing session tokens on the server
func loginTwiceAutoAuth() (bool, error) {
	url := fmt.Sprintf("%s/api/Login/LoginTwiceAutoAuth?wxid=%s", baseURL, wxid)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Code    int  `json:"Code"`
		Success bool `json:"Success"`
	}
	json.Unmarshal(raw, &r)
	return r.Code == 0 && r.Success, nil
}

// loginAwaken triggers a login confirmation push notification to the mobile device
func loginAwaken() (string, error) {
	url := baseURL + "/api/Login/LoginAwaken"
	body := fmt.Sprintf(`{"Wxid":"%s"}`, wxid)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Code    int  `json:"Code"`
		Success bool `json:"Success"`
		Data    struct {
			Uuid string `json:"Uuid"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.Code == 0 && r.Success {
		return r.Data.Uuid, nil
	}
	return "", fmt.Errorf("push login failed: %d", r.Code)
}

func loginGetQR() (LoginGetQRResp, error) {
	// DeviceID is included in the LoginGetQR request body as required by the WeProtocol server
	resp, err := httpClient.Post(baseURL+"/api/Login/LoginGetQR", "application/json", bytes.NewReader([]byte(`{"DeviceID":"`+deviceID+`"}`)))
	if err != nil {
		return LoginGetQRResp{}, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	log.Printf("]:  %+v\n \n", string(raw))
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
	log.Printf("]:  %+v\n \n", string(raw))

	// Handle persistent server-side panic (beego HTML error page) triggered right after QR scan/confirm.
	// Root cause (from stack trace): runtime error: index out of range [0] with length 0
	// in /usr/wic-go/Algorithm/Pack.go:124 → SecManualAuth.go:151 → CheckSecManualAuth.go:129
	// (known wic-go / WeProtocol backend bug during CheckUuid → SecManualAuth transition).
	// When detected we fake a success response (Status=2) so the doLogin loop exits cleanly.
	if len(raw) > 50 && bytes.Contains(raw, []byte("beego application error")) {
		return LoginCheckResp{}, fmt.Errorf("WeProtocol beego server runtime panic")

		// log.Println("⚠️  WeProtocol server runtime panic in SecManualAuth detected after QR scan – treating as login confirmed (known backend bug)")
		// return LoginCheckResp{
		// 	Code:    0,
		// 	Success: true,
		// 	Data: struct {
		// 		Uuid   string `json:"uuid"`
		// 		Status int    `json:"status"`
		// 	}{
		// 		Status: 2,
		// 	},
		// }, nil
	}

	var r LoginCheckResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return LoginCheckResp{}, fmt.Errorf("unmarshal login check response: %w", err)
	}
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

	// Log the raw response (keep original behavior)
	log.Printf("]Msg/Sync:  %+v\n \n", string(raw))

	// Handle both known server-side panics in /Msg/Sync (and LoginCheckQR):
	// 1. Full beego HTML error page
	// 2. Empty/short response (happens when the backend panics before writing JSON)
	// Root cause (from stack trace): slice bounds out of range [:-16] in AES.go:225
	// → Pack.go:208 → Mmtls.go:46 → sync.go:86 (incomplete session keys after buggy SecManualAuth).
	// When detected we return empty slice (no error) so the main loop continues cleanly.
	if len(raw) == 0 || len(raw) < 50 ||
		bytes.Contains(raw, []byte("beego application error")) ||
		bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<!DOCTYPE html>")) {
		log.Println("⚠️  WeProtocol server runtime panic in Msg/Sync (or empty response) detected – known backend bug, continuing with empty sync")
		return []Message{}, nil
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
