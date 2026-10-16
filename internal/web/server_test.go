package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendardynamics/internal/app"
	"calendardynamics/internal/auth"
	"calendardynamics/internal/config"
	"calendardynamics/internal/db"
	"calendardynamics/internal/jobs"
	"calendardynamics/internal/keys"
	"calendardynamics/internal/messaging"
	"calendardynamics/internal/store"
)

func testServer(t *testing.T) (*httptest.Server, *app.App) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	kr, _ := keys.New(make([]byte, 32))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &app.App{Cfg: config.Config{BaseURL: "http://test"}, Store: store.New(d, kr), Queue: jobs.New(d, log, app.Queues),
		SMS: messaging.Log{L: log}, Email: messaging.Log{L: log}, Log: log}
	a.RegisterJobs()
	srv := httptest.NewServer(New(a).Handler())
	t.Cleanup(srv.Close)
	return srv, a
}

func makeUser(t *testing.T, a *app.App, bizName, email string) {
	t.Helper()
	ctx := context.Background()
	b, err := a.Store.CreateBusiness(ctx, store.NewBusiness{Name: bizName})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("correct horse battery")
	if _, err := a.Store.CreateUser(ctx, b.ID, email, "Owner", hash, "owner"); err != nil {
		t.Fatal(err)
	}
}

func login(t *testing.T, srv *httptest.Server, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"email": {email}, "password": {"correct horse battery"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	return c
}

func call(t *testing.T, c *http.Client, method, u, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "calendar-dynamics")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAPIIsolationAndAuth(t *testing.T) {
	srv, a := testServer(t)
	makeUser(t, a, "Alpha", "a@example.com")
	makeUser(t, a, "Bravo", "b@example.com")
	ca, cb := login(t, srv, "a@example.com"), login(t, srv, "b@example.com")

	start := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	status, ev := call(t, ca, "POST", srv.URL+"/api/events",
		`{"contactName":"Dana Whitfield","type":"job","title":"Water heater","start":"`+start+`","durationMins":120,"notes":"gate 4471"}`)
	if status != http.StatusCreated {
		t.Fatalf("create event: %d %v", status, ev)
	}
	id := ev["id"].(string)
	contactID := ev["contactId"].(string)

	// Bravo can't see Alpha's event or contact.
	if s, _ := call(t, cb, "GET", srv.URL+"/api/events/"+id, ""); s != http.StatusNotFound {
		t.Fatalf("B read A's event: %d", s)
	}
	if s, _ := call(t, cb, "GET", srv.URL+"/api/contacts/"+contactID+"/events", ""); s != http.StatusNotFound {
		t.Fatalf("B read A's timeline: %d", s)
	}
	if s, _ := call(t, cb, "DELETE", srv.URL+"/api/events/"+id, ""); s != http.StatusNotFound {
		t.Fatalf("B deleted A's event: %d", s)
	}
	if s, out := call(t, cb, "POST", srv.URL+"/api/events", `{"contactId":"`+contactID+`","type":"job","title":"x","start":"`+start+`"}`); s != http.StatusBadRequest {
		t.Fatalf("B attached to A's contact: %d %v", s, out)
	}
	// Alpha still sees it.
	if s, got := call(t, ca, "GET", srv.URL+"/api/events/"+id, ""); s != 200 || got["notes"] != "gate 4471" {
		t.Fatalf("A lost its event: %d %v", s, got)
	}

	// No session -> 401.
	if s, _ := call(t, http.DefaultClient, "GET", srv.URL+"/api/me", ""); s != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/me: %d", s)
	}
	// Missing X-Requested-With on a write -> blocked (CSRF guard).
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/events/"+id, nil)
	resp, err := ca.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without header: %d", resp.StatusCode)
	}
	// Cross-site Origin -> blocked.
	req, _ = http.NewRequest("POST", srv.URL+"/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = ca.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
}

func TestFollowupAPIValidation(t *testing.T) {
	srv, a := testServer(t)
	makeUser(t, a, "Alpha", "a@example.com")
	c := login(t, srv, "a@example.com")
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	s, out := call(t, c, "POST", srv.URL+"/api/events", `{"contactName":"Ray","type":"followup","title":"Nudge","start":"`+start+`","channel":"sms"}`)
	if s != http.StatusBadRequest || !strings.Contains(out["error"].(string), "message") {
		t.Fatalf("expected validation error, got %d %v", s, out)
	}
	s, out = call(t, c, "POST", srv.URL+"/api/events", `{"contactName":"Ray","type":"followup","title":"Nudge","start":"`+start+`","channel":"sms","message":"Hi Ray"}`)
	if s != http.StatusCreated || out["sendStatus"] != "scheduled" {
		t.Fatalf("expected scheduled follow-up, got %d %v", s, out)
	}
	var n int
	_ = a.Store.DB.QueryRow(`SELECT count(*) FROM jobs WHERE kind = 'send_followup' AND state = 'available'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected a queued send job, got %d", n)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	srv, a := testServer(t)
	makeUser(t, a, "Alpha", "a@example.com")
	resp, err := http.PostForm(srv.URL+"/login", url.Values{"email": {"a@example.com"}, "password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
