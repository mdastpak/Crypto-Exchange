package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// KavenegarName is the name of Kavenegar in notifications.sms.providers.
const KavenegarName = "kavenegar"

// KavenegarDeliverer sends SMS through the Kavenegar verify lookup API, and
// reads their delivery reports (#371). The message text is the template
// approved in the Kavenegar panel, with the code in its %token placeholder.
type KavenegarDeliverer struct {
	apiKey   string
	template string
	// withdrawalTemplate is the template of the codes that confirm a
	// withdrawal (#161); empty is none, and such codes are refused.
	withdrawalTemplate string
	// reminderTemplate is the template of the reminder to change the
	// anti-phishing code (#375); empty is none, and reminders are refused.
	reminderTemplate string
	baseURL          string
	client           *http.Client
}

var _ SMSProvider = (*KavenegarDeliverer)(nil)

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
		apiKey:             cfg.APIKey,
		template:           cfg.OTPTemplate,
		withdrawalTemplate: cfg.WithdrawalTemplate,
		reminderTemplate:   cfg.AntiPhishingReminderTemplate,
		baseURL:            strings.TrimRight(cfg.BaseURL, "/"),
		client:             &http.Client{Timeout: timeout},
	}, nil
}

// Name is KavenegarName.
func (k *KavenegarDeliverer) Name() string { return KavenegarName }

// Supports reports whether a template of purpose is configured.
func (k *KavenegarDeliverer) Supports(purpose string) bool {
	switch purpose {
	case PurposeWithdrawal:
		return k.withdrawalTemplate != ""
	case PurposeAntiPhishingReminder:
		return k.reminderTemplate != ""
	}
	return true
}

// kavenegarResponse is the response envelope: the result, and the messages
// sent or reported. entries is an array of messages, or one message.
type kavenegarResponse struct {
	Return struct {
		Status int `json:"status"`
	} `json:"return"`
	Entries json.RawMessage `json:"entries"`
}

// kavenegarEntry is a message in the entries of a response.
type kavenegarEntry struct {
	MessageID int64 `json:"messageid"`
	Status    int   `json:"status"`
}

func (r kavenegarResponse) entries() []kavenegarEntry {
	var list []kavenegarEntry
	if json.Unmarshal(r.Entries, &list) == nil {
		return list
	}
	var one kavenegarEntry
	if json.Unmarshal(r.Entries, &one) == nil && one.MessageID != 0 {
		return []kavenegarEntry{one}
	}
	return nil
}

// kavenegarStatusText describes the documented error statuses of the verify
// lookup API.
var kavenegarStatusText = map[int]string{
	400: "missing or invalid parameters",
	401: "the account is inactive",
	403: "invalid API key",
	411: "invalid recipient",
	414: "too many message IDs in one request",
	418: "insufficient account credit",
	422: "the parameters contain invalid characters",
	424: "the template does not exist or is not approved",
	426: "the template requires an advanced plan",
	428: "voice delivery requires a numeric token",
	429: "the IP address is limited",
	431: "invalid token format",
	432: "the template has no token placeholder",
	451: "too many requests from this IP address",
}

// kavenegarMessageStatuses are the statuses that refuse the message itself,
// which no other provider would send either.
var kavenegarMessageStatuses = map[int]bool{411: true, 422: true, 431: true}

// Deliver sends msg; it is Send without the ID, for Kavenegar alone.
func (k *KavenegarDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	_, err := k.Send(ctx, msg)
	return err
}

