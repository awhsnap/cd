// Package admin is the staff-only panel for onboarding and managing
// businesses. It listens on a private address (never the public internet),
// requires a staff login, records every action in the audit log, and only
// shows a business's customer data during a time-limited support session.
package admin

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"calendardynamics/internal/app"
	"calendardynamics/internal/auth"
	"calendardynamics/internal/store"
)

//go:embed templates/*.html
var templates embed.FS

const (
	staffCookie   = "cd_staff"
	staffTTL      = 12 * time.Hour
	supportWindow = 30 * time.Minute
)

type Server struct {
	app  *app.App
	tmpl *template.Template
}

func New(a *app.App) *Server {
	funcs := template.FuncMap{
		"list": func(v ...string) []string { return v },
		"when": func(s string) string {
			if s == "" {
				return "—"
			}
			for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339} {
				if t, err := time.Parse(layout, s); err == nil {
					return t.Local().Format("Jan 2, 2006 3:04 PM")
				}
			}
			return s
		},
		"timeIn": func(t time.Time, tz string) string {
			loc, err := time.LoadLocation(tz)
			if err != nil {
				loc = time.UTC
			}
			return t.In(loc).Format("Mon Jan 2, 3:04 PM")
		},
	}
	return &Server{app: a, tmpl: template.Must(template.New("").Funcs(funcs).ParseFS(templates, "templates/*.html"))}
}

// Role permissions. "owner" can do everything.
var permissions = map[string]map[string]bool{
	"onboarding": {"create": true, "onboard": true, "import": true, "invite": true, "status": true},
	"support":    {"support": true, "invite": true},
	"billing":    {"payment": true, "status": true},
}

