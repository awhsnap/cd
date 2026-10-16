package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"calendardynamics/internal/auth"
	"calendardynamics/internal/db"
)

// Business management used by the staff admin panel. These functions work
// with business metadata; customer data still requires a Tenant.

type NewBusiness struct {
	Name, Trade, Timezone, Plan, OwnerName, OwnerEmail, OwnerPhone string
}

func (s *Store) CreateBusiness(ctx context.Context, in NewBusiness) (Business, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Business{}, fmt.Errorf("%w: business name is required", ErrInvalid)
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || in.Timezone == "" {
		in.Timezone = "America/Chicago"
	}
	if in.Plan == "" {
		in.Plan = "solo"
	}
	id := auth.NewID("biz")
	wrapped, err := s.Keys.NewBusinessKey(id)
	if err != nil {
		return Business{}, err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO businesses (id, name, trade, timezone, plan, owner_name, owner_email,
		owner_phone, wrapped_key, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, strings.TrimSpace(in.Trade), in.Timezone, in.Plan, strings.TrimSpace(in.OwnerName),
		strings.TrimSpace(in.OwnerEmail), NormalizePhone(in.OwnerPhone), wrapped, db.Now())
	if err != nil {
		return Business{}, err
	}
	return s.GetBusiness(ctx, id)
}

type BusinessSummary struct {
	Business
	Contacts       int
	UpcomingEvents int
	Users          int
	LastActivity   string
}

func (s *Store) ListBusinesses(ctx context.Context, includeDeleted bool) ([]BusinessSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+businessCols+`,
		(SELECT count(*) FROM contacts c WHERE c.business_id = b.id),
		(SELECT count(*) FROM events e WHERE e.business_id = b.id AND e.start_at >= ?),
		(SELECT count(*) FROM users u WHERE u.business_id = b.id),
		coalesce((SELECT max(e.updated_at) FROM events e WHERE e.business_id = b.id), '')
		FROM businesses b WHERE (? OR status <> 'deleted') ORDER BY created_at DESC`, db.Now(), includeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BusinessSummary
	for rows.Next() {
		var bs BusinessSummary
		b := &bs.Business
		if err := rows.Scan(&b.ID, &b.Name, &b.Trade, &b.Timezone, &b.Status, &b.Plan, &b.Deployment,
			&b.QuietStart, &b.QuietEnd, &b.OwnerName, &b.OwnerEmail, &b.OwnerPhone, &b.SMSFrom,
			&b.A2PStatus, &b.OwnerVerifiedAt, &b.PaymentLinkSentAt, &b.CreatedAt, &b.wrappedKey,
			&bs.Contacts, &bs.UpcomingEvents, &bs.Users, &bs.LastActivity); err != nil {
			return nil, err
		}
		out = append(out, bs)
	}
	return out, rows.Err()
}

func (s *Store) SetBusinessStatus(ctx context.Context, id, status string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE businesses SET status = ? WHERE id = ? AND status <> 'deleted'`, status, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

type MessagingSettings struct {
	A2PStatus  string
	SMSFrom    string
	QuietStart int
	QuietEnd   int
	Timezone   string
}

func (s *Store) UpdateMessaging(ctx context.Context, id string, m MessagingSettings) error {
	if m.QuietStart < 0 || m.QuietStart > 23 || m.QuietEnd < 0 || m.QuietEnd > 23 {
		return fmt.Errorf("%w: quiet hours must be 0-23", ErrInvalid)
	}
	if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "" {
		return fmt.Errorf("%w: unknown time zone", ErrInvalid)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE businesses SET a2p_status = ?, sms_from = ?, quiet_start = ?, quiet_end = ?, timezone = ?
		WHERE id = ? AND status <> 'deleted'`, m.A2PStatus, NormalizePhone(m.SMSFrom), m.QuietStart, m.QuietEnd, m.Timezone, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

func (s *Store) MarkPaymentLinkSent(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE businesses SET payment_link_sent_at = ? WHERE id = ?`, db.Now(), id)
	return err
}

func (s *Store) SetVerifyCode(ctx context.Context, id, codeHash string, ttl time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE businesses SET verify_code_hash = ?, verify_expires_at = ? WHERE id = ?`,
		codeHash, db.FormatTime(time.Now().Add(ttl)), id)
	return err
}

// CheckVerifyCode verifies the owner's code once; a used or expired code fails.
func (s *Store) CheckVerifyCode(ctx context.Context, id, code string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE businesses SET owner_verified_at = ?, verify_code_hash = NULL, verify_expires_at = NULL
		WHERE id = ? AND verify_code_hash = ? AND verify_expires_at > ?`, db.Now(), id, auth.HashToken(strings.TrimSpace(code)), db.Now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FindBusinessesForInbound returns candidate businesses for an inbound text:
// the business that owns the receiving number, or — on the shared demo
// number — businesses that have a contact with the sender's phone.
func (s *Store) FindBusinessesForInbound(ctx context.Context, to, from string) ([]string, error) {
	to, from = NormalizePhone(to), NormalizePhone(from)
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM businesses WHERE sms_from = ? AND status <> 'deleted'`, to).Scan(&id)
	if err == nil {
		return []string{id}, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT c.business_id FROM contacts c JOIN businesses b ON b.id = c.business_id
		WHERE c.phone = ? AND b.sms_from = '' AND b.status <> 'deleted'`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteBusiness permanently removes a business's customer data and destroys
// its data key (crypto-shredding), so encrypted fields in backups are
// unreadable too. The business row remains as a tombstone for the audit log.
func (s *Store) DeleteBusiness(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM sessions WHERE business_id = ?`,
		`DELETE FROM invites WHERE business_id = ?`,
		`DELETE FROM users WHERE business_id = ?`,
		`DELETE FROM messages WHERE business_id = ?`,
		`DELETE FROM events WHERE business_id = ?`,
		`DELETE FROM contacts WHERE business_id = ?`,
		`DELETE FROM support_access WHERE business_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE businesses SET status = 'deleted', wrapped_key = NULL, deleted_at = ?,
		owner_email = '', owner_phone = '', owner_name = '', sms_from = '' WHERE id = ?`, db.Now(), id)
	if err != nil {
		return err
	}
	if err := mustAffect(res); err != nil {
		return err
	}
	return tx.Commit()
}
