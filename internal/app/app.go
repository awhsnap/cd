// Package app wires the store, job queue and messaging together and holds
// the background job handlers.
package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"calendardynamics/internal/config"
	"calendardynamics/internal/jobs"
	"calendardynamics/internal/messaging"
	"calendardynamics/internal/store"
)

type App struct {
	Cfg   config.Config
	Store *store.Store
	Queue *jobs.Queue
	SMS   messaging.SMS
	Email messaging.Email
	Log   *slog.Logger
}

// Queues and their worker counts. Separate lanes mean a big import never
// delays a time-sensitive reminder.
var Queues = map[string]int{
	"messages": 3, // outbound texts and emails; keep low to respect provider rate limits
	"default":  4, // inbound replies, misc
	"bulk":     1, // imports
}

func (a *App) RegisterJobs() {
	a.Queue.Register("send_followup", "messages", 8, a.sendFollowup)
	a.Queue.Register("send_email", "messages", 8, a.sendEmail)
	a.Queue.Register("inbound_sms", "default", 5, a.inboundSMS)
	a.Queue.Register("import_contacts", "bulk", 2, a.importContacts)
}

// ---------------------------------------------------------------- follow-ups

type followupArgs struct {
	BusinessID string `json:"businessId"`
	EventID    string `json:"eventId"`
}

// ScheduleFollowup queues (or re-times) the automated send for a follow-up event.
func (a *App) ScheduleFollowup(ctx context.Context, businessID string, ev store.Event) error {
	if ev.Type != "followup" || ev.Channel == "" || ev.SendStatus != "scheduled" {
		return nil
	}
	return a.Queue.Enqueue(ctx, "send_followup", followupArgs{businessID, ev.ID}, ev.Start, "followup:"+ev.ID)
}

func (a *App) sendFollowup(ctx context.Context, job jobs.Job) error {
	var args followupArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.Cancel{Reason: "bad args"}
	}
	t, err := a.Store.Tenant(ctx, args.BusinessID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Cancel{Reason: "business gone"}
	} else if err != nil {
		return err
	}
	if t.Biz.Status == "suspended" {
		return jobs.Cancel{Reason: "business suspended"}
	}
	ev, err := t.GetEvent(ctx, args.EventID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Cancel{Reason: "event deleted"}
	} else if err != nil {
		return err
	}
	if ev.Type != "followup" || ev.Channel == "" || ev.SendStatus != "scheduled" {
		return jobs.Cancel{Reason: "nothing to send (status " + ev.SendStatus + ")"}
	}
	now := time.Now()
	if ev.Start.After(now.Add(30 * time.Second)) { // event was moved later
		return jobs.Reschedule{At: ev.Start}
	}
	if until := messaging.QuietUntil(now, t.Biz.Location(), t.Biz.QuietStart, t.Biz.QuietEnd); until.After(now) {
		return jobs.Reschedule{At: until}
	}
	contact, err := t.GetContact(ctx, ev.ContactID)
	if err != nil {
		return err
	}

	skip := func(reason string) error {
		_ = t.SetEventSendStatus(ctx, ev.ID, "skipped", reason)
		return jobs.Cancel{Reason: reason}
	}

	var from, to, subject, body string
	switch ev.Channel {
	case "sms":
		switch {
		case contact.Phone == "":
			return skip("No phone number for this contact")
		case contact.OptedOut:
			return skip("Customer opted out of texts")
		case !contact.SMSConsent:
			return skip("No texting consent on file for this contact")
		}
		from = t.Biz.SMSFrom
		if from == "" {
			from = a.Cfg.TwilioFrom
		}
		if from == "" && !isLog(a.SMS) {
			return skip("Business has no texting number yet (A2P registration pending)")
		}
		to = contact.Phone
		body = ev.Message
		if n, _ := t.SentSMSCount(ctx, contact.ID); n == 0 && !strings.Contains(strings.ToUpper(body), "STOP") {
			body += " Reply STOP to opt out."
		}
	case "email":
		if contact.Email == "" {
			return skip("No email address for this contact")
		}
		to = contact.Email
		from = a.Cfg.EmailFrom
		subject = "A message from " + t.Biz.Name
		body = ev.Message + "\n\n— " + t.Biz.Name
	}

	// Idempotency: record the message before sending. If a previous attempt
	// died *during* the provider call we can't know whether it went out, so
	// we mark it 'unknown' rather than risk texting the customer twice.
	msg, created, err := t.ClaimOutbound(ctx, "followup:"+ev.ID, contact.ID, ev.ID, ev.Channel, body)
	if err != nil {
		return err
	}
	if !created {
		switch msg.Status {
		case "sent":
			_ = t.SetEventSendStatus(ctx, ev.ID, "sent", "")
			return nil
		case "sending":
			_ = t.SetMessageStatus(ctx, msg.ID, "unknown", "", "process stopped during send")
			_ = t.SetEventSendStatus(ctx, ev.ID, "failed", "Delivery status unknown — check before resending")
			return jobs.Cancel{Reason: "send interrupted; not retrying to avoid a duplicate"}
		}
	}
	if err := t.SetMessageStatus(ctx, msg.ID, "sending", "", ""); err != nil {
		return err
	}

	var providerID string
	if ev.Channel == "sms" {
		providerID, err = a.SMS.SendSMS(ctx, from, to, body)
	} else {
		providerID, err = a.Email.SendEmail(ctx, from, to, subject, body)
	}
	if err != nil {
		_ = t.SetMessageStatus(ctx, msg.ID, "failed", "", err.Error()) // provider said no: safe to retry
		if messaging.IsPermanent(err) || job.Final() {
			_ = t.SetEventSendStatus(ctx, ev.ID, "failed", err.Error())
			return jobs.Cancel{Reason: err.Error()}
		}
		return err
	}
	_ = t.SetMessageStatus(ctx, msg.ID, "sent", providerID, "")
	return t.SetEventSendStatus(ctx, ev.ID, "sent", "")
}

