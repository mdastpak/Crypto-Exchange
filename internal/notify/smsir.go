package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// SMSIRName is the name of sms.ir in notifications.sms.providers.
const SMSIRName = "smsir"

// The parameters of the sms.ir templates: #CODE# in each template of a
// code, #AMOUNT# and #DEST# in the one of withdrawals, and #COUNT# in the
// reminder to change the anti-phishing code.
const (
	SMSIRParamCode   = "CODE"
	SMSIRParamAmount = "AMOUNT"
	SMSIRParamDest   = "DEST"
	SMSIRParamCount  = "COUNT"
)

// smsirMaxValue is the longest value of a template parameter that sms.ir
// takes (status 114).
const smsirMaxValue = 25

// SMSIRDeliverer sends SMS through the verify API of sms.ir, and reads
// their delivery reports (#371). The templates are defined in its panel
// and named here by their IDs.
type SMSIRDeliverer struct {
	apiKey                          string
	otpTemplate, withdrawalTemplate int
	reminderTemplate                int
	baseURL                         string
	client                          *http.Client
}

var _ SMSProvider = (*SMSIRDeliverer)(nil)

// NewSMSIRDeliverer validates cfg and returns a deliverer for it.
func NewSMSIRDeliverer(cfg config.SMSIRConfig) (*SMSIRDeliverer, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("api_key is required (or set SMSIR_API_KEY)")
	}
	if cfg.OTPTemplateID <= 0 {
		return nil, errors.New("otp_template_id is required")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("base_url must be an absolute URL, got %q", cfg.BaseURL)
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &SMSIRDeliverer{
		apiKey:             cfg.APIKey,
		otpTemplate:        cfg.OTPTemplateID,
		withdrawalTemplate: cfg.WithdrawalTemplateID,
		reminderTemplate:   cfg.AntiPhishingReminderTemplateID,
		baseURL:            strings.TrimRight(cfg.BaseURL, "/"),
		client:             &http.Client{Timeout: timeout},
	}, nil
}

// Name is SMSIRName.
func (s *SMSIRDeliverer) Name() string { return SMSIRName }

// Supports reports whether a template of purpose is configured.
func (s *SMSIRDeliverer) Supports(purpose string) bool {
	switch purpose {
	case PurposeWithdrawal:
		return s.withdrawalTemplate > 0
	case PurposeAntiPhishingReminder:
		return s.reminderTemplate > 0
	}
	return true
}

// smsirParam is a parameter of a template.
type smsirParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// smsirResponse is the response envelope: status 1 is success.
type smsirResponse struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// smsirMessageStatuses are the statuses that refuse the message itself: an
// invalid mobile number (104), or one on the blacklist (115).
var smsirMessageStatuses = map[int]bool{104: true, 115: true}

// smsirRetryStatuses are the failures of sms.ir that may pass: a system
// error, and too many requests.
var smsirRetryStatuses = map[int]bool{0: true, 20: true, 118: true}

// smsirMobile returns a mobile number as sms.ir takes it: its ten digits,
// without the leading 0 or the country code.
func smsirMobile(mobile string) string {
	mobile = strings.TrimPrefix(mobile, "+")
	switch {
	case strings.HasPrefix(mobile, "98") && len(mobile) == 12:
		return mobile[2:]
	case strings.HasPrefix(mobile, "0") && len(mobile) == 11:
		return mobile[1:]
	}
	return mobile
}

// params returns the template and the parameters of msg.
func (s *SMSIRDeliverer) params(msg OTPMessage) (int, []smsirParam, error) {
	switch msg.Purpose {
	case PurposeWithdrawal:
		if s.withdrawalTemplate <= 0 {
			return 0, nil, errors.New("smsir: no withdrawal_template_id for the codes that confirm withdrawals")
		}
		// A code that confirms a withdrawal never goes out with the
		// sign-in text (#161), and says what it confirms.
		return s.withdrawalTemplate, []smsirParam{
			{SMSIRParamCode, msg.Code}, {SMSIRParamAmount, msg.Amount}, {SMSIRParamDest, msg.Destination},
		}, nil
	case PurposeAntiPhishingReminder:
		if s.reminderTemplate <= 0 {
			return 0, nil, errors.New("smsir: no anti_phishing_reminder_template_id for the reminders")
		}
		// The reminder carries the number of emails, never the code.
		return s.reminderTemplate, []smsirParam{{SMSIRParamCount, strconv.FormatInt(msg.Uses, 10)}}, nil
	}
	return s.otpTemplate, []smsirParam{{SMSIRParamCode, msg.Code}}, nil
}

// Deliver sends msg; it is Send without the ID, for sms.ir alone.
func (s *SMSIRDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	_, err := s.Send(ctx, msg)
	return err
}

