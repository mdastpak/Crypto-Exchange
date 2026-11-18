package kyc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

// ObjectStore keeps the documents: their content as the service encrypted
// it, by key.
type ObjectStore interface {
	Put(ctx context.Context, key string, content []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// ErrObjectNotFound is an object that the store does not have.
var ErrObjectNotFound = errors.New("kyc: the object is not in the storage")

// S3Config is an S3-compatible storage: MinIO, ArvanCloud, AWS, and others.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// UseTLS reaches the endpoint over HTTPS.
	UseTLS bool
	// ServerSideEncryption asks the storage to encrypt the objects with
	// its own keys (SSE-S3) as well; the content is encrypted by the
	// service before it leaves.
	ServerSideEncryption bool
}

// S3 is an ObjectStore in a bucket of an S3-compatible storage.
type S3 struct {
	client *minio.Client
	bucket string
	sse    encrypt.ServerSide
}

// NewS3 connects to the storage of cfg. It checks nothing over the network:
// a bucket that does not exist fails at the first write.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("kyc: the storage needs an endpoint and a bucket")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("kyc: the storage: %w", err)
	}
	s := &S3{client: client, bucket: cfg.Bucket}
	if cfg.ServerSideEncryption {
		s.sse = encrypt.NewSSE()
	}
	return s, nil
}

// Put implements ObjectStore.
func (s *S3) Put(ctx context.Context, key string, content []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: contentType, ServerSideEncryption: s.sse})
	if err != nil {
		return fmt.Errorf("kyc: store %s: %w", key, err)
	}
	return nil
}

// Get implements ObjectStore.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("kyc: read %s: %w", key, err)
	}
	defer obj.Close()
	content, err := io.ReadAll(io.LimitReader(obj, MaxDocumentSize*2))
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("kyc: read %s: %w", key, err)
	}
	return content, nil
}

// Delete implements ObjectStore.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("kyc: remove %s: %w", key, err)
	}
	return nil
}

// Memory is an ObjectStore in memory, for tests.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// Put implements ObjectStore.
func (m *Memory) Put(_ context.Context, key string, content []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[key] = append([]byte(nil), content...)
	return nil
}

// Get implements ObjectStore.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	content, ok := m.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), content...), nil
}

// Delete implements ObjectStore.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Len is the number of objects, for tests.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// Replace changes an object in place, as a compromised storage could, for
// tests.
func (m *Memory) Replace(key string, content []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = content
}
