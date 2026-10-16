package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"calendardynamics/internal/config"
	"calendardynamics/internal/db"
	"calendardynamics/internal/jobs"
	"calendardynamics/internal/keys"
	"calendardynamics/internal/store"
)

type fakeSMS struct {
	mu   sync.Mutex
	sent []string
	fail error
}

func (f *fakeSMS) SendSMS(_ context.Context, from, to, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.sent = append(f.sent, to+": "+body)
	return "SM123", nil
}

func (f *fakeSMS) SendEmail(_ context.Context, from, to, subject, body string) (string, error) {
	return f.SendSMS(context.Background(), from, to, body)
}

func newApp(t *testing.T) (*App, *fakeSMS, *store.Tenant) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	kr, _ := keys.New(make([]byte, 32))
	s := store.New(d, kr)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &fakeSMS{}
	a := &App{Cfg: config.Config{TwilioFrom: "+18005550000"}, Store: s, Queue: jobs.New(d, log, Queues), SMS: fake, Email: fake, Log: log}
	a.RegisterJobs()
	ctx := context.Background()
	b, err := s.CreateBusiness(ctx, store.NewBusiness{Name: "Alpha", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	// No quiet hours in tests.
	_, _ = d.Exec(`UPDATE businesses SET quiet_start = 0, quiet_end = 0 WHERE id = ?`, b.ID)
	tn, err := s.Tenant(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, fake, tn
}

func run(t *testing.T, a *App, kind string, args any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	h := map[string]jobs.Handler{"send_followup": a.sendFollowup, "inbound_sms": a.inboundSMS, "import_contacts": a.importContacts}[kind]
	return h(context.Background(), jobs.Job{Kind: kind, Args: raw, Attempts: 1, MaxAttempts: 8})
}

func TestFollowupSendsOnceWithOptOutLanguage(t *testing.T) {
	a, fake, tn := newApp(t)
	ctx := context.Background()
	c := &store.Contact{Name: "Dana", Phone: "8475550142", SMSConsent: true}
	if err := tn.SaveContact(ctx, c, "test"); err != nil {
		t.Fatal(err)
	}
	ev := &store.Event{ContactID: c.ID, Type: "followup", Title: "Quote reminder", Start: time.Now(), Channel: "sms", Message: "Any questions about your quote?"}
	if err := tn.SaveEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	args := followupArgs{tn.ID(), ev.ID}
	if err := run(t, a, "send_followup", args); err != nil {
		t.Fatal(err)
	}
	// Running the same job again (e.g. a retry) must not send twice.
	var cancel jobs.Cancel
	if err := run(t, a, "send_followup", args); !errors.As(err, &cancel) {
		t.Fatalf("second run should be cancelled, got %v", err)
	}
	if len(fake.sent) != 1 || !strings.Contains(fake.sent[0], "Reply STOP to opt out") || !strings.HasPrefix(fake.sent[0], "+18475550142") {
		t.Fatalf("unexpected sends: %v", fake.sent)
	}
	got, _ := tn.GetEvent(ctx, ev.ID)
	if got.SendStatus != "sent" {
		t.Fatalf("status = %q", got.SendStatus)
	}
}

func TestFollowupSkipsWithoutConsent(t *testing.T) {
	a, fake, tn := newApp(t)
	ctx := context.Background()
	c := &store.Contact{Name: "Ray", Phone: "8475550127"} // no consent
	_ = tn.SaveContact(ctx, c, "")
	ev := &store.Event{ContactID: c.ID, Type: "followup", Title: "x", Start: time.Now(), Channel: "sms", Message: "hi"}
	_ = tn.SaveEvent(ctx, ev)
	_ = run(t, a, "send_followup", followupArgs{tn.ID(), ev.ID})
	got, _ := tn.GetEvent(ctx, ev.ID)
	if len(fake.sent) != 0 || got.SendStatus != "skipped" {
		t.Fatalf("sent without consent: %v %q", fake.sent, got.SendStatus)
	}
}

func TestFollowupMovedLaterIsRescheduled(t *testing.T) {
	a, fake, tn := newApp(t)
	ctx := context.Background()
	c := &store.Contact{Name: "Tom", Phone: "8475550193", SMSConsent: true}
	_ = tn.SaveContact(ctx, c, "")
	ev := &store.Event{ContactID: c.ID, Type: "followup", Title: "x", Start: time.Now().Add(2 * time.Hour), Channel: "sms", Message: "hi"}
	_ = tn.SaveEvent(ctx, ev)
	var r jobs.Reschedule
	if err := run(t, a, "send_followup", followupArgs{tn.ID(), ev.ID}); !errors.As(err, &r) {
		t.Fatalf("expected reschedule, got %v", err)
	}
	if len(fake.sent) != 0 {
		t.Fatal("sent early")
	}
}

func TestInboundStopOptsOutAndPausesFollowups(t *testing.T) {
	a, _, tn := newApp(t)
	ctx := context.Background()
	c := &store.Contact{Name: "Priya", Phone: "+12245550115", SMSConsent: true}
	_ = tn.SaveContact(ctx, c, "")
	ev := &store.Event{ContactID: c.ID, Type: "followup", Title: "x", Start: time.Now().Add(24 * time.Hour), Channel: "sms", Message: "hi"}
	_ = tn.SaveEvent(ctx, ev)

	if err := run(t, a, "inbound_sms", InboundSMS{From: "+12245550115", To: "+18005550000", Body: "STOP", SID: "SM1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := tn.GetContact(ctx, c.ID)
	ev2, _ := tn.GetEvent(ctx, ev.ID)
	if !got.OptedOut || ev2.SendStatus != "skipped" {
		t.Fatalf("opted out=%v followup status=%q", got.OptedOut, ev2.SendStatus)
	}
	timeline, _ := tn.ListContactEvents(ctx, c.ID)
	if len(timeline) != 2 {
		t.Fatalf("expected an opt-out note on the timeline, got %d events", len(timeline))
	}
}

func TestParseContactsCSV(t *testing.T) {
	in := "\ufeffFirst Name,Last Name,Mobile,E-mail,Street,City\nDana,Whitfield,(847) 555-0142,dana@example.com,412 Maple St,Arlington Heights\n,,,,,\n"
	rows, err := ParseContactsCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "Dana Whitfield" || rows[0].Phone != "(847) 555-0142" || rows[0].Address != "412 Maple St, Arlington Heights" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if _, err := ParseContactsCSV(strings.NewReader("foo,bar\n1,2\n")); err == nil {
		t.Fatal("expected error for missing name column")
	}
}