// Send sends msg to the mobile number in msg.Contact and returns its
// message ID.
func (s *SMSIRDeliverer) Send(ctx context.Context, msg OTPMessage) (string, error) {
	template, params, err := s.params(msg)
	if err != nil {
		return "", permanent(err)
	}
	for _, p := range params {
		if len([]rune(p.Value)) > smsirMaxValue {
			return "", providerError(SMSIRName, permanent(fmt.Errorf("smsir: the %s parameter is longer than %d characters", p.Name, smsirMaxValue)))
		}
	}
	payload, err := json.Marshal(map[string]any{"mobile": smsirMobile(msg.Contact), "templateId": template, "parameters": params})
	if err != nil {
		return "", permanent(err)
	}
	body, err := s.call(ctx, http.MethodPost, "/v1/send/verify", payload)
	if err != nil {
		return "", err
	}
	var data struct {
		MessageID int64 `json:"messageId"`
	}
	if json.Unmarshal(body.Data, &data) == nil && data.MessageID != 0 {
		return strconv.FormatInt(data.MessageID, 10), nil
	}
	return "", nil
}

// Reports reads the delivery states of messages, one request each, a few
// at a time.
func (s *SMSIRDeliverer) Reports(ctx context.Context, ids []string) (map[string]DeliveryState, error) {
	out := map[string]DeliveryState{}
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, id := range ids {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			state, err := s.report(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[id] = state
		}()
	}
	wg.Wait()
	return out, firstErr
}

// report reads the delivery state of one message.
func (s *SMSIRDeliverer) report(ctx context.Context, id string) (DeliveryState, error) {
	body, err := s.call(ctx, http.MethodGet, "/v1/send/"+id, nil)
	if err != nil {
		var status *smsirStatusError
		if errors.As(err, &status) && status.status == 111 {
			// A message that sms.ir has not recorded, such as one of the
			// sandbox.
			return StateUnknown, nil
		}
		return StatePending, err
	}
	var data struct {
		DeliveryState *int `json:"deliveryState"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		return StatePending, providerError(SMSIRName, errors.New("smsir: unreadable report"))
	}
	if data.DeliveryState == nil {
		return StatePending, nil
	}
	return smsirState(*data.DeliveryState), nil
}

// smsirState maps a deliveryState of the sms.ir API.
func smsirState(state int) DeliveryState {
	switch state {
	case 1:
		return StateDelivered
	case 3, 5:
		// Processing at the carrier, or reached the carrier.
		return StatePending
	case 2, 4, 6, 7:
		// Not delivered, did not reach the carrier, an error, or the
		// blacklist.
		return StateFailed
	}
	return StateUnknown
}

// call calls the API and returns the envelope of a successful answer. A
// failure of sms.ir is a *ProviderError; one of the message itself is
// permanent. The key is a header, so it never reaches an error.
func (s *SMSIRDeliverer) call(ctx context.Context, method, path string, payload []byte) (smsirResponse, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return smsirResponse{}, permanent(errors.New("smsir: cannot build the request"))
	}
	req.Header.Set("X-API-KEY", s.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return smsirResponse{}, providerError(SMSIRName, fmt.Errorf("smsir: %s", s.withoutKey(err)))
	}
	defer resp.Body.Close()

	var body smsirResponse
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	switch {
	case decodeErr == nil && body.Status == 1 && resp.StatusCode == http.StatusOK:
		return body, nil
	case decodeErr != nil:
		err := fmt.Errorf("smsir: unreadable response with HTTP status %d", resp.StatusCode)
		if isPermanentStatus(resp.StatusCode) {
			err = permanent(err)
		}
		return smsirResponse{}, providerError(SMSIRName, err)
	}
	err = &smsirStatusError{status: body.Status, http: resp.StatusCode}
	switch {
	case smsirMessageStatuses[body.Status]:
		return smsirResponse{}, permanent(err)
	case smsirRetryStatuses[body.Status] || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return smsirResponse{}, providerError(SMSIRName, err)
	}
	// The key, the account, the credit, the template, or the line.
	return smsirResponse{}, providerError(SMSIRName, permanent(err))
}

// withoutKey returns the text of a client error without the key, should it
// ever appear.
func (s *SMSIRDeliverer) withoutKey(err error) string {
	return strings.ReplaceAll(err.Error(), s.apiKey, "[api-key]")
}

// smsirStatusError is an answer of sms.ir with a status other than 1.
type smsirStatusError struct{ status, http int }

func (e *smsirStatusError) Error() string {
	return fmt.Sprintf("smsir: status %d with HTTP status %d", e.status, e.http)
}
