package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gorilla/websocket"
	amqp "github.com/rabbitmq/amqp091-go"
)

type WebSocketHandler struct {
	RabbitMQService *rabbitmq.RabbitMQService
	Clients         map[*Client]bool
	Mu              sync.RWMutex
	Upgrader        websocket.Upgrader
	ShutdownCh      chan struct{}
	Wg              sync.WaitGroup
	// Authenticate checks the access token that a connection brings. A
	// connection without a token is anonymous. Without Authenticate, every
	// token is refused.
	Authenticate func(ctx context.Context, token string) (*auth.Claims, error)
	// VerifySession checks that a session is still active and that its user
	// may still sign in, with the errors of auth: ErrSessionInvalid or
	// ErrAccountUnavailable. StartSessionWatcher uses it to close the
	// connections of sessions that ended (#189).
	VerifySession func(ctx context.Context, sessionID, userID string) error

	limits limits
	// open and openPerIP count the connections, guarded by Mu, including
	// upgrades that are still in progress.
	open         int
	openPerIP    map[string]int
	shutdownOnce sync.Once
}

type Client struct {
	Conn     *websocket.Conn
	Send     chan []byte
	Timezone string
	// UserID and SessionID name the signed-in user of an authenticated
	// connection, for the account channels of #66. They are empty for an
	// anonymous connection. A reauth may set them while the connection is
	// open, so they are read with principal once it is registered.
	UserID    string
	SessionID string

	// mu guards UserID, SessionID, and the expiry of the token.
	mu        sync.Mutex
	expiresAt time.Time
	expiry    *time.Timer

	ip       string
	location *time.Location
	limiter  *tokenBucket
}

var (
	// Default upgrader with secure origin check
	defaultUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// In production, configure allowed origins from config
			origin := r.Header.Get("Origin")
			allowed := config.Config.WebSocket.AllowedOrigins
			if len(allowed) == 0 {
				// Default: allow same origin only
				return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
			}
			for _, o := range allowed {
				if o == "*" || o == origin {
					return true
				}
			}
			return false
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// Selected when a browser sends its token as a subprotocol, as the
		// WebSocket handshake requires.
		Subprotocols: []string{accessTokenProtocol},
		Error:        handshakeError,
	}
)

func NewWebSocketHandler(is *models.InfraServices) *WebSocketHandler {
	h := &WebSocketHandler{
		RabbitMQService: is.RabbitMQ,
		Clients:         make(map[*Client]bool),
		Upgrader:        defaultUpgrader,
		ShutdownCh:      make(chan struct{}),
		limits:          limitsFromConfig(),
		openPerIP:       make(map[string]int),
	}
	return h
}

// registerClient adds a client and counts its write pump in Wg. It refuses
// the client once Shutdown has started, which can happen while the upgrade
// is in progress, since the HTTP server no longer tracks a connection once it
// is upgraded. The check here and the close of ShutdownCh both hold Mu, so
// either Shutdown sees the client, or the client sees the shutdown.
func (h *WebSocketHandler) registerClient(client *Client) bool {
	h.Mu.Lock()
	select {
	case <-h.ShutdownCh:
		h.Mu.Unlock()
		return false
	default:
	}
	h.Clients[client] = true
	h.Wg.Add(1)
	h.Mu.Unlock()
	log.Printf("WebSocket client connected (authenticated: %t). Total: %d", client.UserID != "", h.clientCount())
	return true
}

func (h *WebSocketHandler) unregisterClient(client *Client) {
	h.Mu.Lock()
	delete(h.Clients, client)
	h.Mu.Unlock()
	client.forget()
	h.release(client.ip)
	close(client.Send)
	client.Conn.Close()
	log.Printf("WebSocket client disconnected. Total: %d", h.clientCount())
}

func (h *WebSocketHandler) clientCount() int {
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	return len(h.Clients)
}