func isLog(s messaging.SMS) bool {
	_, ok := s.(messaging.Log)
	return ok
}

// ---------------------------------------------------------------- system email

type emailArgs struct {
	To, Subject, Body string
}

// QueueEmail sends a system email (invites, verification codes) with retries.
func (a *App) QueueEmail(ctx context.Context, to, subject, body string) error {
	return a.Queue.Enqueue(ctx, "send_email", emailArgs{to, subject, body}, time.Time{}, "")
}

func (a *App) sendEmail(ctx context.Context, job jobs.Job) error {
	var args emailArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.Cancel{Reason: "bad args"}
	}
	_, err := a.Email.SendEmail(ctx, a.Cfg.EmailFrom, args.To, args.Subject, args.Body)
	if messaging.IsPermanent(err) {
		return jobs.Cancel{Reason: err.Error()}
	}
	return err
}

// ---------------------------------------------------------------- inbound texts

type InboundSMS struct {
	From, To, Body, SID string
}

func (a *App) inboundSMS(ctx context.Context, job jobs.Job) error {
	var in InboundSMS
	if err := json.Unmarshal(job.Args, &in); err != nil {
		return jobs.Cancel{Reason: "bad args"}
	}
	ids, err := a.Store.FindBusinessesForInbound(ctx, in.To, in.From)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		a.Log.Warn("inbound sms: could not match a single business", "to", in.To, "matches", len(ids))
		return jobs.Cancel{Reason: "no unique business for inbound text"}
	}
	t, err := a.Store.Tenant(ctx, ids[0])
	if err != nil {
		return err
	}
	contact, err := t.FindContactByPhone(ctx, in.From)
	if errors.Is(err, store.ErrNotFound) {
		contact = store.Contact{Name: "Unknown " + store.NormalizePhone(in.From), Phone: in.From}
		if err := t.SaveContact(ctx, &contact, ""); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := t.LogInbound(ctx, contact.ID, "sms", in.Body, in.SID); err != nil {
		return err
	}

	note := func(title, notes string) error {
		ev := store.Event{ContactID: contact.ID, Type: "note", Title: title, Start: time.Now(), DurationMins: 15, Notes: notes}
		return t.SaveEvent(ctx, &ev)
	}
	switch messaging.Keyword(in.Body) {
	case "stop":
		if err := t.SetOptOut(ctx, contact.ID, true); err != nil {
			return err
		}
		n, _ := t.PauseFollowups(ctx, contact.ID, "Customer opted out of texts")
		return note("Opted out of texts (replied STOP)", fmt.Sprintf("%d scheduled follow-ups were cancelled.", n))
	case "start":
		if err := t.SetOptOut(ctx, contact.ID, false); err != nil {
			return err
		}
		return note("Opted back in to texts (replied START)", "")
	}
	n, _ := t.PauseFollowups(ctx, contact.ID, "Paused because the customer replied")
	notes := in.Body
	if n > 0 {
		notes += fmt.Sprintf("\n\n(%d scheduled follow-ups paused because the customer replied.)", n)
	}
	return note("Text reply", notes)
}

