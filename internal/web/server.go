// Package web serves the customer-facing app: login, the calendar dashboard,
// its JSON API, and provider webhooks.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"calendardynamics/internal/app"
	"calendardynamics/internal/auth"
	"calendardynamics/internal/messaging"
	"calendardynamics/internal/store"
)

//go:embed assets
var assets embed.FS

const (
	sessionCookie = "cd_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type Server struct {
	app     *app.App
	tmpl    *template.Template
	limiter *limiter
	secure  bool // set the Secure flag on cookies (HTTPS)
}

func New(a *app.App) *Server {
	return &Server{
		app:     a,
		tmpl:    template.Must(template.ParseFS(assets, "assets/templates/*.html")),
		limiter: newLimiter(10, time.Minute),
		secure:  strings.HasPrefix(a.Cfg.BaseURL, "https://"),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app", http.StatusFound) })
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /invite/{token}", s.invitePage)
	mux.HandleFunc("POST /invite/{token}", s.acceptInvite)
	mux.HandleFunc("GET /app", s.requireUserPage(s.dashboard))

	api := func(pattern string, h tenantHandler) { mux.HandleFunc(pattern, s.requireUserAPI(h)) }
	api("GET /api/me", s.me)
	api("GET /api/events", s.listEvents)
	api("GET /api/events/{id}", s.getEvent)
	api("POST /api/events", s.saveEvent)
	api("PUT /api/events/{id}", s.saveEvent)
	api("DELETE /api/events/{id}", s.deleteEvent)
	api("GET /api/contacts", s.listContacts)
	api("GET /api/contacts/{id}", s.getContact)
	api("GET /api/contacts/{id}/events", s.contactEvents)
	api("POST /api/contacts", s.saveContact)
	api("PUT /api/contacts/{id}", s.saveContact)
	api("GET /api/export", s.export)

	mux.HandleFunc("POST /webhooks/twilio/sms", s.twilioInbound)

	return securityHeaders(sameOrigin(mux))
}

// ---------------------------------------------------------------- middleware

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin blocks cross-site form posts (CSRF). API calls must also carry
// an X-Requested-With header, which other sites cannot add without CORS.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			next.ServeHTTP(w, r)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-Requested-With") != "calendar-dynamics" {
			http.Error(w, "missing request header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

type session struct {
	User   store.User
	Tenant *store.Tenant
}

type tenantHandler func(w http.ResponseWriter, r *http.Request, s session) error

func (s *Server) currentSession(r *http.Request) (session, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, store.ErrNotFound
	}
	u, err := s.app.Store.SessionUser(r.Context(), c.Value)
	if err != nil {
		return session{}, err
	}
	t, err := s.app.Store.Tenant(r.Context(), u.BusinessID)
	if err != nil {
		return session{}, err
	}
	return session{User: u, Tenant: t}, nil
}

func (s *Server) requireUserPage(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.currentSession(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if sess.Tenant.Biz.Status == "suspended" {
			s.render(w, http.StatusForbidden, "message.html", map[string]string{
				"Title": "Account paused", "Body": "This account is paused. Please call Calendar Dynamics support.",
			})
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) requireUserAPI(h tenantHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.currentSession(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Please sign in again."})
			return
		}
		if sess.Tenant.Biz.Status == "suspended" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "This account is paused."})
			return
		}
		if err := h(w, r, sess); err != nil {
			s.apiError(w, r, err)
		}
	}
}

func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found."})
	case errors.Is(err, store.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")})
	case errors.Is(err, errBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		s.app.Log.Error("api error", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Something went wrong. Please try again."})
	}
}

var errBadRequest = errors.New("bad request")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.Join(errBadRequest, errors.New("invalid JSON body"))
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.app.Log.Error("template", "name", name, "err", err)
	}
}

