package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// excelBPSReplay may be backed by the shared runtime cache; it serves only
// conversations named by a session header. excelBPSLocalReplay serves every
// other scope and never leaves this process, because content-, cache-key- and
// API-key-derived scopes can be shared by unrelated conversations.
var (
	excelBPSReplay      basispoints.ReplayCache
	excelBPSLocalReplay basispoints.ReplayCache
)

// excelBPSConversationHeaders name one conversation, unlike Idempotency-Key
// (one request) or affinity headers (a routing group).
var excelBPSConversationHeaders = []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "X-Session-Id"}

// excelBPSConversationScoped reports whether the BPS replay scope identifies a
// single conversation: a session header set it and no downstream affinity
// header replaced it.
func excelBPSConversationScoped(headers http.Header, identity requestSessionIdentity) bool {
	if identity.hasDownstreamAffinity {
		return false
	}
	for _, key := range excelBPSConversationHeaders {
		if strings.TrimSpace(headers.Get(key)) != "" {
			return true
		}
	}
	return false
}

// excelBPSDo is kept as a narrow seam for focused adapter tests. Production
// requests use the account-isolated Codex transport and the account proxy.
var excelBPSDo = func(req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	client := *getPooledClient(account, proxyURL)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client.Do(req)
}

type excelBPSHTTPError struct {
	status int
	code   string // Provider code only; never retain the upstream message or body.
}

func (e *excelBPSHTTPError) Error() string {
	if e == nil {
		return "Basispoints upstream request failed"
	}
	return fmt.Sprintf("Basispoints upstream returned HTTP %d", e.status)
}

type excelBPSFailure struct {
	status int
	code   string
	// detail is a local validation reason. It never carries upstream bodies or
	// credentials, so it is safe to return to the caller that sent the request.
	detail string
}

func (e *excelBPSFailure) Error() string {
	if e == nil || e.code == "" {
		return "Basispoints request failed"
	}
	return "Basispoints request failed: " + e.code
}

type excelBPSUpstream struct {
	response *http.Response
	bridge   *basispoints.Bridge
	model    string
}

// ExcelBPSResponse is the response returned by ExecuteExcelBPSRequest. The
// caller owns Response.Body and must close it after consuming Bridge.Stream.
type ExcelBPSResponse struct {
	Response *http.Response
	Bridge   *basispoints.Bridge
	Model    string
}

// excelBPSClientHeaders identify the request as the Excel add-in on desktop
// Office, matching what the add-in sends so backend features gated on the
// client (attachments, image input) behave the same.
var excelBPSClientHeaders = [][2]string{
	{"X-Basispoints-Auth-Mode", "chatgpt"},
	{"X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin"},
	{"X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Editor", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Host", "office"},
	{"X-Openai-Internal-Basispoints-Client-Platform", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Platform-Class", "PC"},
	{"X-Openai-Internal-Basispoints-Client-Runtime", "desktop"},
	{"X-Openai-Internal-Basispoints-Office-Host", "Excel"},
	{"X-Openai-Internal-Basispoints-Office-Platform", "PC"},
	{"X-Stainless-Arch", "unknown"},
	{"X-Stainless-Lang", "js"},
	{"X-Stainless-Os", "Unknown"},
	{"X-Stainless-Package-Version", "6.31.0"},
	{"X-Stainless-Retry-Count", "0"},
	{"X-Stainless-Runtime", "browser:chrome"},
}

// setExcelBPSHeaders applies the account credentials and the Excel add-in
// client identity shared by Responses and attachment requests. Callers set
// Content-Type and Accept for their own body.
func setExcelBPSHeaders(req *http.Request, token, accountID string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("X-Openai-Account-Id", accountID)
	req.Header.Set("Origin", "https://bps.openai.com")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	for _, header := range excelBPSClientHeaders {
		req.Header.Set(header[0], header[1])
	}
}

const (
	excelBPSReplayNamespace = "excel_bps_replay"
	// Long enough for `codex resume` on a conversation from last week, short
	// enough that abandoned threads do not accumulate in Redis.
	excelBPSReplayTTL = 7 * 24 * time.Hour
	// Each lookup is bounded tightly; ReplayCache also caps the whole
	// prefetch and pauses the store after a failure.
	excelBPSReplayTimeout = 200 * time.Millisecond
)

// excelBPSReplayBacking keeps BPS native tool items in the shared runtime
// cache, so a restart or another instance replays them exactly.
type excelBPSReplayBacking struct {
	cache cache.TokenCache
}

func excelBPSReplayKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Load reads one replay record under the hashed key. A miss returns false with
// no error; cache errors are returned so ReplayCache can pause the backing.
func (b excelBPSReplayBacking) Load(key string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), excelBPSReplayTimeout)
	defer cancel()
	raw, ok, err := b.cache.GetRuntime(ctx, excelBPSReplayNamespace, excelBPSReplayKey(key))
	return raw, ok, err
}

