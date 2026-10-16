package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendardynamics/internal/db"
	"calendardynamics/internal/keys"
	"calendardynamics/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	kr, err := keys.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store.New(d, kr)
}

func newTenant(t *testing.T, s *store.Store, name string) *store.Tenant {
	t.Helper()
	ctx := context.Background()
	b, err := s.CreateBusiness(ctx, store.NewBusiness{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	tn, err := s.Tenant(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := newTenant(t, s, "Alpha Plumbing")
	b := newTenant(t, s, "Bravo Electric")

	ca := &store.Contact{Name: "Dana Whitfield", Phone: "(847) 555-0142"}
	if err := a.SaveContact(ctx, ca, "test"); err != nil {
		t.Fatal(err)
	}
	ea := &store.Event{ContactID: ca.ID, Type: "job", Title: "Water heater", Start: time.Now().Add(time.Hour), DurationMins: 60, Notes: "gate code 4471"}
	if err := a.SaveEvent(ctx, ea); err != nil {
		t.Fatal(err)
	}

	// B cannot read A's contact or event by ID.
	if _, err := b.GetContact(ctx, ca.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B read A's contact: %v", err)
	}
	if _, err := b.GetEvent(ctx, ea.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B read A's event: %v", err)
	}
	// B's lists don't include A's data.
	cs, _ := b.ListContacts(ctx, "", 0)
	es, _ := b.ListEvents(ctx, time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	if len(cs) != 0 || len(es) != 0 {
		t.Fatalf("B sees A's data: %d contacts, %d events", len(cs), len(es))
	}
	if _, err := b.FindContactByPhone(ctx, "8475550142"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("B found A's contact by phone")
	}
	// B cannot attach an event to A's contact.
	if err := b.SaveEvent(ctx, &store.Event{ContactID: ca.ID, Type: "job", Title: "x", Start: time.Now()}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("B attached event to A's contact: %v", err)
	}
	// B cannot update or delete A's records.
	if err := b.SaveEvent(ctx, &store.Event{ID: ea.ID, ContactID: ca.ID, Type: "job", Title: "x", Start: time.Now()}); err == nil {
		t.Fatal("B updated A's event")
	}
	if err := b.DeleteEvent(ctx, ea.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("B deleted A's event")
	}
	ca2 := *ca
	ca2.Name = "Hacked"
	if err := b.SaveContact(ctx, &ca2, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("B updated A's contact")
	}

	// A still sees its data, decrypted.
	got, err := a.GetEvent(ctx, ea.ID)
	if err != nil || got.Notes != "gate code 4471" || got.ContactName != "Dana Whitfield" {
		t.Fatalf("A lost its data: %+v %v", got, err)
	}
}

func TestNotesEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := newTenant(t, s, "Alpha")
	c := &store.Contact{Name: "Ray"}
	if err := a.SaveContact(ctx, c, ""); err != nil {
		t.Fatal(err)
	}
	e := &store.Event{ContactID: c.ID, Type: "note", Title: "n", Start: time.Now(), Notes: "SECRET-NOTE"}
	if err := a.SaveEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.DB.QueryRow(`SELECT notes_enc FROM events WHERE id = ?`, e.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || string(raw) == "SECRET-NOTE" || containsBytes(raw, []byte("SECRET")) {
		t.Fatal("notes are not encrypted at rest")
	}
}

func TestDeleteBusinessShredsData(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := newTenant(t, s, "Alpha")
	c := &store.Contact{Name: "Ray"}
	_ = a.SaveContact(ctx, c, "")
	if err := s.DeleteBusiness(ctx, a.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenant(ctx, a.ID()); err == nil {
		t.Fatal("deleted business can still be opened")
	}
	var n int
	_ = s.DB.QueryRow(`SELECT count(*) FROM contacts WHERE business_id = ?`, a.ID()).Scan(&n)
	var key []byte
	_ = s.DB.QueryRow(`SELECT wrapped_key FROM businesses WHERE id = ?`, a.ID()).Scan(&key)
	if n != 0 || len(key) != 0 {
		t.Fatalf("data or key survived deletion: contacts=%d key=%d bytes", n, len(key))
	}
}

func TestOutboundClaimIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := newTenant(t, s, "Alpha")
	m1, created1, err := a.ClaimOutbound(ctx, "evt:1", "con_1", "evt_1", "sms", "hi")
	if err != nil || !created1 {
		t.Fatal(err)
	}
	m2, created2, err := a.ClaimOutbound(ctx, "evt:1", "con_1", "evt_1", "sms", "hi")
	if err != nil || created2 || m1.ID != m2.ID {
		t.Fatalf("second claim should return the same row: %v %v", created2, err)
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"(847) 555-0142":   "+18475550142",
		"847.555.0142":     "+18475550142",
		"1-847-555-0142":   "+18475550142",
		"+44 20 7946 0958": "+442079460958",
		"":                 "",
	} {
		if got := store.NormalizePhone(in); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsBytes(h, n []byte) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if string(h[i:i+len(n)]) == string(n) {
			return true
		}
	}
	return false
}