// ---------------------------------------------------------------- contact import

type ImportArgs struct {
	BusinessID string `json:"businessId"`
	CSV        string `json:"csv"`
	Consent    bool   `json:"consent"`
	ActorType  string `json:"actorType"`
	ActorID    string `json:"actorId"`
	Source     string `json:"source"`
}

func (a *App) importContacts(ctx context.Context, job jobs.Job) error {
	var args ImportArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.Cancel{Reason: "bad args"}
	}
	t, err := a.Store.Tenant(ctx, args.BusinessID)
	if err != nil {
		return jobs.Cancel{Reason: err.Error()}
	}
	rows, err := ParseContactsCSV(strings.NewReader(args.CSV))
	if err != nil {
		a.Store.Audit(ctx, args.ActorType, args.ActorID, args.BusinessID, "contacts.import_failed", err.Error())
		return jobs.Cancel{Reason: err.Error()}
	}
	imported, skipped, err := t.ImportContacts(ctx, rows, args.Consent, args.Source)
	if err != nil {
		return err
	}
	a.Store.Audit(ctx, args.ActorType, args.ActorID, args.BusinessID, "contacts.imported",
		fmt.Sprintf("%d imported, %d skipped (duplicates or missing names); sms consent attested: %v", imported, skipped, args.Consent))
	return nil
}

// ParseContactsCSV reads a spreadsheet export with a header row. It
// recognises common column names (Name / First + Last, Phone / Mobile,
// Email, Address / Street + City).
func ParseContactsCSV(r io.Reader) ([]store.ImportRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, errors.New("the file is empty or not a CSV")
	}
	col := map[string]int{}
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		key = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(key)
		col[key] = i
	}
	find := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}
		return -1
	}
	nameI := find("name", "fullname", "customer", "customername", "contact", "contactname")
	firstI, lastI := find("firstname", "first"), find("lastname", "last", "surname")
	phoneI := find("phone", "phonenumber", "mobile", "cell", "mobilephone", "cellphone", "telephone")
	emailI := find("email", "emailaddress", "e-mail")
	addrI := find("address", "streetaddress", "street", "address1")
	cityI := find("city")
	if nameI < 0 && firstI < 0 {
		return nil, errors.New(`no "Name" (or "First Name") column found in the header row`)
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	var out []store.ImportRow
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", len(out)+2, err)
		}
		name := get(rec, nameI)
		if name == "" {
			name = strings.TrimSpace(get(rec, firstI) + " " + get(rec, lastI))
		}
		addr := get(rec, addrI)
		if c := get(rec, cityI); c != "" {
			addr = strings.Trim(addr+", "+c, ", ")
		}
		out = append(out, store.ImportRow{Name: name, Phone: get(rec, phoneI), Email: get(rec, emailI), Address: addr})
		if len(out) > 20000 {
			return nil, errors.New("too many rows (limit 20,000 per import)")
		}
	}
	return out, nil
}
