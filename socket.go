package tradingview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mitchellh/mapstructure"
)

const (
	ExtractFirstMessagePayloadErrorContext = "Extracting payload from first message"
	SetReadDeadlineErrorContext            = "Setting read deadline on connection"
	ParsePacketErrorContext                = "Parsing WebSocket packet"
	ServerErrorMessageContext              = "Received server error message"
	DecodeQuoteMessageErrorContext         = "Decoding quote message payload"
	MaxReconnectErrorContext               = "Maximum reconnection attempts reached"
	PingErrorContext                       = "Sending ping request"
	ReadTimeoutErrorContext                = "Read operation timed out"
	ExtractPayloadErrorContext             = "Extracting payload from WebSocket message"
)

const (
	defaultReconnectDelay = 5 * time.Second
	maxReconnectAttempts  = 20
	readTimeout           = 20 * time.Second
	pingInterval          = 10 * time.Second
	defaultBuildTime      = "2025_03_05-11_31"
	defaultFromParam      = "/chart"
	defaultConnectionType = "main"
	defaultReconnectHost  = "history-data.tradingview.com"
	defaultProHost        = "prodata.tradingview.com"
	defaultWebSocketHost  = "data.tradingview.com"
)

// Socket represents a WebSocket connection to TradingView
type Socket struct {
	OnReceiveMarketDataCallback OnReceiveDataCallback
	OnErrorCallback             OnErrorCallback

	conn           *websocket.Conn
	host           string
	reconnectHost  string
	connectionType string
	buildTime      string
	fromParam      string

	sessionID      string
	isClosed       bool
	isConnecting   bool
	reconnectCount int
	messageQueue   []*SocketMessage
	addedSymbols   map[string]struct{}
	removedSymbols map[string]struct{}
	pingInfo       PingInfo
	pingTicker     *time.Ticker
	done           chan struct{}
}

// PingInfo holds latency statistics for ping requests
type PingInfo struct {
	Last  time.Duration
	Min   time.Duration
	Max   time.Duration
	Avg   float64
	Count int
}

// NewSocket creates a new Socket instance with configuration
func NewSocket(options ...func(*Socket)) *Socket {
	s := &Socket{
		host:           defaultWebSocketHost,
		reconnectHost:  defaultReconnectHost,
		buildTime:      defaultBuildTime,
		fromParam:      defaultFromParam,
		connectionType: defaultConnectionType,
		addedSymbols:   make(map[string]struct{}),
		removedSymbols: make(map[string]struct{}),
		done:           make(chan struct{}),
	}

	for _, option := range options {
		option(s)
	}

	return s
}

// Connect initializes and connects the WebSocket
func Connect(
	onReceiveMarketData OnReceiveDataCallback,
	onError OnErrorCallback,
	options ...func(*Socket),
) (SocketInterface, error) {
	s := NewSocket(options...)
	s.OnReceiveMarketDataCallback = onReceiveMarketData
	s.OnErrorCallback = onError

	if err := s.Init(); err != nil {
		return nil, err
	}

	return s, nil
}

// Init establishes the WebSocket connection and sets up the session
func (s *Socket) Init() (err error) {
	if s.isConnecting || s.isClosed {
		return nil
	}

	s.isClosed = false
	s.isConnecting = true
	defer func() { s.isConnecting = false }()

	wsURL := s.getWebSocketURL()
	s.conn, _, err = websocket.DefaultDialer.Dial(wsURL, getHeaders())
	if err != nil {
		s.onError(err, InitErrorContext)
		s.scheduleReconnect()
		return fmt.Errorf("connection failed: %w", err)
	}

	if err = s.checkFirstReceivedMessage(); err != nil {
		s.onError(err, ReadFirstMessageErrorContext)
		s.scheduleReconnect()
		return fmt.Errorf("first message error: %w", err)
	}

	if err = s.sendConnectionSetupMessages(); err != nil {
		s.onError(err, ConnectionSetupMessagesErrorContext)
		s.scheduleReconnect()
		return fmt.Errorf("setup messages error: %w", err)
	}

	go s.connectionLoop()
	s.startPing()
	return nil
}

// Close terminates the WebSocket connection
func (s *Socket) Close() (err error) {
	s.isClosed = true
	close(s.done)
	if s.conn != nil {
		err = s.conn.Close()
	}
	return
}

// AddSymbol subscribes to updates for a symbol
func (s *Socket) AddSymbol(symbol string) error {
	delete(s.removedSymbols, symbol)
	s.addedSymbols[symbol] = struct{}{}

	msg := getSocketMessage("quote_add_symbols", []interface{}{s.sessionID, symbol})
	if s.isConnected() {
		return s.sendSocketMessage(msg)
	}
	s.messageQueue = append(s.messageQueue, msg)
	return nil
}

// RemoveSymbol unsubscribes from updates for a symbol
func (s *Socket) RemoveSymbol(symbol string) error {
	delete(s.addedSymbols, symbol)
	s.removedSymbols[symbol] = struct{}{}

	msg := getSocketMessage("quote_remove_symbols", []interface{}{s.sessionID, symbol})
	if s.isConnected() {
		return s.sendSocketMessage(msg)
	}
	s.messageQueue = append(s.messageQueue, msg)
	return nil
}