// Store writes one replay record with excelBPSReplayTTL. It runs on
// ReplayCache's background writer, never on the request path.
func (b excelBPSReplayBacking) Store(key string, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), excelBPSReplayTimeout)
	defer cancel()
	return b.cache.SetRuntime(ctx, excelBPSReplayNamespace, excelBPSReplayKey(key), value, excelBPSReplayTTL)
}

// ConfigureExcelBPSReplay attaches the process-wide BPS replay cache to the
// shared runtime cache. It is called once at startup; a process-local memory
// cache adds nothing over the in-process LRU, so it leaves replay local.
func ConfigureExcelBPSReplay(tc cache.TokenCache) {
	if tc == nil || !tc.SharedAcrossInstances() {
		excelBPSReplay.SetBacking(nil)
		return
	}
	excelBPSReplay.SetBacking(excelBPSReplayBacking{cache: tc})
}

func newExcelBPSRequest(ctx context.Context, body []byte, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setExcelBPSHeaders(req, token, accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// excelBPSAttachmentCache maps (account, image digest) to an uploaded file ID
// so images repeated in conversation history are uploaded once.
type excelBPSAttachmentCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

type excelBPSAttachment struct {
	key    string
	fileID string
}

const excelBPSAttachmentCacheSize = 256

func (c *excelBPSAttachmentCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return ""
	}
	c.order.MoveToBack(entry)
	return entry.Value.(excelBPSAttachment).fileID
}

func (c *excelBPSAttachmentCache) put(key, fileID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if old := c.entries[key]; old != nil {
		c.order.Remove(old)
	}
	c.entries[key] = c.order.PushBack(excelBPSAttachment{key: key, fileID: fileID})
	for len(c.entries) > excelBPSAttachmentCacheSize {
		oldest := c.order.Front()
		delete(c.entries, oldest.Value.(excelBPSAttachment).key)
		c.order.Remove(oldest)
	}
}

func (c *excelBPSAttachmentCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.order.Init()
}

// forget drops cached uploads the upstream may no longer honour.
func (c *excelBPSAttachmentCache) forget(fileIDs map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if fileIDs[entry.Value.(excelBPSAttachment).fileID] {
			delete(c.entries, key)
			c.order.Remove(entry)
		}
	}
}

var excelBPSAttachments excelBPSAttachmentCache

var excelBPSImageExtensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// uploadExcelBPSImage uploads one image the way the Excel add-in's "Upload
// file" button does and returns the OpenAI file ID the Responses body names.
// mediaType has been validated as a plain image/* token by basispoints.
func uploadExcelBPSImage(ctx context.Context, account *auth.Account, proxyURL, token, accountID, mediaType string, data []byte, digest string) (string, error) {
	extension := excelBPSImageExtensions[mediaType]
	if extension == "" {
		extension = "png"
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="picture-%s.%s"`, digest[:12], extension))
	header.Set("Content-Type", mediaType)
	part, err := form.CreatePart(header)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.AttachmentsURL, &body)
	if err != nil {
		return "", err
	}
	setExcelBPSHeaders(req, token, accountID)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	response, err := excelBPSDo(req, account, proxyURL)
	if err != nil {
		return "", fmt.Errorf("attachment upload failed: %w", err)
	}
	if response == nil {
		return "", errors.New("attachment upload returned no response")
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		log.Printf("[excel-bps] account=%d attachment upload HTTP %d: %s", account.ID(), response.StatusCode, excelBPSErrorShape(payload))
		return "", fmt.Errorf("attachment upload returned HTTP %d", response.StatusCode)
	}
	fileID := strings.TrimSpace(gjson.GetBytes(payload, "openai_file_id").String())
	if fileID == "" {
		return "", errors.New("attachment upload returned no file ID")
	}
	log.Printf("[excel-bps] account=%d uploaded %d KB image as %s", account.ID(), max(1, len(data)>>10), fileID)
	return fileID, nil
}

// excelBPSErrorShape summarizes a provider error body for operator logs with
// its code, type and size only; provider messages can echo request content.
func excelBPSErrorShape(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return fmt.Sprintf("non-JSON body bytes=%d", len(body))
	}
	return fmt.Sprintf("%s bytes=%d", basispoints.TerminalErrorShape(payload), len(body))
}

// excelBPSImageRefusal reports whether a rejected request may have failed
// because of its images. BPS answers invalid image placement with a bare 422
// schema error; a 400 counts only when it names images or files, so unrelated
// 400s (context length, bad fields) are not retried with re-uploads.
func excelBPSImageRefusal(status int, body []byte) bool {
	if status == http.StatusUnprocessableEntity {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String() + " " + gjson.GetBytes(body, "detail").String())
	return strings.Contains(message, "image") || strings.Contains(message, "file") || strings.Contains(message, "attachment")
}

// nextExcelBPSImageMode picks the recovery step after BPS refuses a body with
// images: re-upload stale or inline images first, then drop them entirely.
func nextExcelBPSImageMode(report basispoints.ImageReport, uploaded map[string]bool) (basispoints.ImageMode, map[string]bool) {
	reused := make(map[string]bool)
	for _, fileID := range report.FileIDs {
		if !uploaded[fileID] {
			reused[fileID] = true
		}
	}
	// Fresh uploads always follow a re-upload, so this cannot repeat forever.
	if report.Inline || len(reused) > 0 {
		return basispoints.ImagesUploadAll, reused
	}
	return basispoints.ImagesOmit, reused
}

func excelBPSAccountID(account *auth.Account, token string) string {
	if account != nil {
		if id := strings.TrimSpace(account.EffectiveAccountID()); id != "" {
			return id
		}
	}
	if claims := auth.ParseAccessToken(token); claims != nil {
		return strings.TrimSpace(claims.ChatGPTAccountID)
	}
	return ""
}

func setExcelBPSPromptCacheKey(raw []byte, threadKey string, compact bool) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil || source == nil {
		return nil, errors.New("invalid Responses request")
	}
	if compact {
		var input []any
		switch value := source["input"].(type) {
		case []any:
			input = append(input, value...)
		case string:
			input = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": value}}}}
		default:
			return nil, errors.New("compact request input is missing")
		}
		input = append(input, map[string]any{"type": "compaction_trigger"})
		source["input"] = input
		source["tool_choice"] = "none"
	}
	if strings.TrimSpace(threadKey) != "" {
		source["prompt_cache_key"] = threadKey
	}
	return json.Marshal(source)
}

func prepareExcelBPSUpstream(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, persistReplay bool) (*excelBPSUpstream, error) {
	if account == nil || !account.IsExcelBPSEnabled() {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "disabled"}
	}
	token := account.GetAccessToken()
	if token == "" {
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "auth_unavailable"}
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "account_identity_missing"}
	}
	// Requests that need image generation go to native Codex before this
	// point, so an image tool left here (typically injected by the gateway) is
	// unusable; drop it instead of adding a hosted-tool warning to the prompt.
	raw = stripResponsesImageGenerationTool(raw)
	preparedInput, err := setExcelBPSPromptCacheKey(raw, threadKey, compact)
	if err != nil {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_invalid", detail: err.Error()}
	}
	replay := &excelBPSLocalReplay
	if persistReplay {
		replay = &excelBPSReplay
	}
	prepared, bridge, err := basispoints.Prepare(preparedInput, scope, replay)
	if err != nil {
		log.Printf("[excel-bps] account=%d prepare rejected: %v", account.ID(), err)
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_unsupported", detail: err.Error()}
	}
	uploaded := make(map[string]bool)
	upload := func(image basispoints.InlineImage) (string, error) {
		key := fmt.Sprintf("%d\x00%s", account.ID(), image.Digest)
		if fileID := excelBPSAttachments.get(key); fileID != "" {
			return fileID, nil
		}
		mediaType, data, err := image.Decode()
		if err != nil {
			return "", err
		}
		fileID, err := uploadExcelBPSImage(ctx, account, proxyURL, token, accountID, mediaType, data, image.Digest)
		if err != nil {
			return "", err
		}
		excelBPSAttachments.put(key, fileID)
		uploaded[fileID] = true
		return fileID, nil
	}
	mode := basispoints.ImagesDefault
	encryptedRetried := false
	for {
		body, images, err := basispoints.RewriteImages(prepared, mode, upload)
		if err != nil {
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "request_build_failed"}
		}
		if err := images.CurrentUploadErr; err != nil {
			log.Printf("[excel-bps] account=%d image in the latest turn was not uploaded: %v", account.ID(), err)
			// Invalid-image messages name only the problem, so they are safe
			// and useful to return to the client.
			if errors.Is(err, basispoints.ErrInvalidImage) {
				return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_unsupported", detail: err.Error()}
			}
			// Upload errors can name the account proxy, so the client gets a
			// generic message while the log keeps the cause.
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "image_upload_failed"}
		}
		if images.UploadErr != nil {
			log.Printf("[excel-bps] account=%d history image upload failed, sending a note instead: %v", account.ID(), images.UploadErr)
		}
		if images.InputErr != nil {
			log.Printf("[excel-bps] account=%d history image is invalid, sending a note instead: %v", account.ID(), images.InputErr)
		}
		request, err := newExcelBPSRequest(ctx, body, token, accountID)
		if err != nil {
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "request_build_failed"}
		}
		response, err := excelBPSDo(request, account, proxyURL)
		if err != nil {
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "transport_error"}
		}
		if response == nil {
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "empty_response"}
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return &excelBPSUpstream{response: response, bridge: bridge, model: gjson.GetBytes(preparedInput, "model").String()}, nil
		}
		// The body stays out of the client response; operators still need the
		// provider reason to tell unsupported input from account problems.
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		_ = response.Body.Close()
		log.Printf("[excel-bps] account=%d upstream HTTP %d: %s", account.ID(), response.StatusCode, excelBPSErrorShape(snippet))
		invalidEncrypted := response.StatusCode == http.StatusBadRequest && isExcelBPSInvalidEncryptedContent(snippet)
		if invalidEncrypted && !encryptedRetried {
			// Reasoning produced elsewhere (for example by native Codex before a
			// fallback) cannot be verified here. Retry once without it; nothing
			// was generated for the rejected request.
			if stripped, ok := basispoints.StripReasoningEncryptedContent(prepared); ok {
				encryptedRetried = true
				prepared = stripped
				log.Printf("[excel-bps] account=%d retrying once without unverifiable encrypted reasoning", account.ID())
				continue
			}
		}
		if !excelBPSImageRefusal(response.StatusCode, snippet) || !images.Any() || mode == basispoints.ImagesOmit {
			code := gjson.GetBytes(snippet, "error.code").String()
			if invalidEncrypted {
				code = "invalid_encrypted_content"
			}
			excelBPSHealth.observeFailure(ctx, account, gjson.GetBytes(preparedInput, "model").String(), response.StatusCode, code, response.Header, proxyURL)
			return nil, &excelBPSHTTPError{status: response.StatusCode, code: code}
		}
		next, stale := nextExcelBPSImageMode(images, uploaded)
		excelBPSAttachments.forget(stale)
		log.Printf("[excel-bps] account=%d upstream refused images (inline=%t file_ids=%d stale=%d); retrying with image mode %d", account.ID(), images.Inline, len(images.FileIDs), len(stale), next)
		mode = next
	}
}

// ExecuteExcelBPSRequest sends one prepared BPS request for account-test code
// and other non-handler callers. It intentionally exposes only the response
// stream and bridge; credentials and wire construction remain private.
func ExecuteExcelBPSRequest(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact bool) (*ExcelBPSResponse, error) {
	// Account tests use synthetic threads; their replay stays in memory.
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact, false)
	if err != nil {
		return nil, err
	}
	return &ExcelBPSResponse{Response: upstream.response, Bridge: upstream.bridge, Model: upstream.model}, nil
}

type excelBPSResult struct {
	StatusCode       int
	Terminal         string
	ResponseID       string
	Model            string
	UpstreamModel    string
	RequestID        string
	DurationMs       int
	FirstTokenMs     int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	CachedTokens     int
	ClientDisconnect bool
	// Synthesized marks a completion rebuilt after the upstream closed early;
	// it has no usage, so its token counts are unknown rather than zero.
	Synthesized bool
	// Completed is the translated response.completed event data, kept so the
	// caller can index the turn for later previous_response_id expansion.
	Completed []byte
	// Effort is the reasoning tier Basispoints actually ran (for example max
	// is sent as xhigh), recorded instead of the requested tier.
	Effort string
}

func (r *excelBPSResult) usageFrom(payload []byte) {
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() {
		return
	}
	r.ResponseID = response.Get("id").String()
	r.UpstreamModel = response.Get("model").String()
	usage := response.Get("usage")
	r.PromptTokens = int(usage.Get("input_tokens").Int())
	r.CompletionTokens = int(usage.Get("output_tokens").Int())
	r.TotalTokens = int(usage.Get("total_tokens").Int())
	r.ReasoningTokens = int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	r.CachedTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
}

func excelBPSTerminal(kind string) bool {
	switch kind {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func writeExcelBPSFrame(w io.Writer, event string, data []byte) error {
	if event == "" {
		event = gjson.GetBytes(data, "type").String()
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// forwardExcelBPS writes exactly one terminal outcome. BPS is always requested
// upstream as SSE, while non-stream Responses callers receive the completed
// response object after the terminal event is validated.
func forwardExcelBPS(ctx context.Context, c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream, persistReplay bool) (excelBPSResult, error) {
	start := time.Now()
	result := excelBPSResult{}
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact, persistReplay)
	if err != nil {
		return result, err
	}
	defer upstream.response.Body.Close()
	result.StatusCode = upstream.response.StatusCode
	result.Model = upstream.model
	result.RequestID = upstream.response.Header.Get("x-request-id")
	result.Effort = upstream.bridge.Effort
	converted := upstream.bridge.Stream(upstream.response.Body)
	defer converted.Close()
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	flusher, _ := c.Writer.(http.Flusher)
	var completed []byte
	var writeErr error
	terminalSeen := false
	readErr := ReadSSEStreamWithEvent(converted, func(event string, data []byte) bool {
		if terminalSeen {
			return false
		}
		kind := gjson.GetBytes(data, "type").String()
		if result.FirstTokenMs == 0 && (kind == "response.output_text.delta" || kind == "response.output_item.added") {
			result.FirstTokenMs = int(time.Since(start).Milliseconds())
		}
		if excelBPSTerminal(kind) {
			terminalSeen = true
			result.Terminal = kind
			if kind == "response.completed" {
				status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
				if status == "failed" || status == "incomplete" {
					result.Terminal = "response." + status
				}
			}
			result.usageFrom(data)
			if result.Terminal == "response.completed" {
				result.Completed = append([]byte(nil), data...)
			}
			if !stream {
				completed = append(completed[:0], data...)
				return false
			}
		}
		if stream {
			writeErr = writeExcelBPSFrame(c.Writer, event, data)
			if writeErr == nil && flusher != nil {
				flusher.Flush()
			}
			if writeErr != nil {
				result.ClientDisconnect = true
				return false
			}
		}
		return !terminalSeen
	})
	result.DurationMs = int(time.Since(start).Milliseconds())
	result.Synthesized = upstream.bridge.SynthesizedCompletion()
	if writeErr != nil {
		return result, writeErr
	}
	if ctx.Err() != nil {
		result.ClientDisconnect = true
		return result, ctx.Err()
	}
	if readErr != nil {
		return result, readErr
	}
	if !terminalSeen {
		return result, errors.New("Basispoints stream ended before a terminal event")
	}
	if !stream {
		if result.Terminal != "response.completed" {
			return result, errors.New("Basispoints response did not complete")
		}
		response := gjson.GetBytes(completed, "response")
		if !response.Exists() {
			return result, errors.New("Basispoints response did not contain a completed response")
		}
		c.Data(http.StatusOK, "application/json", []byte(response.Raw))
	}
	return result, nil
}

func excelBPSFailureInfo(err error) (int, string, string) {
	var upstream *excelBPSHTTPError
	if errors.As(err, &upstream) {
		status := upstream.status
		if status >= http.StatusInternalServerError {
			status = http.StatusBadGateway
		}
		return status, "basispoints_upstream_error", "Basispoints upstream rejected the request"
	}
	var failure *excelBPSFailure
	if errors.As(err, &failure) && failure != nil {
		status := failure.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		switch failure.code {
		case "request_invalid", "request_unsupported":
			message := "Basispoints does not support this request"
			if failure.detail != "" {
				message += ": " + failure.detail
			}
			return status, "basispoints_request_invalid", message
		case "image_upload_failed":
			return status, "basispoints_image_upload_failed", "Basispoints could not upload an image from the latest turn; retry the request"
		case "account_identity_missing":
			return status, "basispoints_account_id_missing", "The selected account has no Basispoints identity"
		case "auth_unavailable":
			return status, "basispoints_auth_unavailable", "The selected account OAuth credential is unavailable"
		default:
			return status, "basispoints_transport_error", "Basispoints connection failed"
		}
	}
	return http.StatusBadGateway, "basispoints_transport_error", "Basispoints connection failed"
}

func writeExcelBPSFailure(c *gin.Context, stream bool, status int, code, message string) {
	if c == nil {
		return
	}
	if !stream || !c.Writer.Written() {
		c.JSON(status, gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed", "output": []any{},
			"error": map[string]string{"code": code, "message": message},
		},
	})
	_ = writeExcelBPSFrame(c.Writer, "response.failed", payload)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

const excelBPSNativeFallbackKey = "codex2api.excel_bps_native_fallback"

// excelBPSNativeRequestReason identifies opaque client context which the Excel
// schema cannot represent. Plaintext agent tasks are normalized by the bridge;
// genuinely opaque context is preserved and routed to native Codex.
func excelBPSNativeRequestReason(raw []byte) string {
	if gjson.GetBytes(raw, "previous_response_id").String() != "" {
		return "stored_response"
	}
	if reason := excelBPSImageIntentReason(raw); reason != "" {
		return reason
	}
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		isAgent := item.Get("type").String() == "agent_message"
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("type").String() == "encrypted_content" {
					if isAgent && field == "content" && !part.Get("text").Exists() && basispoints.IsPlaintextAgentContent(part.Get("encrypted_content").String()) {
						continue
					}
					if isAgent {
						return "agent_context"
					}
					return "opaque_context"
				}
			}
		}
	}
	return ""
}

// Only retry before anything has been sent to the client. Authentication errors,
// generic forbidden responses and failures after partial output stay visible.
func excelBPSNativeFailureReason(err error) string {
	var upstream *excelBPSHTTPError
	if errors.As(err, &upstream) {
		if upstream.status >= 500 && upstream.status <= 599 {
			return "upstream_5xx"
		}
		if upstream.status == http.StatusForbidden && upstream.code == "basispoints_model_access_changed" {
			return "model_access"
		}
		// Encrypted context that Basispoints still cannot verify after one
		// retry belongs to native Codex, which can read its own reasoning.
		if upstream.status == http.StatusBadRequest && upstream.code == "invalid_encrypted_content" {
			return "encrypted_context"
		}
		// Basispoints throttles its own endpoint; native Codex capacity is separate.
		if upstream.status == http.StatusTooManyRequests {
			return "rate_limited"
		}
		// Native Codex owns token refresh and account auth state; let it see
		// the same credential instead of duplicating that handling here.
		if upstream.status == http.StatusUnauthorized {
			return "auth"
		}
	}
	var failure *excelBPSFailure
	if errors.As(err, &failure) {
		switch failure.code {
		case "request_unsupported":
			return "unsupported_request"
		case "transport_error", "empty_response":
			return "transport_error"
		}
	}
	return ""
}

func markExcelBPSNativeFallback(c *gin.Context, account *auth.Account, reason string) {
	c.Set(excelBPSNativeFallbackKey, reason)
	c.Header("X-Codex2api-Upstream-Fallback", "basispoints-to-codex")
	log.Printf("[excel-bps] account=%d native fallback reason=%s before_output=true", account.ID(), reason)
}

// handleExcelBPS returns false when the normal Codex handler must continue with
// the same acquired account and original body. It must not release that account
// or write a response on fallback. The context marker prevents BPS retry loops.

// It deliberately does not report provider failures to the account scheduler:
// BPS is an opt-in alternate provider surface, not a Codex health probe.
func (h *Handler) handleExcelBPS(c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream, persistReplay bool, endpoint, logModel, effectiveModel, reasoningEffort string, affinityKey string, affinityGuard auth.SessionAffinityGuard, start time.Time, onCompleted func(completed []byte)) bool {
	if c.GetString(excelBPSNativeFallbackKey) != "" {
		return false
	}
	if c.Request.Context().Err() == nil && !c.Writer.Written() {
		if reason := excelBPSNativeRequestReason(raw); reason != "" {
			markExcelBPSNativeFallback(c, account, reason)
			return false
		}
	}
	result, err := forwardExcelBPS(c.Request.Context(), c, account, raw, scope, threadKey, proxyURL, compact, stream, persistReplay)
	if result.DurationMs == 0 && !start.IsZero() {
		result.DurationMs = int(max(int64(0), time.Since(start).Milliseconds()))
	}
	statusCode := result.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	logInput := &database.UsageLogInput{
		AccountID: account.ID(), Model: logModel, EffectiveModel: effectiveModel,
		Endpoint: endpoint, InboundEndpoint: endpoint, UpstreamEndpoint: basispoints.ResponsesURL,
		StatusCode: statusCode, DurationMs: result.DurationMs, FirstTokenMs: result.FirstTokenMs,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		TotalTokens: result.TotalTokens, InputTokens: result.PromptTokens, OutputTokens: result.CompletionTokens,
		ReasoningTokens: result.ReasoningTokens, CachedTokens: result.CachedTokens,
		ReasoningEffort: reasoningEffort, Stream: stream, Compact: compact,
		RequestID: result.RequestID, UpstreamResponseModel: result.UpstreamModel,
	}
	if result.Effort != "" {
		logInput.ReasoningEffort = result.Effort
	}
	if err != nil {
		status, code, message := excelBPSFailureInfo(err)
		logInput.StatusCode = status
		logInput.UpstreamErrorKind = code
		logInput.ErrorMessage = message
		if reason := excelBPSNativeFailureReason(err); reason != "" && !result.ClientDisconnect && c.Request.Context().Err() == nil && !c.Writer.Written() {
			markExcelBPSNativeFallback(c, account, reason)
			logInput.IsRetryAttempt = true
			if h != nil {
				h.logUsageForRequest(c, logInput)
			}
			return false
		}
		if !result.ClientDisconnect {
			writeExcelBPSFailure(c, stream, status, code, message)
		}
	}
	if result.Synthesized && logInput.UpstreamErrorKind == "" {
		// Status stays 200; the kind makes the missing usage visible in logs.
		logInput.UpstreamErrorKind = "basispoints_cutoff_completed"
	}
	if result.Terminal != "response.completed" && result.Terminal != "" && logInput.ErrorMessage == "" {
		logInput.UpstreamErrorKind = "basispoints_terminal_" + strings.TrimPrefix(result.Terminal, "response.")
		logInput.ErrorMessage = "Basispoints returned a non-completed terminal event"
	}
	if h != nil {
		h.logUsageForRequest(c, logInput)
	}
	if err == nil && result.Terminal == "response.completed" {
		if onCompleted != nil && len(result.Completed) > 0 {
			onCompleted(result.Completed)
		}
		if h != nil && h.store != nil {
			h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
		}
		return true
	}
	if h != nil && h.store != nil {
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		h.store.Release(account)
	}
	return true
}
