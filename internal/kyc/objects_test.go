package kyc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3 keeps the objects of one bucket, and the headers of each write.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	headers map[string]http.Header
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			body = unchunk(body)
		}
		f.objects[r.URL.Path], f.headers[r.URL.Path] = body, r.Header.Clone()
		w.Header().Set("ETag", `"etag"`)
	case http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
			return
		}
		w.Header().Set("Last-Modified", "Wed, 18 Nov 2026 10:00:00 GMT")
		w.Header().Set("ETag", `"etag"`)
		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}

// unchunk decodes a body of signed chunks, "size;chunk-signature=...\r\n",
// the data, and "\r\n", until a chunk of size 0.
func unchunk(body []byte) []byte {
	var out []byte
	for len(body) > 0 {
		line, rest, ok := strings.Cut(string(body), "\r\n")
		if !ok {
			break
		}
		sizeHex, _, _ := strings.Cut(line, ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil || size == 0 {
			break
		}
		out = append(out, rest[:size]...)
		body = []byte(rest[size+2:])
	}
	return out
}

// The S3 store signs its requests, asks for server-side encryption, and
// reads back what it wrote (#72).
func TestS3(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}, headers: map[string]http.Header{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	s, err := NewS3(S3Config{Endpoint: strings.TrimPrefix(server.URL, "http://"), Region: "us-east-1", Bucket: "kyc",
		AccessKey: "access", SecretKey: "secret", ServerSideEncryption: true})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "kyc/a/b", []byte("sealed"), "application/octet-stream"))
	assert.Equal(t, []byte("sealed"), fake.objects["/kyc/kyc/a/b"])
	assert.Equal(t, "AES256", fake.headers["/kyc/kyc/a/b"].Get("X-Amz-Server-Side-Encryption"))
	got, err := s.Get(ctx, "kyc/a/b")
	require.NoError(t, err)
	assert.Equal(t, []byte("sealed"), got)

	require.NoError(t, s.Delete(ctx, "kyc/a/b"))
	_, err = s.Get(ctx, "kyc/a/b")
	assert.ErrorIs(t, err, ErrObjectNotFound)

	_, err = NewS3(S3Config{Endpoint: "minio:9000"})
	assert.Error(t, err, "a bucket is required")
}
