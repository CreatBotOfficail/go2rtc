package webrtc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
)

// Constants for message size thresholds and buffer management
const (
	MessageSizeThreshold = 32 * 1024 // 32KB threshold for chunking
	MaxSafeMessageSize   = 48 * 1024 // Maximum safe WebRTC message size
	ChunkHeaderReserved  = 1024      // Reserved space for chunk metadata (1KB)
	MaxChunkDataSize     = MaxSafeMessageSize - ChunkHeaderReserved
	BufferThreshold      = 2048 * 1024 // Buffer threshold (2MB)
)

var log zerolog.Logger

// InitLogger initializes the logger for this package
func InitLogger() {
	log = app.GetLogger("webrtc")
	log.Info().Msg("WebRTC datachannel proxy module initialized")
}

// Message represents a unified message structure for HTTP requests
type Message struct {
	ID      interface{}            `json:"id,omitempty"` // Supports string or number ID types
	Method  string                 `json:"method"`
	Path    string                 `json:"path"`
	Params  map[string]interface{} `json:"params,omitempty"`
	Headers map[string]string      `json:"headers,omitempty"`
	Body    json.RawMessage        `json:"body,omitempty"`
}

// FileMessage represents file transfer message structure
type FileMessage struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"` // file_start, file_chunk, file_end
	Index       int         `json:"index,omitempty"`
	TotalChunks int         `json:"total_chunks,omitempty"`
	Hash        string      `json:"hash,omitempty"`
	Data        string      `json:"data,omitempty"` // Base64 encoded data
	Metadata    interface{} `json:"metadata,omitempty"`
}

// FileReceiver manages incoming file chunks
type FileReceiver struct {
	ID          string
	Metadata    interface{}
	TotalChunks int
	Chunks      [][]byte
	ReceivedAt  time.Time
}

// HTTPProxyManager manages HTTP proxy operations and file transfers
type HTTPProxyManager struct {
	client        *http.Client
	fileReceivers map[string]*FileReceiver
	receiverMutex sync.Mutex
}

// HandleDataChannelProxy routes data channels to appropriate proxy handlers based on channel label
func HandleDataChannelProxy(channel *webrtc.DataChannel) {
	name := channel.Label()
	switch name {
	case "websocket":
		log.Info().Str("name", name).Msg("Connected to WebSocket")
		proxyWebSocketChannel(channel)
	case "http":
		log.Info().Str("name", name).Msg("Connected to HTTP proxy")
		proxyHTTPChannelWithChunking(channel)
	default:
		log.Warn().Str("name", name).Msg("Unknown channel type")
	}
}

// NewHTTPProxyManager creates a new HTTP proxy manager instance
func NewHTTPProxyManager() *HTTPProxyManager {
	return &HTTPProxyManager{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:       10,
				IdleConnTimeout:    30 * time.Second,
				DisableCompression: false,
			},
		},
		fileReceivers: make(map[string]*FileReceiver),
	}
}

// websocketProxy manages WebSocket connection state and lifecycle
type websocketProxy struct {
	mu           sync.Mutex
	conn         *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	channel      *webrtc.DataChannel
	wg           sync.WaitGroup
	shuttingDown bool
}

func newWebsocketProxy(channel *webrtc.DataChannel) *websocketProxy {
	ctx, cancel := context.WithCancel(context.Background())
	return &websocketProxy{
		ctx:     ctx,
		cancel:  cancel,
		channel: channel,
	}
}

// ensureConnected establishes WebSocket connection with retry logic
func (p *websocketProxy) ensureConnected() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shuttingDown {
		return fmt.Errorf("proxy is shutting down")
	}

	if p.conn != nil {
		return nil
	}

	const maxRetries = 5
	for attempts := 0; attempts < maxRetries; attempts++ {
		select {
		case <-p.ctx.Done():
			return fmt.Errorf("context canceled")
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:7125/websocket", nil)
		if err == nil {
			p.conn = conn
			log.Info().Msg("WebSocket connected successfully")

			p.wg.Add(1)
			go p.startReader()

			return nil
		}
		log.Warn().Err(err).Int("attempt", attempts+1).Msg("WebSocket connection failed")
		time.Sleep(time.Duration(attempts+1) * time.Second)
	}
	return fmt.Errorf("websocket connect failed after %d retries", maxRetries)
}