// Send sends msg to the mobile number in msg.Contact and returns its
// message ID.
func (k *KavenegarDeliverer) Send(ctx context.Context, msg OTPMessage) (string, error) {
	form := url.Values{}
	form.Set("receptor", msg.Contact)
	form.Set("token", msg.Code)
	form.Set("template", k.template)
	switch msg.Purpose {
	case PurposeWithdrawal:
		// A code that confirms a withdrawal never goes out with the
		// sign-in text (#161). %token10 may hold spaces, so it carries
		// what the code confirms.
		if k.withdrawalTemplate == "" {
			return "", permanent(errors.New("kavenegar: no withdrawal_template for the codes that confirm withdrawals"))
		}
		form.Set("template", k.withdrawalTemplate)
		form.Set("token10", msg.Details)
	case PurposeAntiPhishingReminder:
		// The reminder carries no code: %token is the number of emails
		// that have shown the anti-phishing code, never the code itself.
		if k.reminderTemplate == "" {
			return "", permanent(errors.New("kavenegar: no anti_phishing_reminder_template for the reminders"))
		}
		form.Set("template", k.reminderTemplate)
		form.Set("token", strconv.FormatInt(msg.Uses, 10))
	}

	body, err := k.call(ctx, http.MethodPost, "verify/lookup.json", form)
	if err != nil {
		return "", err
	}
	if entries := body.entries(); len(entries) > 0 && entries[0].MessageID != 0 {
		return strconv.FormatInt(entries[0].MessageID, 10), nil
	}
	return "", nil
}

// Reports reads the delivery states of messages, at most 500 at a time;
// Kavenegar keeps them for 48 hours.
func (k *KavenegarDeliverer) Reports(ctx context.Context, ids []string) (map[string]DeliveryState, error) {
	out := map[string]DeliveryState{}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		body, err := k.call(ctx, http.MethodGet, "sms/status.json", url.Values{"messageid": {strings.Join(batch, ",")}})
		if err != nil {
			return out, err
		}
		for _, e := range body.entries() {
			out[strconv.FormatInt(e.MessageID, 10)] = kavenegarState(e.Status)
		}
	}
	return out, nil
}

// kavenegarState maps a status of the status table of the Kavenegar API.
func kavenegarState(status int) DeliveryState {
	switch status {
	case 1, 2, 4, 5:
		// Queued, scheduled, or with the carrier.
		return StatePending
	case 10:
		return StateDelivered
	case 6, 11, 13, 14:
		// Failed, not delivered (such as a phone off), cancelled, or
		// blocked.
		return StateFailed
	}
	// 100: an unknown ID, or one older than 48 hours.
	return StateUnknown
}

// call calls method of the API with params and returns the envelope of a
// successful answer. A failure of Kavenegar is a *ProviderError; one of the
// message itself is permanent.
func (k *KavenegarDeliverer) call(ctx context.Context, httpMethod, method string, params url.Values) (kavenegarResponse, error) {
	// The API key is part of the path, so no error below may include the URL.
	endpoint := k.baseURL + "/v1/" + url.PathEscape(k.apiKey) + "/" + method
	var payload io.Reader
	if httpMethod == http.MethodGet {
		endpoint += "?" + params.Encode()
	} else {
		payload = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, httpMethod, endpoint, payload)
	if err != nil {
		return kavenegarResponse{}, permanent(errors.New("kavenegar: cannot build the request"))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := k.client.Do(req)
	if err != nil {
		return kavenegarResponse{}, providerError(KavenegarName, fmt.Errorf("kavenegar: %s", k.withoutKey(err)))
	}
	defer resp.Body.Close()

	var body kavenegarResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&body); err != nil || body.Return.Status == 0 {
		err := fmt.Errorf("kavenegar: unreadable response with HTTP status %d", resp.StatusCode)
		if isPermanentStatus(resp.StatusCode) {
			err = permanent(err)
		}
		return kavenegarResponse{}, providerError(KavenegarName, err)
	}

	status := body.Return.Status
	if status == http.StatusOK {
		return body, nil
	}
	text, ok := kavenegarStatusText[status]
	if !ok {
		text = "unexpected status"
	}
	err = fmt.Errorf("kavenegar: status %d: %s", status, text)
	if kavenegarMessageStatuses[status] {
		return kavenegarResponse{}, permanent(err)
	}
	if isPermanentStatus(status) {
		err = permanent(err)
	}
	return kavenegarResponse{}, providerError(KavenegarName, err)
}

// isPermanentStatus reports whether a request failed in a way that sending it
// again cannot fix. Rate limiting and server errors are worth retrying.
func isPermanentStatus(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests && status != 451
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
