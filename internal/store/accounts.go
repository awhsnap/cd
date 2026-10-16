package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"calendardynamics/internal/auth"
	"calendardynamics/internal/db"
)

// ---------------------------------------------------------------- customer users

type User struct {
	ID           string
	BusinessID   string
	Email        string
	Name         string
	Role         string
	passwordHash string
}

func (u User) CheckPassword(pw string) bool { return auth.CheckPassword(u.passwordHash, pw) }

func (s *Store) CreateUser(ctx context.Context, businessID, email, name, passwordHash, role string) (User, error) {
	u := User{ID: auth.NewID("usr"), BusinessID: businessID, Email: strings.TrimSpace(email), Name: name, Role: role}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO users (id, business_id, email, name, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, businessID, u.Email, name, passwordHash, role, db.Now())
	return u, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.DB.QueryRowContext(ctx, `SELECT id, business_id, email, name, role, password_hash FROM users WHERE email = ?`,
		strings.TrimSpace(email)).Scan(&u.ID, &u.BusinessID, &u.Email, &u.Name, &u.Role, &u.passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) CountUsers(ctx context.Context, businessID string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE business_id = ?`, businessID).Scan(&n)
	return n, err
}

func (s *Store) CreateSession(ctx context.Context, u User, ttl time.Duration) (raw string, err error) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, business_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`, hash, u.ID, u.BusinessID, db.FormatTime(time.Now().Add(ttl)), db.Now())
	return raw, err
}

// SessionUser resolves a session cookie value to its user, if unexpired.
func (s *Store) SessionUser(ctx context.Context, raw string) (User, error) {
	var u User
	err := s.DB.QueryRowContext(ctx, `SELECT u.id, u.business_id, u.email, u.name, u.role, u.password_hash
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, auth.HashToken(raw), db.Now()).
		Scan(&u.ID, &u.BusinessID, &u.Email, &u.Name, &u.Role, &u.passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, raw string) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, auth.HashToken(raw))
}

// ---------------------------------------------------------------- invites

type Invite struct {
	BusinessID string
	Email      string
	Name       string
}

func (s *Store) CreateInvite(ctx context.Context, businessID, email, name, createdBy string, ttl time.Duration) (raw string, err error) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO invites (token_hash, business_id, email, name, created_by, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, hash, businessID, strings.TrimSpace(email), name, createdBy,
		db.FormatTime(time.Now().Add(ttl)), db.Now())
	return raw, err
}

func (s *Store) GetInvite(ctx context.Context, raw string) (Invite, error) {
	var inv Invite
	err := s.DB.QueryRowContext(ctx, `SELECT business_id, email, name FROM invites
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, auth.HashToken(raw), db.Now()).
		Scan(&inv.BusinessID, &inv.Email, &inv.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return inv, ErrNotFound
	}
	return inv, err
}

// AcceptInvite marks the invite used and creates the user, atomically.
func (s *Store) AcceptInvite(ctx context.Context, raw, name, passwordHash string) (User, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var inv Invite
	err = tx.QueryRowContext(ctx, `UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING business_id, email`, db.Now(), auth.HashToken(raw), db.Now()).Scan(&inv.BusinessID, &inv.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE business_id = ?`, inv.BusinessID).Scan(&count); err != nil {
		return User{}, err
	}
	role := "member"
	if count == 0 {
		role = "owner"
	}
	u := User{ID: auth.NewID("usr"), BusinessID: inv.BusinessID, Email: inv.Email, Name: name, Role: role}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, business_id, email, name, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, u.BusinessID, u.Email, name, passwordHash, role, db.Now()); err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// ---------------------------------------------------------------- staff

type Staff struct {
	ID           string
	Email        string
	Name         string
	Role         string
	passwordHash string
}

func (st Staff) CheckPassword(pw string) bool { return auth.CheckPassword(st.passwordHash, pw) }

var StaffRoles = map[string]bool{"owner": true, "onboarding": true, "support": true, "billing": true}

func (s *Store) CreateStaff(ctx context.Context, email, name, role, passwordHash string) (Staff, error) {
	st := Staff{ID: auth.NewID("stf"), Email: strings.TrimSpace(email), Name: name, Role: role}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO staff_users (id, email, name, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, st.ID, st.Email, name, passwordHash, role, db.Now())
	return st, err
}

func (s *Store) StaffByEmail(ctx context.Context, email string) (Staff, error) {
	var st Staff
	err := s.DB.QueryRowContext(ctx, `SELECT id, email, name, role, password_hash FROM staff_users WHERE email = ?`,
		strings.TrimSpace(email)).Scan(&st.ID, &st.Email, &st.Name, &st.Role, &st.passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

func (s *Store) CreateStaffSession(ctx context.Context, staffID string, ttl time.Duration) (string, error) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO staff_sessions (token_hash, staff_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`, hash, staffID, db.FormatTime(time.Now().Add(ttl)), db.Now())
	return raw, err
}

func (s *Store) StaffBySession(ctx context.Context, raw string) (Staff, error) {
	var st Staff
	err := s.DB.QueryRowContext(ctx, `SELECT u.id, u.email, u.name, u.role, u.password_hash
		FROM staff_sessions s JOIN staff_users u ON u.id = s.staff_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, auth.HashToken(raw), db.Now()).
		Scan(&st.ID, &st.Email, &st.Name, &st.Role, &st.passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

func (s *Store) DeleteStaffSession(ctx context.Context, raw string) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM staff_sessions WHERE token_hash = ?`, auth.HashToken(raw))
}

// ---------------------------------------------------------------- support access

type SupportAccess struct {
	StaffName string `json:"staffName"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expiresAt"`
	CreatedAt string `json:"createdAt"`
}

func (s *Store) OpenSupportAccess(ctx context.Context, businessID string, st Staff, reason string, d time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO support_access (id, business_id, staff_id, staff_name, reason, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, auth.NewID("sup"), businessID, st.ID, st.Name, reason,
		db.FormatTime(time.Now().Add(d)), db.Now())
	return err
}

// ActiveSupportAccess reports whether this staff member has an unexpired session for the business.
func (s *Store) ActiveSupportAccess(ctx context.Context, businessID, staffID string) (SupportAccess, bool) {
	var a SupportAccess
	err := s.DB.QueryRowContext(ctx, `SELECT staff_name, reason, expires_at, created_at FROM support_access
		WHERE business_id = ? AND staff_id = ? AND expires_at > ? ORDER BY created_at DESC LIMIT 1`,
		businessID, staffID, db.Now()).Scan(&a.StaffName, &a.Reason, &a.ExpiresAt, &a.CreatedAt)
	return a, err == nil
}

// RecentSupportAccess lists recent staff access so the business owner can see it.
func (s *Store) RecentSupportAccess(ctx context.Context, businessID string, since time.Time) ([]SupportAccess, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT staff_name, reason, expires_at, created_at FROM support_access
		WHERE business_id = ? AND created_at >= ? ORDER BY created_at DESC LIMIT 10`, businessID, db.FormatTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupportAccess
	for rows.Next() {
		var a SupportAccess
		if err := rows.Scan(&a.StaffName, &a.Reason, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
