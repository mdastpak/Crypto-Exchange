package notify

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// DeliveryLog remembers the sign-in code jobs that were delivered (#232).
// The broker hands a job out again when its worker stopped after sending
// the code but before acknowledging it; without the log, the code would be
// sent twice, which costs an SMS and makes the user wonder which code
// counts.
type DeliveryLog interface {
	// Delivered reports whether the job with id was delivered.
	Delivered(ctx context.Context, id string) (bool, error)
	// MarkDelivered records the job with id as delivered for ttl.
	MarkDelivered(ctx context.Context, id string, ttl time.Duration) error
}

// RedisDeliveryLog keeps the delivery log in Redis, which every instance
// shares, so that a job redelivered to another instance is recognized too.
// The keys hold only the job's ID, never the code or the contact.
type RedisDeliveryLog struct {
	Client redis.UniversalClient
	// Prefix starts every key; NewRedisDeliveryLog sets
	// "notify:otp:delivered:".
	Prefix string
}

// NewRedisDeliveryLog returns a delivery log on client.
func NewRedisDeliveryLog(client redis.UniversalClient) *RedisDeliveryLog {
	return &RedisDeliveryLog{Client: client, Prefix: "notify:otp:delivered:"}
}

func (l *RedisDeliveryLog) Delivered(ctx context.Context, id string) (bool, error) {
	err := l.Client.Get(ctx, l.Prefix+id).Err()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, redis.Nil):
		return false, nil
	default:
		return false, err
	}
}

func (l *RedisDeliveryLog) MarkDelivered(ctx context.Context, id string, ttl time.Duration) error {
	return l.Client.Set(ctx, l.Prefix+id, 1, ttl).Err()
}
