package messaging

import (
	"net/url"
	"testing"
	"time"
)

func TestQuietUntil(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, loc) }

	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{at(14, 0), at(14, 0)},                  // afternoon: send now
		{at(21, 30), at(8, 0).AddDate(0, 0, 1)}, // evening: next morning
		{at(6, 15), at(8, 0)},                   // early morning: 8am today
		{at(8, 0), at(8, 0)},                    // exactly at end: send
		{at(20, 0), at(8, 0).AddDate(0, 0, 1)},  // exactly at start: wait
	}
	for _, c := range cases {
		got := QuietUntil(c.now, loc, 20, 8)
		if !got.Equal(c.want) {
			t.Errorf("QuietUntil(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestKeyword(t *testing.T) {
	for in, want := range map[string]string{"STOP": "stop", " stop. ": "stop", "Unsubscribe": "stop", "start": "start", "Yes please": "", "stop by tuesday?": ""} {
		if got := Keyword(in); got != want {
			t.Errorf("Keyword(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTwilioSignature(t *testing.T) {
	// Worked example from https://www.twilio.com/docs/usage/security
	form := url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Caller":  {"+14158675310"},
		"Digits":  {"1234"},
		"From":    {"+14158675310"},
		"To":      {"+18005551212"},
	}
	ok := ValidTwilioSignature("12345", "https://example.com/myapp.php?foo=1&bar=2", form, "L/OH5YylLD5NRKLltdqwSvS0BnU=")
	if !ok {
		t.Fatal("expected valid signature")
	}
	if ValidTwilioSignature("12345", "https://example.com/myapp.php?foo=1&bar=2", form, "nope") {
		t.Fatal("expected invalid signature")
	}
}