// GetSessionID returns the current session ID
func (s *Socket) GetSessionID() string {
	return s.sessionID
}

// GetPingInfo returns latency statistics
func (s *Socket) GetPingInfo() PingInfo {
	return s.pingInfo
}

func (s *Socket) isConnected() bool {
	return s.conn != nil && !s.isClosed
}

func (s *Socket) checkFirstReceivedMessage() error {
	_, msg, err := s.conn.ReadMessage()
	if err != nil {
		s.onError(err, ReadFirstMessageErrorContext)
		return err
	}

	payload, err := extractPayload(msg)
	if err != nil {
		s.onError(fmt.Errorf("%w: %s", err, msg), ExtractPayloadErrorContext)
		return err
	}

	s.sessionID = string(payload)
	return nil
}

func (s *Socket) sendConnectionSetupMessages() error {
	messages := []*SocketMessage{
		getSocketMessage("set_auth_token", []string{"unauthorized_user_token"}),
		getSocketMessage("quote_create_session", []string{s.sessionID}),
		getSocketMessage("quote_set_fields", []string{s.sessionID, "lp", "volume", "bid", "ask"}),
	}

	for _, msg := range messages {
		if err := s.sendSocketMessage(msg); err != nil {
			return err
		}
	}

	added := make([]string, 0, len(s.addedSymbols))
	for sym := range s.addedSymbols {
		added = append(added, sym)
	}

	for _, sym := range added {
		msg := getSocketMessage("quote_add_symbols", []interface{}{s.sessionID, sym, getFlags()})
		if err := s.sendSocketMessage(msg); err != nil {
			return err
		}
	}

	return nil
}

func (s *Socket) sendSocketMessage(msg *SocketMessage) error {
	payload, _ := json.Marshal(msg)
	framedMsg := formatMessage(payload)
	return s.conn.WriteMessage(websocket.TextMessage, framedMsg)
}

func (s *Socket) connectionLoop() {
	defer s.conn.Close()

	for {
		if s.isClosed {
			return
		}

		if err := s.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			s.onError(err, SetReadDeadlineErrorContext)
			s.scheduleReconnect()
			return
		}

		msgType, msg, err := s.conn.ReadMessage()
		if err != nil {
			s.handleReadError(err)
			return
		}

		go s.processMessage(msgType, msg)
	}
}

func (s *Socket) processMessage(msgType int, msg []byte) {
	if msgType != websocket.TextMessage {
		return
	}

	if isHeartbeatMessage(msg) {
		if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			s.onError(err, SendKeepAliveMessageErrorContext)
		}
		return
	}

	s.parsePacket(msg)
}

func (s *Socket) parsePacket(packet []byte) {
	index := 0
	for index < len(packet) {
		payload, length, err := extractNextPayload(packet[index:])
		if err != nil {
			s.onError(err, ParsePacketErrorContext)
			return
		}
		index += length

		data, err := s.parsePayload(payload)
		if err != nil || data == nil {
			continue
		}

		s.OnReceiveMarketDataCallback(data)
	}
}

func (s *Socket) parsePayload(payload []byte) (*QuoteData, error) {
	var msg SocketMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		s.onError(err, DecodeMessageErrorContext)
		return nil, err
	}

	if msg.Message == "critical_error" || msg.Message == "error" {
		err := fmt.Errorf("server error: %s", payload)
		s.onError(err, ServerErrorMessageContext)
		return nil, err
	}

	if msg.Message != "qsd" {
		return nil, nil
	}

	payloadData, ok := msg.Payload.([]interface{})
	if !ok || len(payloadData) < 2 {
		return nil, errors.New("invalid payload format")
	}

	var qm QuoteMessage
	if err := mapstructure.Decode(payloadData[1], &qm); err != nil {
		s.onError(err, DecodeQuoteMessageErrorContext)
		return nil, err
	}

	if qm.Status != "ok" || qm.Symbol == "" || qm.Data == nil {
		return nil, errors.New("incomplete quote message")
	}

	qm.Data.Date = time.Now().UTC()
	details := strings.Split(qm.Symbol, ":")
	qm.Data.Source = details[0]
	qm.Data.Symbol = details[1]

	return qm.Data, nil
}

func (s *Socket) scheduleReconnect() {
	if s.reconnectCount >= maxReconnectAttempts {
		s.onError(errors.New("maximum reconnect attempts reached"), MaxReconnectErrorContext)
		return
	}

	delay := time.Duration(math.Pow(2, float64(s.reconnectCount))) * defaultReconnectDelay
	s.reconnectCount++

	time.AfterFunc(delay, func() {
		if err := s.Init(); err != nil {
			s.scheduleReconnect()
		} else {
			s.reconnectCount = 0
		}
	})
}

func (s *Socket) startPing() {
	s.pingTicker = time.NewTicker(pingInterval)
	go func() {
		for {
			select {
			case <-s.pingTicker.C:
				s.sendPing()
			case <-s.done:
				s.pingTicker.Stop()
				return
			}
		}
	}()
}