func can(st store.Staff, action string) bool {
	return st.Role == "owner" || permissions[st.Role][action]
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	h := func(pattern string, fn staffHandler) { mux.HandleFunc(pattern, s.requireStaff(fn)) }
	h("GET /{$}", s.index)
	h("GET /businesses/new", s.newBusinessPage)
	h("POST /businesses", s.createBusiness)
	h("GET /businesses/{id}", s.businessPage)
	h("POST /businesses/{id}/verify/send", s.sendVerifyCode)
	h("POST /businesses/{id}/verify/check", s.checkVerifyCode)
	h("POST /businesses/{id}/payment", s.markPayment)
	h("POST /businesses/{id}/messaging", s.updateMessaging)
	h("POST /businesses/{id}/import", s.importContacts)
	h("POST /businesses/{id}/invite", s.invite)
	h("POST /businesses/{id}/status", s.setStatus)
	h("POST /businesses/{id}/support", s.openSupport)
	h("GET /businesses/{id}/export", s.export)
	h("POST /businesses/{id}/delete", s.deleteBusiness)
	h("GET /audit", s.audit)

	return headers(sameOrigin(mux))
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// "same-origin" (not "no-referrer"): with no-referrer, browsers send
		// `Origin: null` on form posts, which the CSRF check would reject.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request blocked", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

type staffHandler func(w http.ResponseWriter, r *http.Request, st store.Staff)

func (s *Server) requireStaff(fn staffHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(staffCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		st, err := s.app.Store.StaffBySession(r.Context(), c.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fn(w, r, st)
	}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.app.Log.Error("admin template", "name", name, "err", err)
	}
}

func back(w http.ResponseWriter, r *http.Request, id, msg string) {
	http.Redirect(w, r, "/businesses/"+id+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) audit2(r *http.Request, st store.Staff, businessID, action, detail string) {
	s.app.Store.Audit(r.Context(), "staff", st.ID, businessID, action, st.Email+": "+detail)
}

func forbidden(w http.ResponseWriter) {
	http.Error(w, "Your staff role doesn't allow this action.", http.StatusForbidden)
}

// ---------------------------------------------------------------- auth

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "login.html", map[string]any{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	email, pw := strings.TrimSpace(r.FormValue("email")), r.FormValue("password")
	st, err := s.app.Store.StaffByEmail(r.Context(), email)
	if err != nil {
		auth.DummyCheck(pw)
	}
	if err != nil || !st.CheckPassword(pw) {
		s.app.Store.Audit(r.Context(), "staff", "", "", "staff.login_failed", email)
		s.render(w, http.StatusUnauthorized, "login.html", map[string]any{"Error": "Wrong email or password.", "Email": email})
		return
	}
	raw, err := s.app.Store.CreateStaffSession(r.Context(), st.ID, staffTTL)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: staffCookie, Value: raw, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(staffTTL.Seconds())})
	s.audit2(r, st, "", "staff.login", "signed in")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(staffCookie); err == nil {
		s.app.Store.DeleteStaffSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: staffCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------- pages

func (s *Server) index(w http.ResponseWriter, r *http.Request, st store.Staff) {
	list, err := s.app.Store.ListBusinesses(r.Context(), r.URL.Query().Get("deleted") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats, _ := s.app.Queue.Stats(r.Context())
	counts := map[string]int{}
	for _, b := range list {
		counts[b.Status]++
	}
	s.render(w, http.StatusOK, "index.html", map[string]any{"Staff": st, "Businesses": list, "Counts": counts, "Jobs": stats, "Msg": r.URL.Query().Get("msg")})
}

func (s *Server) newBusinessPage(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "create") {
		forbidden(w)
		return
	}
	s.render(w, http.StatusOK, "new.html", map[string]any{"Staff": st, "Form": store.NewBusiness{Timezone: "America/Chicago", Plan: "solo"}})
}

func (s *Server) createBusiness(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "create") {
		forbidden(w)
		return
	}
	in := store.NewBusiness{
		Name: r.FormValue("name"), Trade: r.FormValue("trade"), Timezone: r.FormValue("timezone"), Plan: r.FormValue("plan"),
		OwnerName: r.FormValue("owner_name"), OwnerEmail: r.FormValue("owner_email"), OwnerPhone: r.FormValue("owner_phone"),
	}
	if strings.TrimSpace(in.OwnerEmail) == "" || !strings.Contains(in.OwnerEmail, "@") {
		s.render(w, http.StatusBadRequest, "new.html", map[string]any{"Staff": st, "Form": in, "Error": "The owner's email is required — it's where their invite and verification code go."})
		return
	}
	b, err := s.app.Store.CreateBusiness(r.Context(), in)
	if err != nil {
		s.render(w, http.StatusBadRequest, "new.html", map[string]any{"Staff": st, "Form": in, "Error": err.Error()})
		return
	}
	s.audit2(r, st, b.ID, "business.created", b.Name)
	back(w, r, b.ID, "Business created. Next: verify the owner while you're on the phone.")
}

type checklistItem struct {
	Label, Detail string
	Done          bool
}

func (s *Server) businessPage(w http.ResponseWriter, r *http.Request, st store.Staff) {
	s.renderBusiness(w, r, st, nil)
}

func (s *Server) renderBusiness(w http.ResponseWriter, r *http.Request, st store.Staff, extra map[string]any) {
	ctx := r.Context()
	id := r.PathValue("id")
	b, err := s.app.Store.GetBusiness(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	users, _ := s.app.Store.CountUsers(ctx, id)
	var contacts int
	_ = s.app.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE business_id = ?`, id).Scan(&contacts)

	a2p := map[string]string{"not_started": "Not started", "submitted": "Submitted — waiting on carriers", "approved": "Approved", "rejected": "Rejected — fix and resubmit"}[b.A2PStatus]
	checklist := []checklistItem{
		{"Business created", b.Name, true},
		{"Owner identity verified", "Code sent to the owner's email and read back on the call", b.OwnerVerifiedAt != ""},
		{"Payment link sent", "Never take card numbers over the phone", b.PaymentLinkSentAt != ""},
		{"Texting registration (A2P 10DLC)", a2p, b.A2PStatus == "approved"},
		{"Contacts imported", fmt.Sprintf("%d contacts", contacts), contacts > 0},
		{"Owner account created", fmt.Sprintf("%d users", users), users > 0},
		{"Live", "Status is Active", b.Status == "active"},
	}
	done := 0
	for _, c := range checklist {
		if c.Done {
			done++
		}
	}

	data := map[string]any{
		"Staff": st, "B": b, "Checklist": checklist, "Done": done, "Total": len(checklist), "Pct": done * 100 / len(checklist),
		"Msg": r.URL.Query().Get("msg"), "Contacts": contacts, "Users": users, "Can": map[string]bool{
			"onboard": can(st, "onboard"), "import": can(st, "import"), "invite": can(st, "invite"),
			"status": can(st, "status"), "support": can(st, "support"), "payment": can(st, "payment"), "delete": st.Role == "owner",
		},
	}
	if access, ok := s.app.Store.ActiveSupportAccess(ctx, id, st.ID); ok && b.Status != "deleted" {
		data["Access"] = access
		if t, err := s.app.Store.Tenant(ctx, id); err == nil {
			cs, _ := t.ListContacts(ctx, "", 50)
			evs, _ := t.ListEvents(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(14*24*time.Hour))
			data["TenantContacts"], data["TenantEvents"] = cs, evs
		}
	}
	data["Audit"], _ = s.app.Store.RecentAudit(ctx, id, 25)
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, http.StatusOK, "business.html", data)
}

// ---------------------------------------------------------------- onboarding actions

func (s *Server) sendVerifyCode(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "onboard") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	b, err := s.app.Store.GetBusiness(r.Context(), id)
	if err != nil || b.OwnerEmail == "" {
		back(w, r, id, "This business has no owner email on file.")
		return
	}
	code, hash, err := auth.NewCode()
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if err := s.app.Store.SetVerifyCode(r.Context(), id, hash, 15*time.Minute); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	body := fmt.Sprintf("Your Calendar Dynamics verification code is %s.\n\nRead it to the Calendar Dynamics team member you're speaking with. It expires in 15 minutes. If you're not on a call with us, ignore this email.", code)
	if err := s.app.QueueEmail(r.Context(), b.OwnerEmail, "Your verification code: "+code, body); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "owner.verify_code_sent", b.OwnerEmail)
	back(w, r, id, "Code sent to "+b.OwnerEmail+". Ask the owner to read it back to you.")
}

func (s *Server) checkVerifyCode(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "onboard") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	ok, err := s.app.Store.CheckVerifyCode(r.Context(), id, r.FormValue("code"))
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.audit2(r, st, id, "owner.verify_failed", "wrong or expired code")
		back(w, r, id, "That code didn't match or has expired. Send a new one.")
		return
	}
	s.audit2(r, st, id, "owner.verified", "code matched")
	back(w, r, id, "Owner verified.")
}

func (s *Server) markPayment(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "payment") && !can(st, "onboard") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	link := strings.TrimSpace(r.FormValue("link"))
	b, err := s.app.Store.GetBusiness(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if link != "" {
		if u, err := url.Parse(link); err != nil || u.Scheme != "https" {
			back(w, r, id, "The payment link must be an https:// link (e.g. a Stripe Checkout or Payment Link).")
			return
		}
		body := fmt.Sprintf("Hi %s,\n\nHere's the secure link to start your Calendar Dynamics subscription:\n\n%s\n\nThanks!", b.OwnerName, link)
		if err := s.app.QueueEmail(r.Context(), b.OwnerEmail, "Start your Calendar Dynamics subscription", body); err != nil {
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
	}
	_ = s.app.Store.MarkPaymentLinkSent(r.Context(), id)
	s.audit2(r, st, id, "payment.link_sent", link)
	back(w, r, id, "Payment link recorded.")
}

func (s *Server) updateMessaging(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "onboard") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	qs, _ := strconv.Atoi(r.FormValue("quiet_start"))
	qe, _ := strconv.Atoi(r.FormValue("quiet_end"))
	m := store.MessagingSettings{A2PStatus: r.FormValue("a2p_status"), SMSFrom: r.FormValue("sms_from"), QuietStart: qs, QuietEnd: qe, Timezone: r.FormValue("timezone")}
	if err := s.app.Store.UpdateMessaging(r.Context(), id, m); err != nil {
		back(w, r, id, "Couldn't save: "+err.Error())
		return
	}
	s.audit2(r, st, id, "messaging.updated", fmt.Sprintf("a2p=%s from=%s quiet=%d-%d tz=%s", m.A2PStatus, m.SMSFrom, qs, qe, m.Timezone))
	back(w, r, id, "Messaging settings saved.")
}

func (s *Server) importContacts(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "import") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	file, _, err := r.FormFile("csv")
	if err != nil {
		back(w, r, id, "Choose a CSV file to import (max 10 MB).")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		back(w, r, id, "Couldn't read the file.")
		return
	}
	// Validate now so staff see errors immediately; the import itself runs on the bulk queue.
	rows, err := app.ParseContactsCSV(strings.NewReader(string(raw)))
	if err != nil {
		back(w, r, id, "Import problem: "+err.Error())
		return
	}
	consent := r.FormValue("consent") == "yes"
	args := app.ImportArgs{BusinessID: id, CSV: string(raw), Consent: consent, ActorType: "staff", ActorID: st.ID,
		Source: "Owner attested consent during onboarding call with " + st.Email}
	if err := s.app.Queue.Enqueue(r.Context(), "import_contacts", args, time.Time{}, ""); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "contacts.import_queued", fmt.Sprintf("%d rows, consent attested: %v", len(rows), consent))
	back(w, r, id, fmt.Sprintf("Importing %d rows in the background — refresh in a moment.", len(rows)))
}

func (s *Server) invite(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "invite") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	b, err := s.app.Store.GetBusiness(r.Context(), id)
	if err != nil || b.Status == "deleted" {
		http.NotFound(w, r)
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" {
		email = b.OwnerEmail
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" && email == b.OwnerEmail {
		name = b.OwnerName
	}
	raw, err := s.app.Store.CreateInvite(r.Context(), id, email, name, st.ID, 72*time.Hour)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	link := s.app.Cfg.BaseURL + "/invite/" + raw
	body := fmt.Sprintf("Hi %s,\n\nYour Calendar Dynamics account for %s is ready. Set your password here (the link works for 3 days):\n\n%s\n\nSee you on the calendar!", name, b.Name, link)
	if err := s.app.QueueEmail(r.Context(), email, "Set up your Calendar Dynamics account", body); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "user.invited", email)
	// Show the link once so it can also be read out or texted during the call.
	s.renderBusiness(w, r, st, map[string]any{"InviteLink": link, "Msg": "Invite emailed to " + email + "."})
}

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "status") {
		forbidden(w)
		return
	}
	id, status := r.PathValue("id"), r.FormValue("status")
	if status != "active" && status != "suspended" && status != "onboarding" {
		back(w, r, id, "Unknown status.")
		return
	}
	if err := s.app.Store.SetBusinessStatus(r.Context(), id, status); err != nil {
		back(w, r, id, "Couldn't change status: "+err.Error())
		return
	}
	s.audit2(r, st, id, "business.status", status)
	back(w, r, id, "Status set to "+status+".")
}

func (s *Server) openSupport(w http.ResponseWriter, r *http.Request, st store.Staff) {
	if !can(st, "support") {
		forbidden(w)
		return
	}
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) < 10 {
		back(w, r, id, "Give a specific reason (who asked, ticket number) — it's shown to the business owner.")
		return
	}
	if err := s.app.Store.OpenSupportAccess(r.Context(), id, st, reason, supportWindow); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "support.access_opened", reason)
	back(w, r, id, "Support access open for 30 minutes. The owner will see this in their dashboard.")
}

func (s *Server) export(w http.ResponseWriter, r *http.Request, st store.Staff) {
	id := r.PathValue("id")
	if _, ok := s.app.Store.ActiveSupportAccess(r.Context(), id, st.ID); !ok {
		back(w, r, id, "Open a support session first — exports include customer data.")
		return
	}
	t, err := s.app.Store.Tenant(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	x, err := t.Export(r.Context())
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "data.exported", "by staff")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="export-%s.json"`, id))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(x)
}

func (s *Server) deleteBusiness(w http.ResponseWriter, r *http.Request, st store.Staff) {
	id := r.PathValue("id")
	if st.Role != "owner" {
		forbidden(w)
		return
	}
	b, err := s.app.Store.GetBusiness(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != b.Name {
		back(w, r, id, "Type the business name exactly to confirm deletion.")
		return
	}
	if err := s.app.Store.DeleteBusiness(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit2(r, st, id, "business.deleted", b.Name+" (data erased, key destroyed)")
	http.Redirect(w, r, "/?msg="+url.QueryEscape(b.Name+" was permanently deleted."), http.StatusSeeOther)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, st store.Staff) {
	entries, err := s.app.Store.RecentAudit(r.Context(), "", 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "audit.html", map[string]any{"Staff": st, "Entries": entries})
}

// CreateStaff is used by the `server create-staff` command.
func CreateStaff(ctx context.Context, st *store.Store, email, name, role, password string) error {
	if !store.StaffRoles[role] {
		return errors.New("role must be one of: owner, onboarding, support, billing")
	}
	if len(password) < 12 {
		return errors.New("staff passwords must be at least 12 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = st.CreateStaff(ctx, email, name, role, hash)
	return err
}
