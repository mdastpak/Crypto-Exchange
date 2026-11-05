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

	"encrypted-db/config"
	"encrypted-db/internal/models"
	"encrypted-db/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/streadway/amqp"
)

type WebSocketHandler struct {
	RabbitMQService *rabbitmq.RabbitMQService
	Clients         map[*Client]bool
	Mu              sync.RWMutex
	Upgrader        websocket.Upgrader
	ShutdownCh      chan struct{}
	Wg              sync.WaitGroup
}

type Client struct {
	Conn     *websocket.Conn
	Send     chan []byte
	Timezone string
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
	}
)

func NewWebSocketHandler(is *models.InfraServices) *WebSocketHandler {
	h := &WebSocketHandler{
		RabbitMQService: is.RabbitMQ,
		Clients:         make(map[*Client]bool),
		Upgrader:        defaultUpgrader,
		ShutdownCh:      make(chan struct{}),
	}
	return h
}

func (h *WebSocketHandler) ServeWSGin(c *gin.Context) {
	h.ServeWS(c.Writer, c.Request)
}

func (h *WebSocketHandler) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &Client{
		Conn:     conn,
		Send:     make(chan []byte, 256),
		Timezone: "UTC",
	}

	h.registerClient(client)
	// Note: readPump's own defer unregisters the client on exit; unregistering
	// again here would double-close client.Send and panic.

	h.Wg.Add(1)
	go h.writePump(client)

	h.readPump(client)
}

func (h *WebSocketHandler) registerClient(client *Client) {
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()
	log.Printf("WebSocket client connected. Total: %d", h.clientCount())
}

func (h *WebSocketHandler) unregisterClient(client *Client) {
	h.Mu.Lock()
	delete(h.Clients, client)
	h.Mu.Unlock()
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
	// h.Wg.Add(1) call in ServeWS before it is started); readPump runs
	// synchronously on the caller's goroutine and must not call h.Wg.Done()
	// itself, or the WaitGroup counter goes negative and panics on every
	// normal client disconnect.
	defer h.unregisterClient(client)

	client.Conn.SetReadLimit(512)
	// A deadline that cannot be set means the connection is broken.
	if err := client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return
	}
	client.Conn.SetPongHandler(func(string) error {
		// An error here ends ReadMessage, and with it the connection.
		return client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	for {
		select {
		case <-h.ShutdownCh:
			return
		default:
			messageType, message, err := client.Conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket read error: %v", err)
				}
				return
			}

			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}

			h.handleClientMessage(client, message)
		}
	}
}

func (h *WebSocketHandler) writePump(client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		h.Wg.Done()
	}()

	for {
		select {
		case message, ok := <-client.Send:
			if err := client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
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
			if err := client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := client.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-h.ShutdownCh:
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
			client.Timezone = tz
		}
		h.handleClientPing(client)

	case "subscribe":
		// Future: handle channel subscriptions
		log.Printf("Client subscribed to: %v", event["channels"])

	default:
		log.Printf("Unhandled event type: %s", evt)
	}
}

func (h *WebSocketHandler) handleClientPing(client *Client) {
	loc, err := time.LoadLocation(client.Timezone)
	if err != nil {
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

func (h *WebSocketHandler) Shutdown(ctx context.Context) error {
	close(h.ShutdownCh)
	h.Mu.Lock()
	for client := range h.Clients {
		client.Conn.Close()
	}
	h.Mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.Wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