func (p *websocketProxy) sendChunkedMessage(data []byte) error {
	// If message is small enough, send directly
	if len(data) <= MessageSizeThreshold {
		return p.channel.Send(data)
	}

	// For large messages, use HTTP-style file transfer protocol
	log.Debug().Int("size", len(data)).Int("threshold", MessageSizeThreshold).Msg("Large WebSocket message detected, using file transfer protocol")

	// Try to extract id from the data
	var messageID interface{}
	var jsonData map[string]interface{}
	if err := json.Unmarshal(data, &jsonData); err == nil {
		if id, exists := jsonData["id"]; exists {
			messageID = id
		} else {
			messageID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
	} else {
		// Not JSON data, generate an id
		messageID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	fileID := getIDString(messageID)

	// Calculate chunk count using HTTP chunking parameters
	chunkSize := MaxChunkDataSize
	totalChunks := (len(data) + chunkSize - 1) / chunkSize

	// Calculate data hash for integrity verification
	hash := fmt.Sprintf("%x", sha256.Sum256(data))

	// Build metadata for WebSocket large message
	metadata := map[string]interface{}{
		"originalId":  messageID,
		"isWebSocket": true,
		"dataType":    "websocket_large_message",
	}

	log.Debug().Str("file_id", fileID).Int("total_chunks", totalChunks).Int("size", len(data)).Msg("Starting WebSocket file transfer")

	// Send file start message
	startMsg := FileMessage{
		ID:          fileID,
		Type:        "file_start",
		TotalChunks: totalChunks,
		Hash:        hash,
		Metadata:    metadata,
	}

	startBytes, err := json.Marshal(startMsg)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal WebSocket file start message")
		return err
	}
	if err := p.channel.Send(startBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send WebSocket file start message")
		return err
	}

	// Send data chunks
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}

		chunkDataBase64 := base64.StdEncoding.EncodeToString(data[start:end])
		chunkMsg := FileMessage{
			ID:    fileID,
			Type:  "file_chunk",
			Index: i,
			Data:  chunkDataBase64,
		}

		chunkBytes, err := json.Marshal(chunkMsg)
		if err != nil {
			log.Error().Err(err).Int("chunk_index", i).Msg("Failed to marshal chunk")
			return err
		}

		if err := p.channel.Send(chunkBytes); err != nil {
			log.Error().Err(err).Int("chunk_index", i).Msg("Failed to send chunk")
			return err
		}

		// Log progress for large transfers
		if i%50 == 0 || i == totalChunks-1 {
			log.Debug().Int("chunk_index", i+1).Int("total_chunks", totalChunks).Float64("progress", float64(i+1)/float64(totalChunks)*100).Msg("WebSocket message chunking progress")
		}
	}

	log.Debug().Interface("message_id", messageID).Int("total_chunks", totalChunks).Msg("Large WebSocket message sent successfully")
	return nil
}

// startReader reads from WebSocket and forwards messages to WebRTC
func (p *websocketProxy) startReader() {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			log.Debug().Msg("Context cancelled, stopping WebSocket reader")
			return
		default:
			p.mu.Lock()
			conn := p.conn
			p.mu.Unlock()

			if conn == nil {
				log.Warn().Msg("No WebSocket connection available for reader")
				return
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error().Err(err).Msg("WebSocket unexpected close error")
				} else {
					log.Debug().Err(err).Msg("WebSocket read error")
				}

				if !p.isShuttingDown() {
					log.Debug().Msg("Attempting WebSocket reconnection")
					if err := p.reconnect(); err != nil {
						log.Error().Err(err).Msg("WebSocket reconnection failed")
					}
				}
				return
			}

			if err := p.sendChunkedMessage(data); err != nil {
				log.Error().Err(err).Msg("WebRTC channel send failed")
				return
			}
		}
	}
}

// reconnect attempts to reestablish WebSocket connection
func (p *websocketProxy) reconnect() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shuttingDown {
		return fmt.Errorf("proxy is shutting down")
	}

	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}

	const maxRetries = 5
	for attempts := 0; attempts < maxRetries; attempts++ {
		select {
		case <-p.ctx.Done():
			return fmt.Errorf("context canceled")
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:7125/websocket", nil)
		if err == nil {
			p.conn = conn
			log.Info().Msg("WebSocket reconnected successfully")

			p.wg.Add(1)
			go p.startReader()

			return nil
		}
		log.Warn().Err(err).Int("attempt", attempts+1).Msg("WebSocket reconnection attempt failed")
		time.Sleep(time.Duration(attempts+1) * time.Second)
	}

	errMsg := fmt.Sprintf("WebSocket reconnection failed after %d retries", maxRetries)
	if sendErr := p.channel.SendText(errMsg); sendErr != nil {
		log.Error().Err(sendErr).Msg("Failed to send error message on data channel")
	}
	return fmt.Errorf("websocket reconnection failed after %d retries", maxRetries)
}

