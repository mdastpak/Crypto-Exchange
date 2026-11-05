package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// otpJobType identifies sign-in code jobs on the notifications exchange.
const otpJobType = "otp"

// otpJob is a queued sign-in code. It carries the code and the raw contact,
// so it is only ever written to the broker, never to a log or to the
// dead-letter queue.
type otpJob struct {
	Type          string    `json:"type"`
	ContactType   string    `json:"contact_type"`
	Contact       string    `json:"contact"`
	MaskedContact string    `json:"masked_contact"`
	Code          string    `json:"code"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (j otpJob) message() OTPMessage {
	return OTPMessage{
		ContactType:   j.ContactType,
		Contact:       j.Contact,
		MaskedContact: j.MaskedContact,
		Code:          j.Code,
	}
}

// expired reports whether the code expired before now. A job without an
// expiry never expires.
func (j otpJob) expired(now time.Time) bool {
	return !j.ExpiresAt.IsZero() && !now.Before(j.ExpiresAt)
}

// deliveryContext ends when the code expires, since delivering an expired
// code is pointless.
func (j otpJob) deliveryContext() (context.Context, context.CancelFunc) {
	if j.ExpiresAt.IsZero() {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), j.ExpiresAt)
}

// Publisher publishes a message and waits until the broker confirms it.
type Publisher interface {
	PublishMessage(ctx context.Context, exchange string, msg amqp.Publishing) error
}

// QueueSender queues sign-in codes on the notifications exchange for a Worker
// to deliver, so that a slow or failing provider does not hold up sign-in
// requests and failed deliveries can be retried.
type QueueSender struct {
	publisher Publisher
	exchange  string
	channels  Channels
	codeTTL   time.Duration
	now       func() time.Time
}

// NewQueueSender returns a sender that publishes to exchange. channels decides
// which contact types are accepted, and codeTTL is the lifetime of the codes.
func NewQueueSender(publisher Publisher, exchange string, channels Channels, codeTTL time.Duration) *QueueSender {
	return &QueueSender{
		publisher: publisher,
		exchange:  exchange,
		channels:  channels,
		codeTTL:   codeTTL,
		now:       time.Now,
	}
}

func (s *QueueSender) Supports(contactType string) bool {
	return s.channels.For(contactType) != nil
}

// SendOTP queues msg. It returns once the broker has accepted the job, before
// the code is delivered.
func (s *QueueSender) SendOTP(ctx context.Context, msg OTPMessage) error {
	if !s.Supports(msg.ContactType) {
		return fmt.Errorf("%w: %s", ErrUnsupportedContactType, msg.ContactType)
	}

	now := s.now()
	job := otpJob{
		Type:          otpJobType,
		ContactType:   msg.ContactType,
		Contact:       msg.Contact,
		MaskedContact: msg.MaskedContact,
		Code:          msg.Code,
	}
	publishing := amqp.Publishing{
		ContentType: "application/json",
		Type:        otpJobType,
		// A code is useless once it expires, so there is no reason to keep it
		// on the broker's disk across restarts.
		DeliveryMode: amqp.Transient,
		Timestamp:    now,
	}
	if s.codeTTL > 0 {
		job.ExpiresAt = now.Add(s.codeTTL)
		// The broker drops the job if no worker takes it before the code expires.
		publishing.Expiration = strconv.FormatInt(s.codeTTL.Milliseconds(), 10)
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("encode the sign-in code job: %w", err)
	}
	publishing.Body = body
	if err := s.publisher.PublishMessage(ctx, s.exchange, publishing); err != nil {
		return fmt.Errorf("queue the sign-in code: %w", err)
	}
	return nil
}