// clientIP prefers Fly's header (set by its edge proxy) over the socket address.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------- pages

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.app.Store.DB.PingContext(ctx); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.currentSession(r); err == nil {
		http.Redirect(w, r, "/app", http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "login.html", map[string]any{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("login:" + clientIP(r)) {
		s.render(w, http.StatusTooManyRequests, "login.html", map[string]any{"Error": "Too many attempts. Wait a minute and try again."})
		return
	}
	email, password := strings.TrimSpace(r.FormValue("email")), r.FormValue("password")
	u, err := s.app.Store.UserByEmail(r.Context(), email)
	if err != nil {
		auth.DummyCheck(password)
	}
	if err != nil || !u.CheckPassword(password) {
		s.render(w, http.StatusUnauthorized, "login.html", map[string]any{"Error": "That email and password don't match.", "Email": email})
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.app.Log.Error("login: session", "err", err)
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	s.app.Store.Audit(r.Context(), "user", u.ID, u.BusinessID, "user.login", clientIP(r))
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	raw, err := s.app.Store.CreateSession(r.Context(), u, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.app.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	inv, err := s.app.Store.GetInvite(r.Context(), r.PathValue("token"))
	if err != nil {
		s.render(w, http.StatusNotFound, "message.html", map[string]string{
			"Title": "This invite link has expired", "Body": "Ask Calendar Dynamics support to send you a new one.",
		})
		return
	}
	biz, _ := s.app.Store.GetBusiness(r.Context(), inv.BusinessID)
	s.render(w, http.StatusOK, "invite.html", map[string]any{"Email": inv.Email, "Name": inv.Name, "Business": biz.Name})
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	inv, err := s.app.Store.GetInvite(r.Context(), token)
	if err != nil {
		s.render(w, http.StatusNotFound, "message.html", map[string]string{
			"Title": "This invite link has expired", "Body": "Ask Calendar Dynamics support to send you a new one.",
		})
		return
	}
	biz, _ := s.app.Store.GetBusiness(r.Context(), inv.BusinessID)
	name, pw, pw2 := strings.TrimSpace(r.FormValue("name")), r.FormValue("password"), r.FormValue("password2")
	data := map[string]any{"Email": inv.Email, "Name": name, "Business": biz.Name}
	switch {
	case name == "":
		data["Error"] = "Please enter your name."
	case len(pw) < 10:
		data["Error"] = "Use at least 10 characters for your password."
	case pw != pw2:
		data["Error"] = "The two passwords don't match."
	}
	if data["Error"] != nil {
		s.render(w, http.StatusBadRequest, "invite.html", data)
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	u, err := s.app.Store.AcceptInvite(r.Context(), token, name, hash)
	if err != nil {
		data["Error"] = "This invite was already used, or an account with this email exists."
		s.render(w, http.StatusConflict, "invite.html", data)
		return
	}
	s.app.Store.Audit(r.Context(), "user", u.ID, u.BusinessID, "user.invite_accepted", u.Email)
	if err := s.startSession(w, r, u); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, _ session) {
	s.render(w, http.StatusOK, "app.html", nil)
}

// ---------------------------------------------------------------- webhooks

func (s *Server) twilioInbound(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.app.Cfg.TwilioToken != "" {
		fullURL := s.app.Cfg.BaseURL + r.URL.RequestURI()
		if !messaging.ValidTwilioSignature(s.app.Cfg.TwilioToken, fullURL, r.PostForm, r.Header.Get("X-Twilio-Signature")) {
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
	} else if s.app.Cfg.Prod() {
		http.Error(w, "twilio not configured", http.StatusServiceUnavailable)
		return
	}
	in := app.InboundSMS{From: r.PostFormValue("From"), To: r.PostFormValue("To"), Body: r.PostFormValue("Body"), SID: r.PostFormValue("MessageSid")}
	// Save and respond fast; the job queue does the real work (and retries).
	if err := s.app.Queue.Enqueue(r.Context(), "inbound_sms", in, time.Time{}, "inbound:"+in.SID); err != nil {
		s.app.Log.Error("inbound sms: enqueue", "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable) // Twilio will retry / use the fallback URL
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response></Response>`))
}

// ---------------------------------------------------------------- rate limiting

// limiter is a small in-memory fixed-window limiter. One machine, so memory is fine.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(max int, per time.Duration) *limiter {
	return &limiter{max: max, window: per, hits: map[string]*window{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10000 {
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) > l.window {
		l.hits[key] = &window{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= l.max
}
