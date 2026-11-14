package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// otpJobType identifies sign-in code jobs on the notifications exchange.
const otpJobType = "otp"

// otpJob is a queued sign-in code. It carries the code and the raw contact,
// so it is only ever written to the broker, never to a log or to the
// dead-letter queue.
type otpJob struct {
	// ID identifies the job, so that a worker can tell a job that the
	// broker hands out again from a new one (#232).
	ID            string    `json:"id,omitempty"`
	Type          string    `json:"type"`
	ContactType   string    `json:"contact_type"`
	Contact       string    `json:"contact"`
	MaskedContact string    `json:"masked_contact"`
	Code          string    `json:"code"`
	Purpose       string    `json:"purpose,omitempty"`
	Details       string    `json:"details,omitempty"`
	Amount        string    `json:"amount,omitempty"`
	Destination   string    `json:"destination,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// deliveryID is the key of the job in the delivery log: its ID, or, for a
// job that carries none, a hash of its body. The body
// holds the code and its expiry, so two jobs never share it.
func (j otpJob) deliveryID(body []byte) string {
	if j.ID != "" {
		return j.ID
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (j otpJob) message() OTPMessage {
	return OTPMessage{
		ContactType:   j.ContactType,
		Contact:       j.Contact,
		MaskedContact: j.MaskedContact,
		Code:          j.Code,
		Purpose:       j.Purpose,
		Details:       j.Details,
		Amount:        j.Amount,
		Destination:   j.Destination,
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
		ID:            uuid.NewString(),
		Type:          otpJobType,
		ContactType:   msg.ContactType,
		Contact:       msg.Contact,
		MaskedContact: msg.MaskedContact,
		Code:          msg.Code,
		Purpose:       msg.Purpose,
		Details:       msg.Details,
		Amount:        msg.Amount,
		Destination:   msg.Destination,
	}
	publishing := amqp.Publishing{
		ContentType: "application/json",
		Type:        otpJobType,
		MessageId:   job.ID,
		// A code is useless once it expires, so it is published as
		// transient. The quorum queue (#232) still writes it to the disks of
		// a majority of its nodes, whatever its delivery mode: that is what
		// keeps it when a node is lost. It expires there with the code.
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
		// The broker is away or did not confirm the job: the request is
		// answered with 503 and Retry-After, not 500 (#232).
		dberr.MarkUnavailable(ctx, dberr.DeliveryUnavailable)
		return fmt.Errorf("queue the sign-in code: %w", err)
	}
	return nil
}