func (s *Socket) sendPing() {
	start := time.Now()
	host := s.selectHost()
	url := fmt.Sprintf("https://%s/ping", host)

	resp, err := http.Get(url)
	if err != nil {
		s.onError(err, PingErrorContext)
		return
	}
	defer resp.Body.Close()

	latency := time.Since(start)

	if s.pingInfo.Count == 0 {
		s.pingInfo.Min = latency
		s.pingInfo.Max = latency
	} else {
		if latency < s.pingInfo.Min {
			s.pingInfo.Min = latency
		}
		if latency > s.pingInfo.Max {
			s.pingInfo.Max = latency
		}
	}
	s.pingInfo.Last = latency
	s.pingInfo.Count++
	total := s.pingInfo.Avg*float64(s.pingInfo.Count-1) + float64(latency)
	s.pingInfo.Avg = total / float64(s.pingInfo.Count)
}

func (s *Socket) selectHost() string {
	if s.reconnectCount > 3 && s.reconnectHost != "" {
		return s.reconnectHost
	}
	return s.host
}

func (s *Socket) getWebSocketURL() string {
	u := url.URL{
		Scheme: "wss",
		Host:   s.selectHost(),
		Path:   "/socket.io/websocket",
	}

	q := u.Query()
	q.Add("from", s.fromParam)
	q.Add("date", s.buildTime)
	q.Add("type", s.connectionType)

	return u.String()
}

func (s *Socket) handleReadError(err error) {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		return
	}

	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		s.onError(err, ReadTimeoutErrorContext)
	} else {
		s.onError(err, ReadMessageErrorContext)
	}

	s.scheduleReconnect()
}

func (s *Socket) onError(err error, context string) {
	if s.OnErrorCallback != nil {
		s.OnErrorCallback(err, context)
	}
}

// Helper functions
func formatMessage(payload []byte) []byte {
	header := fmt.Sprintf("~m~%d~m~", len(payload))
	return append([]byte(header), payload...)
}

func isHeartbeatMessage(msg []byte) bool {
	return strings.HasPrefix(string(msg), "~m~") && strings.Contains(string(msg), "~h~")
}

func extractNextPayload(data []byte) ([]byte, int, error) {
	if len(data) < 6 || !strings.HasPrefix(string(data), "~m~") {
		return nil, 0, errors.New("invalid message format")
	}

	endLengthIndex := strings.Index(string(data[3:]), "~m~") + 3
	if endLengthIndex < 4 {
		return nil, 0, errors.New("invalid length format")
	}

	lengthStr := string(data[3:endLengthIndex])
	length, err := strconv.Atoi(lengthStr)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid length value: %w", err)
	}

	startPayload := endLengthIndex + 3
	endPayload := startPayload + length
	if endPayload > len(data) {
		return nil, 0, errors.New("payload exceeds message length")
	}

	return data[startPayload:endPayload], endPayload, nil
}

func getHeaders() http.Header {
	headers := http.Header{}

	headers.Set("Accept-Encoding", "gzip, deflate, br")
	headers.Set("Accept-Language", "en-US,en;q=0.9,es;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Host", "data.tradingview.com")
	headers.Set("Origin", "https://www.tradingview.com")
	headers.Set("Pragma", "no-cache")
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.193 Safari/537.36")

	return headers
}

// Configuration options
func WithHost(host string) func(*Socket) {
	return func(s *Socket) {
		s.host = host
	}
}

func WithReconnectHost(host string) func(*Socket) {
	return func(s *Socket) {
		s.reconnectHost = host
	}
}

func WithBuildTime(time string) func(*Socket) {
	return func(s *Socket) {
		s.buildTime = time
	}
}

func WithFromParam(param string) func(*Socket) {
	return func(s *Socket) {
		s.fromParam = param
	}
}

func WithConnectionType(connType string) func(*Socket) {
	return func(s *Socket) {
		s.connectionType = connType
	}
}

func getSocketMessage(m string, p interface{}) *SocketMessage {
	return &SocketMessage{
		Message: m,
		Payload: p,
	}
}

func getFlags() *Flags {
	return &Flags{
		Flags: []string{"force_permission"},
	}
}

// extractPayload handles the TradingView WebSocket message format "~m~{length}~m~{payload}"
func extractPayload(msg []byte) ([]byte, error) {
	const headerPrefix = "~m~"

	if !bytes.HasPrefix(msg, []byte(headerPrefix)) {
		return nil, errors.New("invalid message format - missing header prefix")
	}

	// Find the second ~m~ delimiter
	endHeaderIndex := bytes.Index(msg[3:], []byte(headerPrefix))
	if endHeaderIndex == -1 {
		return nil, errors.New("invalid message format - missing closing header")
	}

	// Calculate positions: 3 (start offset) + found index + 3 (header length)
	payloadStart := 3 + endHeaderIndex + 3
	if payloadStart > len(msg) {
		return nil, errors.New("invalid message format - payload start exceeds message length")
	}

	return msg[payloadStart:], nil
}
