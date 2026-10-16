package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"calendardynamics/internal/auth"
	"calendardynamics/internal/db"
	"calendardynamics/internal/keys"
)

// Tenant is a handle on ONE business's data. Every method filters on t.Biz.ID.
type Tenant struct {
	db  *sql.DB
	Biz Business
	key *keys.Business
}

func (t *Tenant) ID() string { return t.Biz.ID }

// ---------------------------------------------------------------- contacts

type Contact struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	Address    string `json:"address"`
	SMSConsent bool   `json:"smsConsent"`
	OptedOut   bool   `json:"optedOut"`
	CreatedAt  string `json:"createdAt"`

	// Filled in by ListContacts for the contacts panel.
	NextStart string `json:"nextStart,omitempty"`
	NextTitle string `json:"nextTitle,omitempty"`
	LastStart string `json:"lastStart,omitempty"`
	LastTitle string `json:"lastTitle,omitempty"`
}

const contactCols = `c.id, c.name, c.phone, c.email, c.address,
	c.sms_consent_at IS NOT NULL, c.opted_out_at IS NOT NULL, c.created_at`

func scanContact(r scanner, extra ...any) (Contact, error) {
	var c Contact
	dest := append([]any{&c.ID, &c.Name, &c.Phone, &c.Email, &c.Address, &c.SMSConsent, &c.OptedOut, &c.CreatedAt}, extra...)
	err := r.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (t *Tenant) ListContacts(ctx context.Context, q string, limit int) ([]Contact, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	q = strings.TrimSpace(q)
	like := "%" + q + "%"
	now := db.Now()
	rows, err := t.db.QueryContext(ctx, `SELECT `+contactCols+`,
		coalesce((SELECT e.start_at || '|' || e.title FROM events e
		          WHERE e.business_id = c.business_id AND e.contact_id = c.id AND e.start_at >= ?2
		          ORDER BY e.start_at LIMIT 1), ''),
		coalesce((SELECT e.start_at || '|' || e.title FROM events e
		          WHERE e.business_id = c.business_id AND e.contact_id = c.id AND e.start_at < ?2
		          ORDER BY e.start_at DESC LIMIT 1), '')
		FROM contacts c
		WHERE c.business_id = ?1
		  AND (?3 = '' OR c.name LIKE ?4 OR c.phone LIKE ?4 OR c.email LIKE ?4 OR c.address LIKE ?4)
		ORDER BY c.name COLLATE NOCASE LIMIT ?5`, t.Biz.ID, now, q, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var next, last string
		c, err := scanContact(rows, &next, &last)
		if err != nil {
			return nil, err
		}
		c.NextStart, c.NextTitle = splitSummary(next)
		c.LastStart, c.LastTitle = splitSummary(last)
		out = append(out, c)
	}
	return out, rows.Err()
}

func splitSummary(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	at, title, _ := strings.Cut(s, "|")
	if ts, err := db.ParseTime(at); err == nil {
		at = ts.Format(time.RFC3339)
	}
	return at, title
}

func (t *Tenant) GetContact(ctx context.Context, id string) (Contact, error) {
	return scanContact(t.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts c
		WHERE c.business_id = ? AND c.id = ?`, t.Biz.ID, id))
}

func (t *Tenant) FindContactByName(ctx context.Context, name string) (Contact, error) {
	return scanContact(t.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts c
		WHERE c.business_id = ? AND c.name = ? COLLATE NOCASE ORDER BY c.created_at LIMIT 1`, t.Biz.ID, strings.TrimSpace(name)))
}