// forwardMessage forwards WebRTC messages to WebSocket
func (p *websocketProxy) forwardMessage(msg webrtc.DataChannelMessage) {
	if p.isShuttingDown() {
		return
	}

	if err := p.ensureConnected(); err != nil {
		log.Error().Err(err).Msg("Failed to ensure WebSocket connection")
		return
	}

	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()

	if conn == nil {
		log.Warn().Msg("No WebSocket connection available for forwarding")
		return
	}

	messageType := websocket.BinaryMessage
	if msg.IsString {
		messageType = websocket.TextMessage
	}

	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Error().Err(err).Msg("Failed to set WebSocket write deadline")
		return
	}

	if err := conn.WriteMessage(messageType, msg.Data); err != nil {
		log.Error().Err(err).Msg("WebSocket write failed")

		if !p.isShuttingDown() {
			log.Debug().Msg("Attempting WebSocket reconnection after write error")
			go func() {
				if err := p.reconnect(); err != nil {
					log.Error().Err(err).Msg("Reconnection after write error failed")
				}
			}()
		}
	}
}

// isShuttingDown safely checks if proxy is shutting down
func (p *websocketProxy) isShuttingDown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shuttingDown
}

// shutdown safely closes WebSocket connection and cancels context
func (p *websocketProxy) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.shuttingDown = true

	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
		log.Info().Msg("WebSocket connection closed due to WebRTC channel closure")
	}

	p.cancel()
}

// proxyWebSocketChannel establishes WebSocket proxy for WebRTC data channel
func proxyWebSocketChannel(channel *webrtc.DataChannel) {
	p := newWebsocketProxy(channel)
	log.Debug().Str("state", channel.ReadyState().String()).Msg("WebRTC data channel state check")

	channel.OnOpen(func() {
		log.Info().Str("state", channel.ReadyState().String()).Msg("WebRTC data channel opened")
	})

	channel.OnClose(func() {
		log.Info().Str("state", channel.ReadyState().String()).Msg("WebRTC data channel closed")
		p.shutdown()
		p.wg.Wait()
	})

	log.Debug().Msg("Registering WebRTC message handler")
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		p.forwardMessage(msg)
	})
}

// proxyHTTPChannelWithChunking handles HTTP requests with automatic chunking support
func proxyHTTPChannelWithChunking(channel *webrtc.DataChannel) {
	manager := NewHTTPProxyManager()

	log.Debug().Str("state", channel.ReadyState().String()).Msg("HTTP data channel initialized")

	channel.OnOpen(func() {
		log.Info().Str("state", channel.ReadyState().String()).Msg("HTTP data channel opened")
	})

	channel.OnClose(func() {
		log.Info().Str("state", channel.ReadyState().String()).Msg("HTTP data channel closed")
	})

	channel.SetBufferedAmountLowThreshold(BufferThreshold)
	channel.OnBufferedAmountLow(func() {
		log.Debug().Uint64("buffered_bytes", channel.BufferedAmount()).Msg("WebRTC buffer level low")
	})

	log.Debug().Msg("Registering WebRTC message handler with auto-chunking support")
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		log.Debug().Int("message_size", len(msg.Data)).Msg("Received HTTP message")
		manager.handleMessage(channel, msg.Data)
	})
}

