package chathub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"m365-native/internal/proxy"
)

const (
	rs          = "\x1e"
	defaultTone = "magic"
	wsBase      = "wss://substrate.office.com/m365Copilot/Chathub"
)

// Variants mirrored from the verified browser / Python probe.
const variants = "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin,EnableRequestPlugins,feature.EnableSensitivityLabels,EnableUnsupportedUrlDetector,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3,feature.enablechatpages,feature.enableCodeCanvas,feature.turnOnWorkTabRecommendation,turnOffWorkTabUpsellFromClient,feature.turnOnDARecommendation,feature.IsStreamingModeInChatRequestEnabled,IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,feature.EnableDeduplicatingSourceAttributions,Enable3PActionProgressMessages,feature.enableClientWebRtc,feature.EnableMeetingRecapOfSeriesMeetingWithCiq,feature.EnableReferencesListCompleteSignal,feature.StorageMessageSplitDisabled,feature.EnableCuaTakeControlApi,feature.cwcallowedos,feature.disabledisallowedmsgs,feature.enableCitationsForSynthesisData,feature.enableGenerateGraphicArtOptionsSet,cdximagen,feature.EnableUpdatedUXForConfirmationDialog,feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,feature.EnableDesignEditorImageGrounding,feature.EnableDesignerEditor,feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix,feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix,feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"

type Account struct {
	AccessToken string
	OID         string
	TID         string
	Proxy       string
}

type Request struct {
	Text           string
	Tone           string
	ConversationID string
	SessionID      string
	Attachments    []Attachment
	Tools          []Tool
	ToolChoice     any
	// Started is true only for the first turn of a ChatHub conversation.
	Started bool
}

// StreamEvent is the protocol-neutral event exposed while ChatHub is still
// producing a response. Text events are safe to show immediately; progress and
// tool events are normally buffered by protocol adapters.
type StreamEvent struct {
	Kind        string
	Text        string
	MessageType string
	ContentType string
	ToolName    string
	Arguments   json.RawMessage
	Raw         json.RawMessage
}

type StreamHandler func(StreamEvent) error

type Result struct {
	Text           string
	ConversationID string
	SessionID      string
	RequestID      string
	Throttling     any
	RawResult      string
	Events         []json.RawMessage
	Normalized     []Event
	Images         []string
}

type Client struct {
	HTTPHeader http.Header
	Dialer     *websocket.Dialer
}

func NewClient() *Client {
	h := make(http.Header)
	h.Set("Origin", "https://m365.cloud.microsoft")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	return &Client{
		HTTPHeader: h,
		Dialer: &websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			// substrate frames can be large
			ReadBufferSize:  1024 * 1024,
			WriteBufferSize: 64 * 1024,
		},
	}
}

// HTTPClient returns the default outbound client used for attachment
// uploads/fetches when no account proxy is configured.
func (c *Client) HTTPClient() *http.Client {
	return &http.Client{Timeout: 90 * time.Second}
}

func (c *Client) Chat(ctx context.Context, acc Account, req Request) (Result, error) {
	return c.ChatWithDelta(ctx, acc, req, nil)
}

// ChatWithEvents is the compatibility entry point for the full event stream.
// The initial implementation exposes every upstream text delta immediately;
// the existing ChatWithDelta path remains the source of truth until the
// SignalR frame parser is migrated to emit progress/tool events as well.
func (c *Client) ChatWithEvents(ctx context.Context, acc Account, req Request, handler StreamHandler) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) error {
		if handler == nil {
			return nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler)
}

// ChatWithDelta preserves Chat semantics while exposing upstream text deltas as
// soon as SignalR delivers them. onDelta must return quickly; returning an error
// cancels the request. Full snapshot messages are retained for final-result
// reconstruction but are not emitted as deltas, preventing duplicate text.
func (c *Client) ChatWithDelta(ctx context.Context, acc Account, req Request, onDelta func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, nil)
}

