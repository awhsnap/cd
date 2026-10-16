// Package store is the only code that touches the database tables.
//
// Tenant isolation rule: customer data is reachable only through a *Tenant,
// which is created for exactly one business. Every Tenant query filters on
// that business's ID, so handlers cannot accidentally read another
// business's rows. Tests in tenant_test.go enforce this.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"calendardynamics/internal/db"
	"calendardynamics/internal/keys"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
)

type Store struct {
	DB   *sql.DB
	Keys *keys.Keyring
}

func New(database *sql.DB, kr *keys.Keyring) *Store {
	return &Store{DB: database, Keys: kr}
}

type Business struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Trade             string `json:"trade"`
	Timezone          string `json:"timezone"`
	Status            string `json:"status"`
	Plan              string `json:"plan"`
	Deployment        string `json:"deployment"`
	QuietStart        int    `json:"quietStart"`
	QuietEnd          int    `json:"quietEnd"`
	OwnerName         string `json:"ownerName"`
	OwnerEmail        string `json:"ownerEmail"`
	OwnerPhone        string `json:"ownerPhone"`
	SMSFrom           string `json:"smsFrom"`
	A2PStatus         string `json:"a2pStatus"`
	OwnerVerifiedAt   string `json:"ownerVerifiedAt"`
	PaymentLinkSentAt string `json:"paymentLinkSentAt"`
	CreatedAt         string `json:"createdAt"`

	wrappedKey []byte
}

// Location returns the business's time zone (falls back to UTC).
func (b Business) Location() *time.Location {
	if loc, err := time.LoadLocation(b.Timezone); err == nil {
		return loc
	}
	return time.UTC
}

const businessCols = `id, name, trade, timezone, status, plan, deployment, quiet_start, quiet_end,
	owner_name, owner_email, owner_phone, sms_from, a2p_status,
	coalesce(owner_verified_at, ''), coalesce(payment_link_sent_at, ''), created_at, wrapped_key`

type scanner interface{ Scan(dest ...any) error }

func scanBusiness(r scanner) (Business, error) {
	var b Business
	err := r.Scan(&b.ID, &b.Name, &b.Trade, &b.Timezone, &b.Status, &b.Plan, &b.Deployment,
		&b.QuietStart, &b.QuietEnd, &b.OwnerName, &b.OwnerEmail, &b.OwnerPhone, &b.SMSFrom,
		&b.A2PStatus, &b.OwnerVerifiedAt, &b.PaymentLinkSentAt, &b.CreatedAt, &b.wrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

func (s *Store) GetBusiness(ctx context.Context, id string) (Business, error) {
	return scanBusiness(s.DB.QueryRowContext(ctx, `SELECT `+businessCols+` FROM businesses WHERE id = ?`, id))
}

// Tenant opens a business for data access. Deleted businesses cannot be opened.
func (s *Store) Tenant(ctx context.Context, businessID string) (*Tenant, error) {
	b, err := s.GetBusiness(ctx, businessID)
	if err != nil {
		return nil, err
	}
	if b.Status == "deleted" {
		return nil, ErrNotFound
	}
	k, err := s.Keys.Business(b.ID, b.wrappedKey)
	if err != nil {
		return nil, err
	}
	return &Tenant{db: s.DB, Biz: b, key: k}, nil
}

// Audit appends to the append-only audit log.
func (s *Store) Audit(ctx context.Context, actorType, actorID, businessID, action, detail string) {
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO audit_log (at, actor_type, actor_id, business_id, action, detail)
		VALUES (?, ?, ?, ?, ?, ?)`, db.Now(), actorType, actorID, businessID, action, detail)
}

type AuditEntry struct {
	At, ActorType, ActorID, BusinessID, Action, Detail string
}

func (s *Store) RecentAudit(ctx context.Context, businessID string, limit int) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, actor_type, actor_id, business_id, action, detail
		FROM audit_log WHERE (? = '' OR business_id = ?) ORDER BY id DESC LIMIT ?`, businessID, businessID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.At, &a.ActorType, &a.ActorID, &a.BusinessID, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NormalizePhone converts common US formats to E.164 ("+18475550142").
// Numbers it can't interpret are returned trimmed, so nothing is silently lost.
func NormalizePhone(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	var digits strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(p, "+") && len(d) >= 8:
		return "+" + d
	case len(d) == 10:
		return "+1" + d
	case len(d) == 11 && d[0] == '1':
		return "+" + d
	}
	return p
}