// handleMessage processes incoming messages and routes them appropriately
func (hpm *HTTPProxyManager) handleMessage(channel *webrtc.DataChannel, data []byte) {
	var message map[string]interface{}
	if err := json.Unmarshal(data, &message); err != nil {
		log.Error().Err(err).Msg("Failed to parse incoming message")
		sendErrorResponse(channel, 400, "Invalid message format", nil)
		return
	}

	// Check for file transfer message types
	if msgType, exists := message["type"].(string); exists {
		switch msgType {
		case "file_start":
			hpm.handleFileStart(channel, message)
			return
		case "file_chunk":
			hpm.handleFileChunk(channel, message)
			return
		case "file_end":
			hpm.handleFileEnd(channel, message)
			return
		}
	}

	// Parse as HTTP request message
	var httpMsg Message
	if err := json.Unmarshal(data, &httpMsg); err != nil {
		log.Error().Err(err).Msg("Failed to parse HTTP message")
		sendErrorResponse(channel, 400, "Invalid HTTP message format", nil)
		return
	}

	// Validate required fields
	if httpMsg.Method == "" || httpMsg.Path == "" {
		log.Warn().Msg("Missing required fields (method or path)")
		sendErrorResponse(channel, 400, "Missing method or path", httpMsg.ID)
		return
	}

	// Ensure ID exists, generate random one if missing
	if httpMsg.ID == nil || getIDString(httpMsg.ID) == "" {
		httpMsg.ID = fmt.Sprintf("req_%d", time.Now().UnixNano())
		log.Debug().Interface("generated_id", httpMsg.ID).Msg("Generated random ID for request")
	}

	idStr := getIDString(httpMsg.ID)
	log.Debug().Str("method", httpMsg.Method).Str("path", httpMsg.Path).Str("id", idStr).Msg("Processing HTTP request")

	// Execute HTTP request
	responseData, status, headers, err := hpm.executeHTTPRequest(&httpMsg)
	if err != nil {
		log.Error().Err(err).Str("id", idStr).Msg("HTTP request execution failed")
		sendErrorResponse(channel, 502, "Request execution failed", httpMsg.ID)
		return
	}

	// Determine response strategy based on content type and size
	contentType := headers["Content-Type"]
	disposition := headers["Content-Disposition"]

	isFileResponse := hpm.isFileResponse(contentType) ||
		strings.Contains(strings.ToLower(disposition), "attachment")

	if isFileResponse || len(responseData) > MessageSizeThreshold {
		if isFileResponse {
			if strings.Contains(strings.ToLower(disposition), "attachment") {
				log.Debug().Str("disposition", disposition).Str("id", idStr).Msg("Detected attachment download, using file transfer")
			} else {
				log.Debug().Str("content_type", contentType).Str("id", idStr).Msg("Detected file response, using file transfer")
			}
		} else {
			log.Debug().Int("size", len(responseData)).Str("id", idStr).Msg("Large response detected, using file transfer")
		}
		hpm.sendAsFile(channel, responseData, httpMsg.ID, status, headers, isFileResponse)
	} else {
		log.Debug().Int("size", len(responseData)).Str("id", idStr).Msg("Small response, using regular message")
		hpm.sendAsMessage(channel, responseData, httpMsg.ID, status, headers)
	}
}

// executeHTTPRequest executes HTTP request and returns response data
func (hpm *HTTPProxyManager) executeHTTPRequest(msg *Message) ([]byte, int, map[string]string, error) {
	// Build complete URL
	fullURL := "http://127.0.0.1:7125" + msg.Path

	// Add query parameters
	if len(msg.Params) > 0 {
		queryParams := url.Values{}
		processParams(queryParams, "", msg.Params)
		fullURL += "?" + queryParams.Encode()
	}

	log.Debug().Str("method", msg.Method).Str("url", fullURL).Msg("Executing HTTP request")

	// Create HTTP request
	var body io.Reader
	if len(msg.Body) > 0 {
		body = bytes.NewReader(msg.Body)
	}

	httpReq, err := http.NewRequest(msg.Method, fullURL, body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to build HTTP request: %v", err)
	}

	// Set request headers
	if len(msg.Body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for key, value := range msg.Headers {
		httpReq.Header.Set(key, value)
	}

	// Send HTTP request
	resp, err := hpm.client.Do(httpReq)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to read response: %v", err)
	}

	// Build response headers
	headers := make(map[string]string)
	for key, values := range resp.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	log.Debug().Int("status_code", resp.StatusCode).Int("size", len(respData)).Msg("HTTP response received")
	return respData, resp.StatusCode, headers, nil
}

// sendAsMessage sends small data as regular message without base64 encoding
func (hpm *HTTPProxyManager) sendAsMessage(channel *webrtc.DataChannel, data []byte, requestID interface{}, status int, headers map[string]string) {
	// Check if channel is still open before sending response
	if channel.ReadyState() != webrtc.DataChannelStateOpen {
		log.Warn().Str("state", channel.ReadyState().String()).Interface("request_id", requestID).Msg("Cannot send message response: data channel is not open")
		return
	}

	// Handle response body data type without base64 encoding
	var bodyField interface{}

	if json.Valid(data) {
		bodyField = json.RawMessage(data)
	} else {
		bodyField = string(data)
	}

	// Build response
	response := map[string]interface{}{
		"id":      requestID,
		"status":  status,
		"headers": headers,
		"body":    bodyField,
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal response")
		sendErrorResponse(channel, 500, "Response marshal error", requestID)
		return
	}

	if err := channel.Send(responseBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send response")
	} else {
		log.Debug().Int("size", len(responseBytes)).Msg("Regular message response sent successfully")
	}
}

