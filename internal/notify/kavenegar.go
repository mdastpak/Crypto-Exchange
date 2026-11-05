package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
)

// KavenegarDeliverer sends sign-in codes by SMS through the Kavenegar verify
// lookup API. The message text is the template approved in the Kavenegar
// panel, with the code in its %token placeholder.
type KavenegarDeliverer struct {
	apiKey   string
	template string
	baseURL  string
	client   *http.Client
}

// NewKavenegarDeliverer validates cfg and returns a deliverer for it.
func NewKavenegarDeliverer(cfg config.KavenegarConfig) (*KavenegarDeliverer, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("api_key is required (or set KAVENEGAR_API_KEY)")
	}
	if cfg.OTPTemplate == "" {
		return nil, errors.New("otp_template is required")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("base_url must be an absolute URL, got %q", cfg.BaseURL)
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &KavenegarDeliverer{
		apiKey:   cfg.APIKey,
		template: cfg.OTPTemplate,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// kavenegarResponse is the part of the response envelope that reports the
// result. The HTTP status mirrors return.status.
type kavenegarResponse struct {
	Return struct {
		Status int `json:"status"`
	} `json:"return"`
}

// kavenegarStatusText describes the documented error statuses of the verify
// lookup API.
var kavenegarStatusText = map[int]string{
	400: "missing or invalid parameters",
	401: "the account is inactive",
	403: "invalid API key",
	411: "invalid recipient",
	418: "insufficient account credit",
	422: "the parameters contain invalid characters",
	424: "the template does not exist or is not approved",
	426: "the template requires an advanced plan",
	428: "voice delivery requires a numeric token",
	431: "invalid token format",
	432: "the template has no token placeholder",
}

// Deliver sends the code to the mobile number in msg.Contact.
func (k *KavenegarDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	form := url.Values{}
	form.Set("receptor", msg.Contact)
	form.Set("token", msg.Code)
	form.Set("template", k.template)

	// The API key is part of the path, so no error below may include the URL.
	endpoint := k.baseURL + "/v1/" + url.PathEscape(k.apiKey) + "/verify/lookup.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return permanent(errors.New("kavenegar: cannot build the request"))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("kavenegar: %s", k.withoutKey(err))
	}
	defer resp.Body.Close()

	var body kavenegarResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil || body.Return.Status == 0 {
		err := fmt.Errorf("kavenegar: unreadable response with HTTP status %d", resp.StatusCode)
		if isPermanentStatus(resp.StatusCode) {
			return permanent(err)
		}
		return err
	}

	status := body.Return.Status
	if status == http.StatusOK {
		return nil
	}
	text, ok := kavenegarStatusText[status]
	if !ok {
		text = "unexpected status"
	}
	err = fmt.Errorf("kavenegar: status %d: %s", status, text)
	if isPermanentStatus(status) {
		return permanent(err)
	}
	return err
}

// isPermanentStatus reports whether a request failed in a way that sending it
// again cannot fix. Rate limiting and server errors are worth retrying.
func isPermanentStatus(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests
}

// withoutKey returns the text of a client error without the request URL, which
// contains the API key.
func (k *KavenegarDeliverer) withoutKey(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	text := strings.ReplaceAll(err.Error(), k.apiKey, "[api-key]")
	return strings.ReplaceAll(text, url.PathEscape(k.apiKey), "[api-key]")
}
