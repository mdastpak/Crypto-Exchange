package notify

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type publishedMessage struct {
	exchange string
	msg      amqp.Publishing
}

type recordingPublisher struct {
	mu        sync.Mutex
	published []publishedMessage
	err       error
}

func (p *recordingPublisher) PublishMessage(ctx context.Context, exchange string, msg amqp.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, publishedMessage{exchange: exchange, msg: msg})
	return p.err
}

func (p *recordingPublisher) messages() []publishedMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]publishedMessage(nil), p.published...)
}

func emailOnlyChannels() Channels {
	return Channels{Email: deliverFunc(func(context.Context, OTPMessage) error { return nil })}
}

func TestQueueSender_PublishesAnExpiringJob(t *testing.T) {
	publisher := &recordingPublisher{}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), 90*time.Second)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	sender.now = func() time.Time { return now }

	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))

	published := publisher.messages()
	require.Len(t, published, 1)
	assert.Equal(t, "notifications", published[0].exchange)
	msg := published[0].msg
	assert.Equal(t, "application/json", msg.ContentType)
	assert.Equal(t, "otp", msg.Type)
	assert.Equal(t, "90000", msg.Expiration, "the broker drops the job when the code expires")
	assert.Equal(t, uint8(amqp.Transient), msg.DeliveryMode, "codes are not meant to outlive their expiry")

	var job otpJob
	require.NoError(t, json.Unmarshal(msg.Body, &job))
	_, err := uuid.Parse(job.ID)
	require.NoError(t, err, "every job has an ID of its own (#232)")
	assert.Equal(t, job.ID, msg.MessageId)
	assert.Equal(t, job.ID, job.deliveryID(msg.Body))
	assert.Equal(t, otpJob{
		ID:            job.ID,
		Type:          "otp",
		ContactType:   ContactEmail,
		Contact:       "alice@example.com",
		MaskedContact: "ali*****@example.com",
		Code:          "482913",
		ExpiresAt:     now.Add(90 * time.Second),
	}, job)
}

func TestQueueSender_RefusesContactTypesWithoutAChannel(t *testing.T) {
	publisher := &recordingPublisher{}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), time.Minute)

	assert.True(t, sender.Supports(ContactEmail))
	assert.False(t, sender.Supports(ContactMobile))
	err := sender.SendOTP(context.Background(), testSMSMessage)

	assert.ErrorIs(t, err, ErrUnsupportedContactType)
	assert.Empty(t, publisher.messages())
}

func TestQueueSender_ReportsPublishFailures(t *testing.T) {
	publisher := &recordingPublisher{err: errors.New("publish confirm timeout")}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), time.Minute)

	ctx := dberr.Track(context.Background())
	err := sender.SendOTP(ctx, testEmailMessage)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish confirm timeout")
	answer, ok := dberr.UnavailableOf(ctx)
	require.True(t, ok, "the request is answered with 503, not 500 (#232)")
	assert.Equal(t, dberr.CodeDeliveryUnavailable, answer.Code)
}

// TestQueueSender_NoticesKeepLonger checks that a message without a code,
// such as the reminder to change the anti-phishing code (#375), waits a
// day for a worker, not the lifetime of a code.
func TestQueueSender_NoticesKeepLonger(t *testing.T) {
	publisher := &recordingPublisher{}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), 90*time.Second)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	sender.now = func() time.Time { return now }

	reminder := OTPMessage{ContactType: ContactEmail, Contact: "alice@example.com", MaskedContact: "ali*****@example.com",
		Purpose: PurposeAntiPhishingReminder, AntiPhishingCode: "blue-falcon", Uses: 212, AgeDays: 190}
	require.NoError(t, sender.SendOTP(context.Background(), reminder))

	published := publisher.messages()
	require.Len(t, published, 1)
	assert.Equal(t, "86400000", published[0].msg.Expiration)
	var job otpJob
	require.NoError(t, json.Unmarshal(published[0].msg.Body, &job))
	assert.Equal(t, now.Add(NoticeTTL), job.ExpiresAt)
	assert.Equal(t, reminder, job.message(), "the reminder reaches the worker whole")
}