// sendAsFile sends large data using file transfer protocol with chunking
func (hpm *HTTPProxyManager) sendAsFile(channel *webrtc.DataChannel, data []byte, requestID interface{}, status int, headers map[string]string, isFileResponse bool) {
	// Use the same ID as the request to keep consistency, but convert to string for file operations
	fileID := getIDString(requestID)

	// Calculate chunk count
	chunkSize := MaxChunkDataSize
	totalChunks := (len(data) + chunkSize - 1) / chunkSize

	// Calculate data hash for integrity verification
	hash := fmt.Sprintf("%x", sha256.Sum256(data))

	// Build metadata
	metadata := map[string]interface{}{
		"requestId": requestID,
		"status":    status,
		"headers":   headers,
		"isFile":    isFileResponse,
	}

	// Add file information for file responses
	if isFileResponse {
		contentType := headers["Content-Type"]
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		filename := extractFilename(headers)
		if filename == "" {
			filename = fmt.Sprintf("download_%s", getIDString(requestID))
		}

		// Core metadata format consistent with upload files
		metadata = map[string]interface{}{
			"requestId": requestID,
			"filename":  filename,
			"filetype":  contentType,
			"size":      len(data),
			"isFile":    true,
		}

		// Optional HTTP response information for debugging
		if status != 200 {
			metadata["httpStatus"] = status
		}

		log.Debug().Str("filename", filename).Str("content_type", contentType).Int("size", len(data)).Msg("Preparing to send file")
		log.Debug().Interface("metadata", metadata).Msg("File metadata")
	}

	// Send file start message
	startMsg := FileMessage{
		ID:          fileID,
		Type:        "file_start",
		TotalChunks: totalChunks,
		Hash:        hash,
		Metadata:    metadata,
	}

	// Check if channel is still open before sending
	if channel.ReadyState() != webrtc.DataChannelStateOpen {
		log.Warn().Str("state", channel.ReadyState().String()).Str("file_id", fileID).Msg("Cannot send file start message: data channel is not open")
		return
	}

	startBytes, _ := json.Marshal(startMsg)
	if err := channel.Send(startBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send file start message")
		return
	}

	log.Debug().Str("file_id", fileID).Int("total_chunks", totalChunks).Int("size", len(data)).Msg("Starting file transfer")

	// Send data chunks
	for i := 0; i < totalChunks; i++ {
		// Check if channel is still open before sending each chunk
		if channel.ReadyState() != webrtc.DataChannelStateOpen {
			log.Warn().Str("state", channel.ReadyState().String()).Str("file_id", fileID).Int("chunk_index", i).Msg("Cannot send file chunk: data channel is not open")
			return
		}

		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}

		// Encode chunk data as base64
		chunkDataBase64 := base64.StdEncoding.EncodeToString(data[start:end])

		chunkMsg := FileMessage{
			ID:    fileID,
			Type:  "file_chunk",
			Index: i,
			Data:  chunkDataBase64,
		}

		chunkBytes, _ := json.Marshal(chunkMsg)
		if err := channel.Send(chunkBytes); err != nil {
			log.Error().Err(err).Int("chunk_index", i).Msg("Failed to send file chunk")
			return
		}

		bufferedAmount := channel.BufferedAmount()
		if bufferedAmount > BufferThreshold {
			time.Sleep(10 * time.Millisecond)
		}

		if i%100 == 0 || i == totalChunks-1 {
			log.Debug().Int("chunk_index", i+1).Int("total_chunks", totalChunks).Float64("progress", float64(i+1)/float64(totalChunks)*100).Msg("File transfer progress")
		}
	}

	// Send file end message
	endMsg := FileMessage{
		ID:   fileID,
		Type: "file_end",
		Hash: hash,
	}

	// Check if channel is still open before sending end message
	if channel.ReadyState() != webrtc.DataChannelStateOpen {
		log.Warn().Str("state", channel.ReadyState().String()).Str("file_id", fileID).Msg("Cannot send file end message: data channel is not open")
		return
	}

	endBytes, _ := json.Marshal(endMsg)
	if err := channel.Send(endBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send file end message")
	} else {
		if isFileResponse {
			log.Debug().Str("file_id", fileID).Msg("File download completed")
		} else {
			log.Debug().Str("file_id", fileID).Msg("Large data transfer completed")
		}
	}
}

// getIDString converts interface{} type ID to string representation
func getIDString(id interface{}) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', 0, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// extractFilename extracts filename from response headers with UTF-8 support
func extractFilename(headers map[string]string) string {
	disposition := headers["Content-Disposition"]
	if disposition == "" {
		return ""
	}

	// Try filename*=UTF-8''... format first (RFC 5987)
	if strings.Contains(disposition, "filename*=UTF-8''") {
		parts := strings.Split(disposition, "filename*=UTF-8''")
		if len(parts) > 1 {
			encoded := strings.Split(parts[1], ";")[0]
			encoded = strings.TrimSpace(encoded)
			// URL decode UTF-8 filename
			if decoded, err := url.QueryUnescape(encoded); err == nil {
				log.Debug().Str("filename", decoded).Msg("Extracted UTF-8 encoded filename")
				return decoded
			}
		}
	}

	// Fallback to standard filename="..." format
	if strings.Contains(disposition, "filename=") {
		parts := strings.Split(disposition, "filename=")
		if len(parts) > 1 {
			filename := strings.Split(parts[1], ";")[0]
			filename = strings.TrimSpace(filename)
			filename = strings.Trim(filename, "\"")
			log.Debug().Str("filename", filename).Msg("Extracted standard filename")
			return filename
		}
	}

	log.Warn().Str("disposition", disposition).Msg("Failed to extract filename from Content-Disposition")
	return ""
}

