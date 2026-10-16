// Package messaging sends texts and emails. Providers are plain HTTPS calls
// (no SDKs). Without credentials, the Log provider prints messages instead,
// which is what you want for local development and demos.
package messaging

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type SMS interface {
	SendSMS(ctx context.Context, from, to, body string) (providerID string, err error)
}

type Email interface {
	SendEmail(ctx context.Context, from, to, subject, body string) (providerID string, err error)
}

// PermanentError means retrying won't help (bad number, unsubscribed, etc.).
type PermanentError struct{ Msg string }

func (e PermanentError) Error() string { return e.Msg }

func IsPermanent(err error) bool {
	var p PermanentError
	return errors.As(err, &p)
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// ---------------------------------------------------------------- log (dev)

type Log struct{ L *slog.Logger }

func (l Log) SendSMS(_ context.Context, from, to, body string) (string, error) {
	l.L.Info("SMS (not sent — no Twilio credentials)", "from", from, "to", to, "body", body)
	return "log-" + fmt.Sprint(time.Now().UnixNano()), nil
}

func (l Log) SendEmail(_ context.Context, from, to, subject, body string) (string, error) {
	l.L.Info("EMAIL (not sent — no Postmark token)", "from", from, "to", to, "subject", subject, "body", body)
	return "log-" + fmt.Sprint(time.Now().UnixNano()), nil
}

// ---------------------------------------------------------------- Twilio

type Twilio struct {
	AccountSID string
	AuthToken  string
}

func (t Twilio) SendSMS(ctx context.Context, from, to, body string) (string, error) {
	form := url.Values{"From": {from}, "To": {to}, "Body": {body}}
	endpoint := "https://api.twilio.com/2010-04-01/Accounts/" + url.PathEscape(t.AccountSID) + "/Messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(t.AccountSID, t.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err // network errors are retried
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		SID     string `json:"sid"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.Unmarshal(raw, &out)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return out.SID, nil
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return "", fmt.Errorf("twilio %d: %s", resp.StatusCode, out.Message)
	default:
		return "", PermanentError{Msg: fmt.Sprintf("twilio %d (code %d): %s", resp.StatusCode, out.Code, out.Message)}
	}
}

// ValidTwilioSignature checks the X-Twilio-Signature header on an inbound
// webhook: base64(HMAC-SHA1(authToken, fullURL + sorted key/value pairs)).
func ValidTwilioSignature(authToken, fullURL string, form url.Values, signature string) bool {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		for _, v := range form[k] {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(want), []byte(signature)) == 1
}

// ---------------------------------------------------------------- Postmark

type Postmark struct{ ServerToken string }

func (p Postmark) SendEmail(ctx context.Context, from, to, subject, body string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"From": from, "To": to, "Subject": subject, "TextBody": body, "MessageStream": "outbound",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.postmarkapp.com/email", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Postmark-Server-Token", p.ServerToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		MessageID string `json:"MessageID"`
		ErrorCode int    `json:"ErrorCode"`
		Message   string `json:"Message"`
	}
	_ = json.Unmarshal(raw, &out)
	switch {
	case resp.StatusCode == 200 && out.ErrorCode == 0:
		return out.MessageID, nil
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return "", fmt.Errorf("postmark %d: %s", resp.StatusCode, out.Message)
	default:
		return "", PermanentError{Msg: fmt.Sprintf("postmark %d (code %d): %s", resp.StatusCode, out.ErrorCode, out.Message)}
	}
}

// ---------------------------------------------------------------- helpers

var stopWords = map[string]bool{"STOP": true, "STOPALL": true, "UNSUBSCRIBE": true, "CANCEL": true, "END": true, "QUIT": true}
var startWords = map[string]bool{"START": true, "UNSTOP": true, "YES": false}

// Keyword classifies an inbound text as an opt-out, opt-in, or normal reply.
func Keyword(body string) string {
	w := strings.ToUpper(strings.Trim(strings.TrimSpace(body), ".!"))
	switch {
	case stopWords[w]:
		return "stop"
	case startWords[w]:
		return "start"
	}
	return ""
}

// QuietUntil returns when a message may be sent. If `now` falls inside the
// business's quiet hours (local time), it returns the end of quiet hours;
// otherwise it returns now unchanged.
func QuietUntil(now time.Time, loc *time.Location, quietStart, quietEnd int) time.Time {
	if quietStart == quietEnd {
		return now
	}
	local := now.In(loc)
	h := local.Hour()
	var quiet bool
	if quietStart > quietEnd { // e.g. 20 -> 8, wraps midnight
		quiet = h >= quietStart || h < quietEnd
	} else {
		quiet = h >= quietStart && h < quietEnd
	}
	if !quiet {
		return now
	}
	end := time.Date(local.Year(), local.Month(), local.Day(), quietEnd, 0, 0, 0, loc)
	if !end.After(local) {
		end = end.AddDate(0, 0, 1)
	}
	return end
}