func (c *Client) chatWithHandlers(ctx context.Context, acc Account, req Request, onDelta func(string) error, onEvent StreamHandler) (Result, error) {
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return Result{}, fmt.Errorf("missing access token / oid / tid")
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return Result{}, fmt.Errorf("empty prompt")
	}
	if req.Tone == "" {
		req.Tone = defaultTone
	}
	firstTurn := req.Started
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
		firstTurn = true
	}
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
		firstTurn = true
	}
	requestID := uuid.NewString()

	// Enterprise Copilot image input requires uploading image attachments
	// first; the returned docId is carried as a message annotation. A failed
	// upload (network, throttling, unsupported URL) falls back to the legacy
	// imageUrl/imageBase64 injection below instead of failing the request.
	if err := c.uploadAttachments(ctx, acc, req.ConversationID, req.Attachments); err != nil {
		log.Printf("chathub upload attachment: %v", err)
	}

	wsURL, err := buildWSURL(acc, req.SessionID, req.ConversationID, requestID)
	if err != nil {
		return Result{}, err
	}

	dialer, err := proxy.WebSocketDialerFor(c.Dialer, acc.Proxy)
	if err != nil {
		return Result{}, fmt.Errorf("proxy dialer: %w", err)
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, c.HTTPHeader.Clone())
	if err != nil {
		return Result{}, fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+rs)); err != nil {
		return Result{}, fmt.Errorf("handshake send: %w", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		return Result{}, fmt.Errorf("handshake recv: %w", err)
	}

	payload := chatPayload(req.Text, req.SessionID, req.ConversationID, requestID, req.Tone, firstTurn, req.Attachments, req.Tools, req.ToolChoice)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		return Result{}, fmt.Errorf("chat send: %w", err)
	}

	var deltas []string
	var streamedText string
	emitDelta := func(d string) error {
		if d == "" {
			return nil
		}
		streamedText += d
		if onDelta != nil {
			return onDelta(d)
		}
		return nil
	}
	emitSnapshot := func(snapshot string) error {
		if snapshot == "" {
			return nil
		}
		if streamedText != "" && strings.HasPrefix(snapshot, streamedText) {
			return emitDelta(strings.TrimPrefix(snapshot, streamedText))
		}
		return emitDelta(snapshot)
	}
	var final string
	var throttling any
	var rawResult string
	var events []json.RawMessage
	seenStreamTools := map[string]bool{}

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// Never convert a timeout or dropped WebSocket into a successful
			// partial response. A response is complete only after SignalR type 3.
			return Result{}, fmt.Errorf("ws read before completion: %w", err)
		}
		for _, part := range strings.Split(string(msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			events = append(events, json.RawMessage(append([]byte(nil), part...)))
			var obj map[string]any
			if err := json.Unmarshal([]byte(part), &obj); err != nil {
				continue
			}
			t, _ := obj["type"].(float64)
			target, _ := obj["target"].(string)

			// SignalR ping
			if int(t) == 6 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
				continue
			}

			if int(t) == 1 && target == "update" {
				args, _ := obj["arguments"].([]any)
				for _, raw := range args {
					arg, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					msgs, _ := arg["messages"].([]any)
					if onEvent != nil {
						for _, ev := range extractToolEvents(arg, seenStreamTools) {
							if err := onEvent(ev); err != nil {
								return Result{}, err
							}
						}

						for _, ev := range classifyUpdateMessages(msgs) {
							ev.Raw = eventRaw(arg)
							if ev.Kind != "text" {
								if err := onEvent(ev); err != nil {
									return Result{}, err
								}
							}
						}
					}
					toolFrame := false
					for _, mraw := range msgs {
						m, _ := mraw.(map[string]any)
						mt, _ := m["messageType"].(string)
						ct, _ := m["contentType"].(string)
						if mt == "Progress" || ct == "SearchResults" || ct == "Code" || ct == "ToolCall" {
							toolFrame = true
						}
					}
					if w, ok := arg["writeAtCursor"].(string); ok && w != "" && !toolFrame {
						deltas = append(deltas, w)
						if err := emitSnapshot(w); err != nil {
							return Result{}, err
						}
					}
					if thr, ok := arg["throttling"]; ok {
						throttling = thr
					}
					if msgs, ok := arg["messages"].([]any); ok {
						for _, mraw := range msgs {
							m, ok := mraw.(map[string]any)
							if !ok {
								continue
							}
							author, _ := m["author"].(string)
							text, _ := m["text"].(string)
							mt, _ := m["messageType"].(string)
							if author == "bot" && mt == "" && text != "" {
								// ChatHub often sends the first visible text as a full snapshot,
								// followed by cursor deltas. Emit only the unseen suffix.
								deltas = append(deltas, text)
								if err := emitSnapshot(text); err != nil {
									return Result{}, err
								}
							}
						}
					}
				}
				continue
			}

			if int(t) == 2 {
				item, _ := obj["item"].(map[string]any)
				if item != nil {
					if thr, ok := item["throttling"]; ok {
						throttling = thr
					}
					if res, ok := item["result"].(map[string]any); ok {
						rawResult, _ = res["value"].(string)
						if msg, ok := res["message"].(string); ok {
							final = msg
						}
					}
				}
				// completion frame often follows; keep reading a bit but we already have content
				continue
			}

			if int(t) == 3 {
				if errObj, ok := obj["error"].(map[string]any); ok {
					return Result{}, fmt.Errorf("chathub completion error: %v", errObj)
				}
				// end of stream
				text := final
				if text == "" {
					text = strings.Join(deltas, "")
				}
				return Result{
					Text:           text,
					ConversationID: req.ConversationID,
					SessionID:      req.SessionID,
					RequestID:      requestID,
					Throttling:     throttling,
					RawResult:      rawResult,
					Events:         events,
					Normalized:     NormalizeEvents(events),
					Images:         filterEchoedImages(imageURLs(events), req.Attachments),
				}, nil
			}
		}
	}

	// Reaching the overall deadline without a SignalR completion frame is
	// an incomplete upstream response. Do not return accumulated deltas as if
	// they were a successful, finished answer.
	return Result{}, fmt.Errorf("chathub response deadline exceeded before completion")
}