// isFileResponse detects if the response is a file based on content type
func (hpm *HTTPProxyManager) isFileResponse(contentType string) bool {
	// Normalize Content-Type (remove parameters like charset)
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	// Check for empty content type
	if contentType == "" {
		return false
	}

	// Explicit file Content-Type list
	fileContentTypes := map[string]bool{
		"image/jpeg":    true,
		"image/png":     true,
		"image/gif":     true,
		"image/webp":    true,
		"image/svg+xml": true,
		"image/bmp":     true,
		"image/tiff":    true,
		"image/ico":     true,
		"image/x-icon":  true,

		// Audio types
		"audio/mpeg": true,
		"audio/mp3":  true,
		"audio/wav":  true,
		"audio/ogg":  true,
		"audio/aac":  true,
		"audio/flac": true,
		"audio/m4a":  true,

		// Video types
		"video/mp4":        true,
		"video/mpeg":       true,
		"video/quicktime":  true,
		"video/x-msvideo":  true, // avi
		"video/x-ms-wmv":   true,
		"video/webm":       true,
		"video/x-flv":      true,
		"video/x-matroska": true, // mkv

		// Document types
		"application/pdf":    true,
		"application/msword": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true, // docx
		"application/vnd.ms-excel": true,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true, // xlsx
		"application/vnd.ms-powerpoint":                                             true,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true, // pptx

		// Archive types
		"application/zip":              true,
		"application/x-zip-compressed": true,
		"application/x-rar-compressed": true,
		"application/x-7z-compressed":  true,
		"application/gzip":             true,
		"application/x-gzip":           true,
		"application/x-tar":            true,

		// Executable files
		"application/x-executable":      true,
		"application/x-msdos-program":   true,
		"application/x-msdownload":      true,
		"application/x-apple-diskimage": true, // dmg

		// Other binary files
		"application/octet-stream": true,
		"application/binary":       true,

		// Special text files (usually downloaded)
		"text/csv":        true,
		"text/plain":      true,
		"application/csv": true,
	}

	// Exact match
	if fileContentTypes[contentType] {
		return true
	}

	// Prefix matching for common patterns
	filePrefixes := []string{
		"application/vnd.", // Vendor-specific types
		"application/x-",   // Non-standard types
	}

	for _, prefix := range filePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			// Exclude some types that are clearly not files
			excludeTypes := []string{
				"application/x-www-form-urlencoded",
				"application/x-javascript",
				"application/x-httpd-php",
			}

			for _, exclude := range excludeTypes {
				if contentType == exclude {
					return false
				}
			}

			return true
		}
	}

	return false
}

// handleFileStart processes file transfer start message
func (hpm *HTTPProxyManager) handleFileStart(channel *webrtc.DataChannel, message map[string]interface{}) {
	fileID, ok := message["id"].(string)
	if !ok || fileID == "" {
		// Generate random ID if missing
		fileID = fmt.Sprintf("file_%d", time.Now().UnixNano())
		log.Debug().Str("generated_file_id", fileID).Msg("Generated random file ID")
	}

	metadata, _ := message["metadata"].(map[string]interface{})
	totalChunks, _ := message["total_chunks"].(float64)

	hpm.receiverMutex.Lock()
	defer hpm.receiverMutex.Unlock()

	hpm.fileReceivers[fileID] = &FileReceiver{
		ID:          fileID,
		Metadata:    metadata,
		TotalChunks: int(totalChunks),
		Chunks:      make([][]byte, int(totalChunks)),
		ReceivedAt:  time.Now(),
	}

	log.Debug().Str("file_id", fileID).Int("total_chunks", int(totalChunks)).Msg("Starting file reception")
}