func (h *WebSocketHandler) readPump(client *Client) {
	// Note: only writePump is tracked via h.Wg (matching the single
	// h.Wg.Add(1) call in registerClient before it is started); readPump runs
	// synchronously on the caller's goroutine and must not call h.Wg.Done()
	// itself, or the WaitGroup counter goes negative and panics on every
	// normal client disconnect.
	defer h.unregisterClient(client)

	// A message larger than the limit ends the connection.
	client.Conn.SetReadLimit(h.limits.maxMessageBytes)
	// A deadline that cannot be set means the connection is broken.
	if err := client.Conn.SetReadDeadline(time.Now().Add(h.limits.pongWait)); err != nil {
		return
	}
	client.Conn.SetPongHandler(func(string) error {
		// An error here ends ReadMessage, and with it the connection.
		return client.Conn.SetReadDeadline(time.Now().Add(h.limits.pongWait))
	})

	// The loop does not watch for shutdown: closing the connection itself
	// could cut off the close frame that writePump sends then. It ends when
	// the client answers that frame, or when Shutdown closes the connection.
	for {
		messageType, message, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			return
		}

		if !client.limiter.allow(time.Now()) {
			// A client that sends faster than its limit is disconnected
			// with a policy violation. WriteControl may run concurrently
			// with writePump.
			_ = client.Conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "too many messages"),
				time.Now().Add(time.Second))
			return
		}

		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}

		h.handleClientMessage(client, message)
	}
}

func (h *WebSocketHandler) writePump(client *Client) {
	ticker := time.NewTicker(h.limits.pingInterval)
	defer func() {
		ticker.Stop()
		h.Wg.Done()
	}()

	for {
		select {
		case message, ok := <-client.Send:
			if err := client.Conn.SetWriteDeadline(time.Now().Add(h.limits.writeTimeout)); err != nil {
				return
			}
			if !ok {
				// The connection closes either way, so a failed close
				// frame changes nothing.
				_ = client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Every message is its own frame, so that each frame holds exactly
			// one JSON document even when several messages are queued.
			if err := client.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			if err := client.Conn.SetWriteDeadline(time.Now().Add(h.limits.writeTimeout)); err != nil {
				return
			}
			if err := client.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-h.ShutdownCh:
			// Tell the client why the connection ends, so that it can
			// reconnect to another instance at once. Shutdown closes the
			// connection afterwards.
			_ = client.Conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server is shutting down"),
				time.Now().Add(h.limits.writeTimeout))
			return
		}
	}
}

func (h *WebSocketHandler) handleClientMessage(client *Client, message []byte) {
	var event map[string]interface{}
	if err := json.Unmarshal(message, &event); err != nil {
		log.Printf("Invalid message format: %v", err)
		return
	}

	evt, ok := event["event"].(string)
	if !ok {
		return
	}

	switch evt {
	case "ping":
		if tz, ok := event["timezone"].(string); ok {
			// An unknown zone keeps the previous one.
			client.setTimezone(tz)
		}
		h.handleClientPing(client)

	case "reauth":
		h.handleReauth(client, event)

	case "subscribe":
		// Future: handle channel subscriptions (#66)
		log.Printf("Client subscribed to: %v", event["channels"])

	default:
		log.Printf("Unhandled event type: %s", quoted(evt))
	}
}