func (t *Tenant) FindContactByPhone(ctx context.Context, phone string) (Contact, error) {
	phone = NormalizePhone(phone)
	if phone == "" {
		return Contact{}, ErrNotFound
	}
	return scanContact(t.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts c
		WHERE c.business_id = ? AND c.phone = ? ORDER BY c.created_at LIMIT 1`, t.Biz.ID, phone))
}

// SaveContact creates (empty ID) or updates a contact. consentSource is
// recorded when SMS consent is newly granted.
func (t *Tenant) SaveContact(ctx context.Context, c *Contact, consentSource string) error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 200 {
		return fmt.Errorf("%w: contact name is required", ErrInvalid)
	}
	c.Phone = NormalizePhone(c.Phone)
	c.Email = strings.TrimSpace(c.Email)
	c.Address = strings.TrimSpace(c.Address)
	now := db.Now()

	if c.ID == "" {
		c.ID = auth.NewID("con")
		var consentAt any
		if c.SMSConsent {
			consentAt = now
		}
		_, err := t.db.ExecContext(ctx, `INSERT INTO contacts
			(id, business_id, name, phone, email, address, sms_consent_at, sms_consent_source, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, t.Biz.ID, c.Name, c.Phone, c.Email, c.Address, consentAt, consentSource, now)
		c.CreatedAt = now
		return err
	}

	res, err := t.db.ExecContext(ctx, `UPDATE contacts SET name = ?, phone = ?, email = ?, address = ?,
		sms_consent_at = CASE WHEN ? THEN coalesce(sms_consent_at, ?) ELSE NULL END,
		sms_consent_source = CASE WHEN ? AND sms_consent_at IS NULL THEN ? ELSE sms_consent_source END
		WHERE business_id = ? AND id = ?`,
		c.Name, c.Phone, c.Email, c.Address, c.SMSConsent, now, c.SMSConsent, consentSource, t.Biz.ID, c.ID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

func (t *Tenant) SetOptOut(ctx context.Context, contactID string, optedOut bool) error {
	var at any
	if optedOut {
		at = db.Now()
	}
	_, err := t.db.ExecContext(ctx, `UPDATE contacts SET opted_out_at = ? WHERE business_id = ? AND id = ?`, at, t.Biz.ID, contactID)
	return err
}

// ---------------------------------------------------------------- events

var EventTypes = map[string]bool{"job": true, "estimate": true, "followup": true, "call": true, "note": true}

type Event struct {
	ID           string    `json:"id"`
	ContactID    string    `json:"contactId"`
	ContactName  string    `json:"contactName"`
	Type         string    `json:"type"`
	Title        string    `json:"title"`
	Start        time.Time `json:"start"`
	DurationMins int       `json:"durationMins"`
	Notes        string    `json:"notes"`
	Channel      string    `json:"channel"`    // follow-ups: "sms", "email" or "" (manual)
	Message      string    `json:"message"`    // follow-ups: what gets sent
	SendStatus   string    `json:"sendStatus"` // "", scheduled, sent, skipped, failed
	SendNote     string    `json:"sendNote"`
}

const eventSelect = `SELECT e.id, e.contact_id, c.name, e.type, e.title, e.start_at, e.duration_mins,
	e.notes_enc, e.channel, e.message_enc, e.send_status, e.send_note
	FROM events e JOIN contacts c ON c.id = e.contact_id AND c.business_id = e.business_id
	WHERE e.business_id = ?`

func (t *Tenant) scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := t.scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (t *Tenant) scanEvent(r scanner) (Event, error) {
	var e Event
	var start string
	var notes, msg []byte
	err := r.Scan(&e.ID, &e.ContactID, &e.ContactName, &e.Type, &e.Title, &start, &e.DurationMins,
		&notes, &e.Channel, &msg, &e.SendStatus, &e.SendNote)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if e.Start, err = db.ParseTime(start); err != nil {
		return e, err
	}
	if e.Notes, err = t.key.Open(notes); err != nil {
		return e, err
	}
	e.Message, err = t.key.Open(msg)
	return e, err
}

func (t *Tenant) ListEvents(ctx context.Context, from, to time.Time) ([]Event, error) {
	rows, err := t.db.QueryContext(ctx, eventSelect+` AND e.start_at >= ? AND e.start_at < ? ORDER BY e.start_at`,
		t.Biz.ID, db.FormatTime(from), db.FormatTime(to))
	if err != nil {
		return nil, err
	}
	return t.scanEvents(rows)
}

func (t *Tenant) ListContactEvents(ctx context.Context, contactID string) ([]Event, error) {
	rows, err := t.db.QueryContext(ctx, eventSelect+` AND e.contact_id = ? ORDER BY e.start_at`, t.Biz.ID, contactID)
	if err != nil {
		return nil, err
	}
	return t.scanEvents(rows)
}

func (t *Tenant) GetEvent(ctx context.Context, id string) (Event, error) {
	return t.scanEvent(t.db.QueryRowContext(ctx, eventSelect+` AND e.id = ?`, t.Biz.ID, id))
}

// SaveEvent validates and creates (empty ID) or updates an event.
func (t *Tenant) SaveEvent(ctx context.Context, e *Event) error {
	e.Title = strings.TrimSpace(e.Title)
	switch {
	case !EventTypes[e.Type]:
		return fmt.Errorf("%w: unknown event type", ErrInvalid)
	case e.Title == "" || len(e.Title) > 300:
		return fmt.Errorf("%w: title is required", ErrInvalid)
	case e.Start.IsZero():
		return fmt.Errorf("%w: start time is required", ErrInvalid)
	case len(e.Notes) > 20000 || len(e.Message) > 1600:
		return fmt.Errorf("%w: text is too long", ErrInvalid)
	}
	if e.DurationMins < 5 || e.DurationMins > 1440 {
		e.DurationMins = 60
	}
	if e.Type != "followup" {
		e.Channel, e.Message = "", ""
	}
	if e.Channel != "" && e.Channel != "sms" && e.Channel != "email" {
		return fmt.Errorf("%w: channel must be sms or email", ErrInvalid)
	}
	if e.Channel != "" && strings.TrimSpace(e.Message) == "" {
		return fmt.Errorf("%w: a follow-up needs a message to send", ErrInvalid)
	}
	// The contact must belong to this business.
	contact, err := t.GetContact(ctx, e.ContactID)
	if err != nil {
		return fmt.Errorf("%w: unknown contact", ErrInvalid)
	}
	e.ContactName = contact.Name

	notes, err := t.key.Seal(e.Notes)
	if err != nil {
		return err
	}
	msg, err := t.key.Seal(e.Message)
	if err != nil {
		return err
	}
	now := db.Now()

	if e.ID == "" {
		e.ID = auth.NewID("evt")
		e.SendStatus = ""
		if e.Channel != "" {
			e.SendStatus = "scheduled"
		}
		_, err = t.db.ExecContext(ctx, `INSERT INTO events (id, business_id, contact_id, type, title, start_at,
			duration_mins, notes_enc, channel, message_enc, send_status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, t.Biz.ID, e.ContactID, e.Type, e.Title, db.FormatTime(e.Start), e.DurationMins,
			notes, e.Channel, msg, e.SendStatus, now, now)
		return err
	}

	existing, err := t.GetEvent(ctx, e.ID)
	if err != nil {
		return err
	}
	switch {
	case existing.SendStatus == "sent":
		e.SendStatus = "sent" // already delivered; editing doesn't resend
	case e.Channel != "":
		e.SendStatus = "scheduled"
	default:
		e.SendStatus = ""
	}
	res, err := t.db.ExecContext(ctx, `UPDATE events SET contact_id = ?, type = ?, title = ?, start_at = ?,
		duration_mins = ?, notes_enc = ?, channel = ?, message_enc = ?, send_status = ?, send_note = '', updated_at = ?
		WHERE business_id = ? AND id = ?`,
		e.ContactID, e.Type, e.Title, db.FormatTime(e.Start), e.DurationMins, notes, e.Channel, msg,
		e.SendStatus, now, t.Biz.ID, e.ID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

func (t *Tenant) DeleteEvent(ctx context.Context, id string) error {
	res, err := t.db.ExecContext(ctx, `DELETE FROM events WHERE business_id = ? AND id = ?`, t.Biz.ID, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

func (t *Tenant) SetEventSendStatus(ctx context.Context, id, status, note string) error {
	_, err := t.db.ExecContext(ctx, `UPDATE events SET send_status = ?, send_note = ?, updated_at = ?
		WHERE business_id = ? AND id = ?`, status, note, db.Now(), t.Biz.ID, id)
	return err
}

// PauseFollowups skips a contact's scheduled automated follow-ups (used when they reply).
func (t *Tenant) PauseFollowups(ctx context.Context, contactID, note string) (int64, error) {
	res, err := t.db.ExecContext(ctx, `UPDATE events SET send_status = 'skipped', send_note = ?, updated_at = ?
		WHERE business_id = ? AND contact_id = ? AND type = 'followup' AND send_status = 'scheduled'`,
		note, db.Now(), t.Biz.ID, contactID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- messages

type Message struct {
	ID         string
	Status     string
	ProviderID string
}

// ClaimOutbound records an outbound message under an idempotency key before
// it is sent. If the key already exists, the existing row is returned and
// created is false — the caller then decides whether sending is safe.
func (t *Tenant) ClaimOutbound(ctx context.Context, key, contactID, eventID, channel, body string) (m Message, created bool, err error) {
	enc, err := t.key.Seal(body)
	if err != nil {
		return m, false, err
	}
	id := auth.NewID("msg")
	res, err := t.db.ExecContext(ctx, `INSERT INTO messages
		(id, business_id, contact_id, event_id, channel, direction, body_enc, status, idempotency_key, created_at)
		VALUES (?, ?, ?, ?, ?, 'out', ?, 'pending', ?, ?) ON CONFLICT (idempotency_key) DO NOTHING`,
		id, t.Biz.ID, contactID, eventID, channel, enc, key, db.Now())
	if err != nil {
		return m, false, err
	}
	n, _ := res.RowsAffected()
	err = t.db.QueryRowContext(ctx, `SELECT id, status, provider_id FROM messages WHERE business_id = ? AND idempotency_key = ?`,
		t.Biz.ID, key).Scan(&m.ID, &m.Status, &m.ProviderID)
	return m, n == 1, err
}

func (t *Tenant) SetMessageStatus(ctx context.Context, id, status, providerID, errText string) error {
	_, err := t.db.ExecContext(ctx, `UPDATE messages SET status = ?, provider_id = ?, error = ?
		WHERE business_id = ? AND id = ?`, status, providerID, errText, t.Biz.ID, id)
	return err
}

func (t *Tenant) LogInbound(ctx context.Context, contactID, channel, body, providerID string) error {
	enc, err := t.key.Seal(body)
	if err != nil {
		return err
	}
	_, err = t.db.ExecContext(ctx, `INSERT INTO messages
		(id, business_id, contact_id, channel, direction, body_enc, provider_id, status, created_at)
		VALUES (?, ?, ?, ?, 'in', ?, ?, 'received', ?)`,
		auth.NewID("msg"), t.Biz.ID, contactID, channel, enc, providerID, db.Now())
	return err
}

// SentSMSCount is used to add opt-out language to the first text a contact receives.
func (t *Tenant) SentSMSCount(ctx context.Context, contactID string) (int, error) {
	var n int
	err := t.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE business_id = ? AND contact_id = ?
		AND channel = 'sms' AND direction = 'out' AND status = 'sent'`, t.Biz.ID, contactID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- import / export

type ImportRow struct {
	Name, Phone, Email, Address string
}

// ImportContacts adds contacts in one transaction, skipping rows that match
// an existing contact by phone (or by name when there's no phone).
func (t *Tenant) ImportContacts(ctx context.Context, rows []ImportRow, consent bool, consentSource string) (imported, skipped int, err error) {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	var consentAt any
	if consent {
		consentAt = now
	}
	for _, r := range rows {
		r.Name = strings.TrimSpace(r.Name)
		r.Phone = NormalizePhone(r.Phone)
		if r.Name == "" {
			skipped++
			continue
		}
		var exists int
		if r.Phone != "" {
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE business_id = ? AND phone = ?`, t.Biz.ID, r.Phone).Scan(&exists)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE business_id = ? AND name = ? COLLATE NOCASE`, t.Biz.ID, r.Name).Scan(&exists)
		}
		if err != nil {
			return 0, 0, err
		}
		if exists > 0 {
			skipped++
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO contacts
			(id, business_id, name, phone, email, address, sms_consent_at, sms_consent_source, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			auth.NewID("con"), t.Biz.ID, r.Name, r.Phone, strings.TrimSpace(r.Email), strings.TrimSpace(r.Address),
			consentAt, consentSource, now); err != nil {
			return 0, 0, err
		}
		imported++
	}
	return imported, skipped, tx.Commit()
}

type Export struct {
	ExportedAt string         `json:"exportedAt"`
	Business   map[string]any `json:"business"`
	Contacts   []Contact      `json:"contacts"`
	Events     []Event        `json:"events"`
	Messages   []ExportMsg    `json:"messages"`
}

type ExportMsg struct {
	ContactID string `json:"contactId"`
	Channel   string `json:"channel"`
	Direction string `json:"direction"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

// Export returns everything the business owns, decrypted — their data, portable.
func (t *Tenant) Export(ctx context.Context) (*Export, error) {
	x := &Export{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Business:   map[string]any{"id": t.Biz.ID, "name": t.Biz.Name, "timezone": t.Biz.Timezone},
	}
	var err error
	if x.Contacts, err = t.ListContacts(ctx, "", 1000000); err != nil {
		return nil, err
	}
	rows, err := t.db.QueryContext(ctx, eventSelect+` ORDER BY e.start_at`, t.Biz.ID)
	if err != nil {
		return nil, err
	}
	if x.Events, err = t.scanEvents(rows); err != nil {
		return nil, err
	}
	mrows, err := t.db.QueryContext(ctx, `SELECT contact_id, channel, direction, body_enc, status, created_at
		FROM messages WHERE business_id = ? ORDER BY created_at`, t.Biz.ID)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m ExportMsg
		var body []byte
		if err := mrows.Scan(&m.ContactID, &m.Channel, &m.Direction, &body, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		if m.Body, err = t.key.Open(body); err != nil {
			return nil, err
		}
		x.Messages = append(x.Messages, m)
	}
	return x, mrows.Err()
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