// handleFileChunk processes file chunk message
func (hpm *HTTPProxyManager) handleFileChunk(channel *webrtc.DataChannel, message map[string]interface{}) {
	fileID, ok := message["id"].(string)
	if !ok || fileID == "" {
		sendErrorResponse(channel, 400, "Missing file ID", nil)
		return
	}

	index, ok := message["index"].(float64)
	if !ok {
		sendErrorResponse(channel, 400, "Missing chunk index", nil)
		return
	}

	dataString, ok := message["data"].(string)
	if !ok {
		sendErrorResponse(channel, 400, "Missing chunk data", nil)
		return
	}

	// Decode base64 data
	chunkData, err := base64.StdEncoding.DecodeString(dataString)
	if err != nil {
		log.Error().Err(err).Str("file_id", fileID).Msg("Failed to decode base64 chunk data")
		sendErrorResponse(channel, 400, "Invalid chunk data encoding", nil)
		return
	}

	hpm.receiverMutex.Lock()
	defer hpm.receiverMutex.Unlock()

	receiver, exists := hpm.fileReceivers[fileID]
	if !exists {
		sendErrorResponse(channel, 400, "File receiver not found", nil)
		return
	}

	// Store chunk
	chunkIndex := int(index)
	if chunkIndex >= 0 && chunkIndex < len(receiver.Chunks) {
		receiver.Chunks[chunkIndex] = chunkData
		log.Debug().Str("file_id", fileID).Int("chunk_index", chunkIndex+1).Int("total_chunks", len(receiver.Chunks)).Msg("Received chunk")
	}
}

// handleFileEnd processes file transfer end message and forwards file to backend
func (hpm *HTTPProxyManager) handleFileEnd(channel *webrtc.DataChannel, message map[string]interface{}) {
	fileID, ok := message["id"].(string)
	if !ok || fileID == "" {
		sendErrorResponse(channel, 400, "Missing file ID", nil)
		return
	}

	expectedHash, _ := message["hash"].(string)

	hpm.receiverMutex.Lock()
	receiver, exists := hpm.fileReceivers[fileID]
	if exists {
		delete(hpm.fileReceivers, fileID)
	}
	hpm.receiverMutex.Unlock()

	if !exists {
		sendErrorResponse(channel, 400, "File receiver not found", nil)
		return
	}

	// Check chunk integrity
	missingChunks := []int{}
	for i := 0; i < receiver.TotalChunks; i++ {
		if i >= len(receiver.Chunks) || receiver.Chunks[i] == nil {
			missingChunks = append(missingChunks, i)
		}
	}

	if len(missingChunks) > 0 {
		log.Error().Int("missing_chunks", len(missingChunks)).Str("file_id", fileID).Msg("File reconstruction failed")

		// Check if channel is still open before sending retry request
		if channel.ReadyState() != webrtc.DataChannelStateOpen {
			log.Warn().Str("state", channel.ReadyState().String()).Str("file_id", fileID).Msg("Cannot send retry request: data channel is not open")
			return
		}

		// Send missing chunks request
		retryRequest := map[string]interface{}{
			"status":         "missing_chunks",
			"message":        "Please resend missing chunks",
			"fileId":         fileID,
			"missing_chunks": missingChunks,
			"total_chunks":   receiver.TotalChunks,
		}

		responseBytes, _ := json.Marshal(retryRequest)
		if err := channel.Send(responseBytes); err != nil {
			log.Error().Err(err).Msg("Failed to send retry request")
		} else {
			log.Debug().Int("missing_chunks", len(missingChunks)).Str("file_id", fileID).Msg("Requested retransmission of missing chunks")
		}

		// Put receiver back to map for retransmission
		hpm.receiverMutex.Lock()
		hpm.fileReceivers[fileID] = receiver
		hpm.receiverMutex.Unlock()
		return
	}

	// Reconstruct file
	var completeFile bytes.Buffer
	for i := 0; i < receiver.TotalChunks; i++ {
		completeFile.Write(receiver.Chunks[i])
	}

	fileData := completeFile.Bytes()

	// Verify hash
	if expectedHash != "" {
		actualHash := fmt.Sprintf("%x", sha256.Sum256(fileData))
		if actualHash != expectedHash {
			log.Error().Str("expected_hash", expectedHash).Str("actual_hash", actualHash).Str("file_id", fileID).Msg("File hash verification failed")
			sendErrorResponse(channel, 400, "Hash verification failed", nil)
			return
		}
	}

	log.Debug().Str("file_id", fileID).Int("size", len(fileData)).Msg("File reception completed")

	// Forward file to backend server
	filename := "received_file"
	contentType := "application/octet-stream"
	if receiver.Metadata != nil {
		if metaMap, ok := receiver.Metadata.(map[string]interface{}); ok {
			if name, ok := metaMap["filename"].(string); ok {
				filename = name
			}
			if ctype, ok := metaMap["filetype"].(string); ok && ctype != "" {
				contentType = ctype
			}
		}
	}

	log.Debug().Str("filename", filename).Str("content_type", contentType).Int("size", len(fileData)).Msg("Starting file forwarding to backend server")
	uploadResponse, uploadErr := hpm.forwardFileToServer(fileData, filename, contentType)

	// Build response
	var response map[string]interface{}
	if uploadErr != nil {
		log.Error().Err(uploadErr).Str("file_id", fileID).Msg("File forwarding failed")
		response = map[string]interface{}{
			"status":   "error",
			"message":  fmt.Sprintf("File upload failed: %v", uploadErr),
			"fileId":   fileID,
			"filename": filename,
			"size":     len(fileData),
		}
	} else {
		log.Info().Str("filename", filename).Str("file_id", fileID).Msg("File forwarding succeeded")
		response = map[string]interface{}{
			"status":         "success",
			"message":        "File uploaded successfully",
			"fileId":         fileID,
			"filename":       filename,
			"size":           len(fileData),
			"uploadResponse": uploadResponse,
		}
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal response")
		return
	}

	// Check if channel is still open before sending response
	if channel.ReadyState() != webrtc.DataChannelStateOpen {
		log.Warn().Str("state", channel.ReadyState().String()).Str("file_id", fileID).Msg("Cannot send file processing response: data channel is not open")
		return
	}

	log.Debug().Str("file_id", fileID).Msg("Sending file processing response")
	if err := channel.Send(responseBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send file processing response")
	} else {
		log.Debug().Str("file_id", fileID).Msg("File processing response sent successfully")
	}
}