func buildWSURL(acc Account, sessionID, conversationID, requestID string) (string, error) {
	q := url.Values{}
	q.Set("chatsessionid", requestID)
	q.Set("clientrequestid", requestID)
	q.Set("X-SessionId", sessionID)
	q.Set("ConversationId", conversationID)
	q.Set("access_token", acc.AccessToken)
	q.Set("variants", variants)
	// source must keep quotes like the browser probe
	q.Set("source", `"officeweb"`)
	q.Set("product", "Office")
	q.Set("agentHost", "Bizchat.FullScreen")
	q.Set("licenseType", "Starter")
	q.Set("agent", "web")
	q.Set("scenario", "OfficeWebIncludedCopilot")

	// url.Values encodes quotes; probe used safe='",' so keep quotes unescaped-ish.
	// Gorilla/url will encode " to %22 which MS accepts.
	u := fmt.Sprintf("%s/%s@%s?%s", wsBase, acc.OID, acc.TID, q.Encode())
	return u, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// uploadAttachments uploads image attachments to the enterprise Copilot
// UploadFile endpoint and records the returned docId for message annotations.
// Only image attachments with a real source are uploaded; failures are logged
// and the attachment is left without DocID so the request can still proceed
// through the legacy imageUrl/imageBase64 injection path.
func (c *Client) uploadAttachments(ctx context.Context, acc Account, conversationID string, attachments []Attachment) error {
	for i := range attachments {
		a := &attachments[i]
		if a.Type != "image" {
			continue
		}
		imageData := a.URL
		if !strings.HasPrefix(imageData, "data:") {
			client, err := proxy.HTTPClientFor(acc.Proxy)
			if err != nil {
				client = c.HTTPClient()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				log.Printf("[upload] fetch request error: %v", err)
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("[upload] fetch error: %v", err)
				continue
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				log.Printf("[upload] fetch status/read: %d %v", resp.StatusCode, err)
				continue
			}
			mimeType := resp.Header.Get("Content-Type")
			if mimeType == "" {
				mimeType = "image/png"
			}
			imageData = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
		comma := strings.IndexByte(imageData, ',')
		if comma < 0 {
			return fmt.Errorf("invalid image data URL")
		}
		encoded := imageData[comma+1:]
		if !strings.Contains(strings.ToLower(imageData[:comma]), ";base64") {
			return fmt.Errorf("image URL is not base64")
		}
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return fmt.Errorf("decode image: %w", err)
		}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("scenario", "UploadImage")
		_ = mw.WriteField("conversationId", conversationID)
		// The browser sends the complete data URL in FileBase64, including the
		// media-type prefix. UploadFile accepts this form and returns docId.
		_ = mw.WriteField("FileBase64", imageData)
		_ = mw.WriteField("optionsSets", "cwcgptvsan")
		_ = mw.WriteField("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch")
		_ = mw.WriteField("optionsSets", "gptvnorm2048")
		if err := mw.Close(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://substrate.office.com/m365Copilot/UploadFile", &body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if acc.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		}
		req.Header.Set("Accept", "application/json")
		// Required by the enterprise Copilot UploadFile image-input path.
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
		if acc.OID != "" && acc.TID != "" {
			req.Header.Set("X-AnchorMailbox", "Oid:"+acc.OID+"@"+acc.TID)
		}
		for k, vv := range c.HTTPHeader {
			for _, v := range vv {
				if k != "Origin" || v != "" {
					req.Header.Add(k, v)
				}
			}
		}
		client, err := proxy.HTTPClientFor(acc.Proxy)
		if err != nil {
			client = c.HTTPClient()
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[upload] http error: %v", err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			log.Printf("[upload] read error: %v", readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("[upload] status %s: %s", resp.Status, strings.TrimSpace(string(data[:minInt(len(data), 500)])))
			continue
		}
		var out struct {
			DocID    string `json:"docId"`
			FileName string `json:"fileName"`
			FileType string `json:"fileType"`
			Result   struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			log.Printf("[upload] json error: %v", err)
			continue
		}
		if out.Result.Value != "Success" || out.DocID == "" {
			log.Printf("[upload] failed: %s", strings.TrimSpace(string(data)))
			continue
		}
		a.DocID = out.DocID
		a.FileType = strings.TrimPrefix(strings.ToLower(out.FileType), ".")
		// ChatHub's ImageFile annotation uses jpg for JPEG uploads.
		if a.FileType == "jpeg" {
			a.FileType = "jpg"
		}
		if a.Name == "" {
			a.Name = out.FileName
		}
		log.Printf("[upload] success doc_id=%s file=%s type=%s", a.DocID, a.Name, a.FileType)
	}
	return nil
}

// filterEchoedImages drops URLs that were part of the request attachments:
// ChatHub echoes the injected imageUrl back into update frames, and treating
// that echo as an output image would corrupt text responses with image parts.
func filterEchoedImages(images []string, attachments []Attachment) []string {
	if len(attachments) == 0 {
		return images
	}
	echoed := make(map[string]bool, len(attachments))
	for _, a := range attachments {
		if a.URL != "" {
			echoed[a.URL] = true
		}
	}
	out := images[:0]
	for _, u := range images {
		if !echoed[u] {
			out = append(out, u)
		}
	}
	return out
}

func chatPayload(text, sessionID, conversationID, requestID, tone string, firstTurn bool, attachments []Attachment, tools []Tool, toolChoice any) string {
	text = toolProtocolPrompt(text, tools, toolChoice)
	message := map[string]any{
		"author":      "user",
		"attachments": attachments,
		"inputMethod": "Keyboard",
		"text":        text,
		"requestId":   requestID,
		"locationInfo": map[string]any{
			"timeZoneOffset": 8,
			"timeZone":       "Asia/Shanghai",
		},
		"locale":         "en-US",
		"messageType":    "Chat",
		"experienceType": "Default",
	}
	// Uploaded images are referenced through the same ImageFile annotation the
	// browser sends after the file has been uploaded by Office.
	annotations := make([]any, 0, len(attachments))
	for _, a := range attachments {
		if a.Type != "image" || a.DocID == "" {
			continue
		}
		if a.Name == "" {
			a.Name = "image." + a.FileType
		}
		fileType := a.FileType
		if fileType == "" {
			fileType = strings.TrimPrefix(strings.ToLower(a.MimeType), "image/")
		}
		if fileType == "" || fileType == "image" || fileType == "*" {
			fileType = "jpg"
		}
		annotations = append(annotations, map[string]any{
			"id": a.DocID,
			"messageAnnotationMetadata": map[string]any{
				"@type": "File", "annotationType": "File",
				"fileType": fileType, "fileName": a.Name,
			},
			"messageAnnotationType": "ImageFile",
		})
	}
	if len(annotations) > 0 {
		message["messageAnnotations"] = annotations
		message["connectedFederatedConnections"] = []string{"dummyId"}
	}
	// Legacy multimodal injection: merge imageUrl/imageBase64 directly into
	// message rather than relying solely on the attachments array. Copilot
	// vision currently requires one of these two fields to see the image.
	for _, a := range attachments {
		if a.Type != "image" || a.URL == "" {
			continue
		}
		if strings.HasPrefix(a.URL, "data:") {
			if comma := strings.IndexByte(a.URL, ','); comma >= 0 && comma+1 < len(a.URL) {
				message["imageBase64"] = a.URL[comma+1:]
			}
		} else {
			message["imageUrl"] = a.URL
		}
		break
	}
	// Vision-related options sets mirrored from the verified browser flow.
	optionsSets := []any{
		"cwc_flux_image",
		"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
		"gptvnorm2048",
		"cwc_fileupload_odb",
	}
	chat := map[string]any{
		"arguments": []any{
			map[string]any{
				"source":              "officeweb",
				"clientCorrelationId": uuid.NewString(),
				"sessionId":           sessionID,
				"optionsSets":         optionsSets,
				"options":             map[string]any{},
				"allowedMessageTypes": []string{
					"Chat", "EndOfRequest",
				},
				"sliceIds":          []any{},
				"threadLevelGptId":  map[string]any{},
				"conversationId":    conversationID,
				"traceId":           uuid.NewString(),
				"isStartOfSession":  firstTurn,
				"productThreadType": "Office",
				"clientInfo": map[string]any{
					"clientPlatform": "mcmcopilot-web",
					"clientAppName":  "Office",
				},
				"tone":          tone,
				"streamingMode": "ConciseWithPadding",
				"message":       message,
				"plugins":       clientPlugins(tools),
				"toolChoice":    toolChoice,
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
	metrics := map[string]any{
		"arguments": []any{
			map[string]any{
				"Timestamps": map[string]string{
					"ConnectionStart":       "",
					"UserInputStart":        "",
					"ConnectionEstablished": "",
					"UserInputSubmit":       "",
				},
			},
		},
		"target": "Metrics",
		"type":   1,
	}
	b1, _ := json.Marshal(chat)
	b2, _ := json.Marshal(metrics)
	return string(b1) + rs + string(b2) + rs
}