func (h *WebSocketHandler) handleClientPing(client *Client) {
	loc := client.location
	if loc == nil {
		loc = time.UTC
	}
	serverTime := time.Now().In(loc).Format(time.RFC3339)

	pongMessage := map[string]interface{}{
		"event":     "pong",
		"timestamp": serverTime,
	}

	data, err := json.Marshal(pongMessage)
	if err != nil {
		log.Printf("Error marshaling pong: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		log.Printf("Client send buffer full, dropping ping")
	}
}

func (h *WebSocketHandler) Broadcast(message []byte) {
	h.Mu.RLock()
	defer h.Mu.RUnlock()

	for client := range h.Clients {
		select {
		case client.Send <- message:
		default:
			log.Printf("Client send buffer full, skipping broadcast")
		}
	}
}

// consumerPrefetch bounds the events delivered to this instance and not yet
// broadcast.
const consumerPrefetch = 64

const maxConsumerBackoff = 30 * time.Second

// subscription is an open consumer of the event exchange.
type subscription struct {
	channel    *amqp.Channel
	deliveries <-chan amqp.Delivery
}

// StartConsumer forwards the events of the price updates exchange to the
// connected clients. It subscribes before it returns, so no event published
// afterwards is missed, and keeps consuming in the background until ctx ends
// or the handler shuts down, subscribing again whenever the channel closes.
func (h *WebSocketHandler) StartConsumer(ctx context.Context) error {
	if h.RabbitMQService == nil {
		return errors.New("the WebSocket consumer needs RabbitMQ")
	}
	exchange := config.Config.RabbitMQ.Exchanges.PriceUpdates
	sub, err := h.subscribe(exchange)
	if err != nil {
		return fmt.Errorf("subscribe to %s: %w", exchange, err)
	}

	h.Wg.Add(1)
	go func() {
		defer h.Wg.Done()
		h.consumeRabbitMQ(ctx, exchange, sub)
	}()
	return nil
}

func (h *WebSocketHandler) consumeRabbitMQ(ctx context.Context, exchange string, sub *subscription) {
	backoff := time.Second
	for {
		if sub != nil {
			err := h.drain(ctx, sub)
			if err == nil {
				return // stopped
			}
			log.Printf("WebSocket consumer: %v", err)
			backoff = time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-h.ShutdownCh:
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxConsumerBackoff)

		var err error
		sub, err = h.subscribe(exchange)
		if err != nil {
			log.Printf("WebSocket consumer: subscribe to %s: %v; retrying in %v", exchange, err, backoff)
		}
	}
}

// subscribe opens a channel of its own and binds a queue for this instance to
// exchange. The queue is server-named, exclusive, and deleted with the
// channel, so every server instance receives every event and forwards it to
// its own clients.
func (h *WebSocketHandler) subscribe(exchange string) (*subscription, error) {
	ch, err := h.RabbitMQService.OpenChannel()
	if err != nil {
		return nil, err
	}
	if err := ch.Qos(consumerPrefetch, 0, false); err != nil {
		ch.Close()
		return nil, err
	}
	queue, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		ch.Close()
		return nil, err
	}
	if err := ch.QueueBind(queue.Name, "", exchange, false, nil); err != nil {
		ch.Close()
		return nil, err
	}
	deliveries, err := ch.Consume(queue.Name, "", false, true, false, false, nil)
	if err != nil {
		ch.Close()
		return nil, err
	}
	log.Printf("WebSocket consumer subscribed to %s", exchange)
	return &subscription{channel: ch, deliveries: deliveries}, nil
}

// drain broadcasts deliveries until the handler stops, which returns nil, or
// the channel closes, which returns an error.
func (h *WebSocketHandler) drain(ctx context.Context, sub *subscription) error {
	defer sub.channel.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.ShutdownCh:
			return nil
		case msg, ok := <-sub.deliveries:
			if !ok {
				return errors.New("the consumer channel closed")
			}
			h.handleCurrencyUpdate(msg.Body)
			_ = msg.Ack(false)
		}
	}
}

// closeGrace is how long Shutdown waits for clients to answer the close
// frame before it closes their connections.
const closeGrace = time.Second

// Shutdown stops the event consumer, sends every client a close frame
// (1001, going away), and closes the connections, waiting for the
// goroutines until ctx ends. It may be called more than once.
func (h *WebSocketHandler) Shutdown(ctx context.Context) error {
	h.shutdownOnce.Do(func() {
		// Under Mu, so that no client registers after this; see
		// registerClient.
		h.Mu.Lock()
		close(h.ShutdownCh)
		h.Mu.Unlock()
	})

	// The write pumps send the close frames and return, and the consumer
	// stops.
	done := make(chan struct{})
	go func() {
		h.Wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}

	// A client answers the close frame with its own, which ends its read
	// loop. Closing a connection before that answer arrives can reset it
	// and lose the frame, so the clients get a moment to answer; the rest
	// are closed.
	grace := time.NewTimer(closeGrace)
	defer grace.Stop()
	for h.clientCount() > 0 {
		select {
		case <-grace.C:
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
			continue
		}
		break
	}
	h.Mu.Lock()
	for client := range h.Clients {
		client.Conn.Close()
	}
	h.Mu.Unlock()
	return err
}