// forwardFileToServer forwards file to backend server using multipart form data
func (hpm *HTTPProxyManager) forwardFileToServer(fileData []byte, filename, contentType string) (interface{}, error) {
	// Create multipart writer
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Create file field
	fileField, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("failed to create file field: %v", err)
	}

	// Write file data
	if _, err := fileField.Write(fileData); err != nil {
		return nil, fmt.Errorf("failed to write file data: %v", err)
	}

	// Close writer
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %v", err)
	}

	// Build HTTP request
	uploadURL := "http://127.0.0.1:7125/server/files/upload"
	httpReq, err := http.NewRequest("POST", uploadURL, &requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
	}

	// Set Content-Type to multipart/form-data
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	log.Debug().Str("url", uploadURL).Int("size", len(fileData)).Msg("Forwarding file to backend server")

	// Send HTTP request
	resp, err := hpm.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	// Read response
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	log.Debug().Int("status_code", resp.StatusCode).Int("size", len(respData)).Msg("File upload response received")

	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("file upload failed: HTTP %d - %s", resp.StatusCode, string(respData))
	}

	// Try to parse JSON response
	var jsonResponse interface{}
	if json.Valid(respData) {
		if err := json.Unmarshal(respData, &jsonResponse); err == nil {
			return jsonResponse, nil
		}
	}

	// Return as string if not JSON
	return string(respData), nil
}

// sendErrorResponse sends error response to WebRTC data channel
func sendErrorResponse(channel *webrtc.DataChannel, status int, message string, id interface{}) {
	// Check if channel is still open before sending response
	if channel.ReadyState() != webrtc.DataChannelStateOpen {
		log.Warn().Str("state", channel.ReadyState().String()).Int("status", status).Str("message", message).Msg("Cannot send error response: data channel is not open")
		return
	}

	result := map[string]interface{}{
		"status": status,
		"body":   message,
	}

	// Add ID to response if provided
	if id != nil {
		result["id"] = id
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal error response")
		return
	}

	if err := channel.Send(resultBytes); err != nil {
		log.Error().Err(err).Msg("Failed to send error response")
	}
}

// processParams recursively processes nested parameters for URL encoding
func processParams(values url.Values, prefix string, params map[string]interface{}) {
	for key, val := range params {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "[" + key + "]"
		}

		switch v := val.(type) {
		case string:
			values.Add(fullKey, v)
		case float64:
			values.Add(fullKey, strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			values.Add(fullKey, strconv.FormatBool(v))
		case nil:
			values.Add(fullKey, "")
		case map[string]interface{}:
			processParams(values, fullKey, v) // Recursively process nested objects
		case []interface{}:
			for i, item := range v {
				nestedKey := fmt.Sprintf("%s[%d]", fullKey, i)
				if itemMap, ok := item.(map[string]interface{}); ok {
					processParams(values, nestedKey, itemMap)
				} else {
					values.Add(fullKey, fmt.Sprintf("%v", item))
				}
			}
		default:
			values.Add(fullKey, fmt.Sprintf("%v", v))
		}
	}
}
