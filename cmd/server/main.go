// Command server runs Calendar Dynamics: the customer app, the private staff
// admin panel, and the background job queue — all in one process.
//
//	server serve          run everything (default)
//	server migrate        apply database migrations and exit
//	server create-staff   create a staff login for the admin panel
//	server seed-demo      create a demo business with sample data
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed time zone data so the container needs no OS packages

	"calendardynamics/internal/admin"
	"calendardynamics/internal/app"
	"calendardynamics/internal/auth"
	"calendardynamics/internal/config"
	"calendardynamics/internal/db"
	"calendardynamics/internal/jobs"
	"calendardynamics/internal/keys"
	"calendardynamics/internal/messaging"
	"calendardynamics/internal/store"
	"calendardynamics/internal/web"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "migrate":
		err = withApp(func(ctx context.Context, a *app.App) error { return nil })
	case "create-staff":
		err = createStaff(args)
	case "seed-demo":
		err = withApp(seedDemo)
	default:
		err = fmt.Errorf("unknown command %q (use serve, migrate, create-staff, seed-demo)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	if cfg.Prod() {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// build opens the database, runs migrations and wires the app together.
func build(ctx context.Context) (*app.App, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	log := newLogger(cfg)
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	kr, err := keys.New(cfg.MasterKey)
	if err != nil {
		database.Close()
		return nil, nil, err
	}

	var sms messaging.SMS = messaging.Log{L: log}
	var email messaging.Email = messaging.Log{L: log}
	if cfg.TwilioSID != "" && cfg.TwilioToken != "" {
		sms = messaging.Twilio{AccountSID: cfg.TwilioSID, AuthToken: cfg.TwilioToken}
	}
	if cfg.PostmarkToken != "" {
		email = messaging.Postmark{ServerToken: cfg.PostmarkToken}
	}
	if cfg.Prod() && (cfg.TwilioSID == "" || cfg.PostmarkToken == "") {
		log.Warn("messaging providers not fully configured; unconfigured channels are logged instead of sent")
	}

	a := &app.App{
		Cfg:   cfg,
		Store: store.New(database, kr),
		Queue: jobs.New(database, log, app.Queues),
		SMS:   sms,
		Email: email,
		Log:   log,
	}
	a.RegisterJobs()
	return a, func() { database.Close() }, nil
}

func withApp(fn func(context.Context, *app.App) error) error {
	ctx := context.Background()
	a, closeFn, err := build(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	return fn(ctx, a)
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, closeFn, err := build(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	log := a.Log

	if err := a.Queue.Start(context.Background()); err != nil {
		return fmt.Errorf("start job queue: %w", err)
	}

	public := &http.Server{
		Addr:              a.Cfg.PublicAddr,
		Handler:           web.New(a).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Handler:           admin.New(a).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("customer app listening", "addr", a.Cfg.PublicAddr, "env", a.Cfg.Env)
		if err := public.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("public server: %w", err)
		}
	}()
	go func() {
		ln, err := net.Listen("tcp", a.Cfg.AdminAddr)
		if err != nil {
			// The customer app keeps running even if the admin address isn't available.
			log.Error("admin panel not started", "addr", a.Cfg.AdminAddr, "err", err)
			return
		}
		log.Info("staff admin listening (private)", "addr", ln.Addr().String())
		if err := adminSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("admin server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		log.Error("server failed", "err", err)
	}

	// Graceful shutdown: stop taking requests, let in-flight requests and
	// running jobs finish. Anything unfinished resumes on next start.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	if err := a.Queue.Stop(shutdownCtx); err != nil {
		log.Warn("jobs still running at shutdown; they will resume on restart", "err", err)
	}
	log.Info("stopped")
	return nil
}

func createStaff(args []string) error {
	fs := flag.NewFlagSet("create-staff", flag.ExitOnError)
	email := fs.String("email", "", "staff email (required)")
	name := fs.String("name", "", "staff name (required)")
	role := fs.String("role", "owner", "owner | onboarding | support | billing")
	_ = fs.Parse(args)
	if *email == "" || *name == "" {
		return errors.New("usage: server create-staff -email you@company.com -name \"Your Name\" [-role owner]")
	}
	password := os.Getenv("STAFF_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "Password (min 12 characters): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		password = strings.TrimRight(line, "\r\n")
	}
	return withApp(func(ctx context.Context, a *app.App) error {
		if err := admin.CreateStaff(ctx, a.Store, *email, *name, *role, password); err != nil {
			return err
		}
		a.Store.Audit(ctx, "system", "", "", "staff.created", *email+" ("+*role+")")
		fmt.Printf("Created staff user %s (%s).\n", *email, *role)
		return nil
	})
}

// seedDemo creates a demo business with sample contacts and events.
func seedDemo(ctx context.Context, a *app.App) error {
	email := os.Getenv("DEMO_EMAIL")
	if email == "" {
		email = "demo@calendardynamics.test"
	}
	if _, err := a.Store.UserByEmail(ctx, email); err == nil {
		fmt.Println("Demo account already exists:", email)
		return nil
	}
	password := os.Getenv("DEMO_PASSWORD")
	if password == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		password = base64.RawURLEncoding.EncodeToString(b)
	}
	biz, err := a.Store.CreateBusiness(ctx, store.NewBusiness{
		Name: "Rivera Plumbing & Electric", Trade: "Plumbing & Electrical", Timezone: "America/Chicago",
		Plan: "crew", OwnerName: "Jordan Rivera", OwnerEmail: email,
	})
	if err != nil {
		return err
	}
	if err := a.Store.SetBusinessStatus(ctx, biz.ID, "active"); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := a.Store.CreateUser(ctx, biz.ID, email, "Jordan Rivera", hash, "owner"); err != nil {
		return err
	}
	t, err := a.Store.Tenant(ctx, biz.ID)
	if err != nil {
		return err
	}

	people := []store.Contact{
		{Name: "Dana Whitfield", Phone: "(847) 555-0142", Email: "dana.w@example.com", Address: "412 Maple St", SMSConsent: true},
		{Name: "Marcus Lee", Phone: "(847) 555-0178", Email: "marcus.lee@example.com", Address: "88 Birch Ln", SMSConsent: true},
		{Name: "Priya Shah", Phone: "(224) 555-0115", Email: "priya.s@example.com", Address: "1530 Oak Ridge Dr", SMSConsent: true},
		{Name: "Tom Alvarez", Phone: "(847) 555-0193", Email: "t.alvarez@example.com", Address: "27 Elm Ct", SMSConsent: true},
		{Name: "Kessler Homes", Phone: "(847) 555-0100", Email: "office@kessler.example", Address: "Lot 14, Prairie View"},
		{Name: "Ray Benson", Phone: "(847) 555-0127", Email: "ray.b@example.com", Address: "61 Willow Way"},
		{Name: "Linda Park", Phone: "(224) 555-0186", Email: "lpark@example.com", Address: "340 Cedar St", SMSConsent: true},
	}
	ids := map[string]string{}
	for i := range people {
		if err := t.SaveContact(ctx, &people[i], "Demo data"); err != nil {
			return err
		}
		ids[strings.Fields(people[i].Name)[0]] = people[i].ID
	}

	loc := biz.Location()
	now := time.Now().In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	at := func(off, h, m int) time.Time {
		return day.AddDate(0, 0, off).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	type seed struct {
		who            string
		off, h, m, dur int
		typ, title     string
		notes, channel string
		message        string
	}
	seeds := []seed{
		{"Dana", -40, 10, 0, 60, "estimate", "Water heater inspection", "Unit is 14 years old, sediment buildup. Quoted replacement.", "", ""},
		{"Dana", -14, 12, 15, 15, "call", "Called to accept quote", "Wants install before the holidays.", "", ""},
		{"Dana", 0, 8, 0, 120, "job", "Water heater replacement", "40-gal gas unit. Bring expansion tank.", "", ""},
		{"Dana", 2, 10, 0, 15, "followup", "Email: review request", "", "email", "Thanks for having us out! If you have a minute, a quick review helps a small shop like ours a lot."},
		{"Marcus", -3, 16, 0, 15, "call", "Intro call — bathroom remodel", "Referred by Dana Whitfield.", "", ""},
		{"Marcus", 0, 11, 30, 60, "estimate", "Bathroom remodel walkthrough", "", "", ""},
		{"Marcus", 5, 10, 0, 15, "followup", "Text: quote follow-up", "", "sms", "Hi Marcus, any questions about the bathroom remodel quote? Happy to walk through it."},
		{"Priya", -6, 13, 0, 45, "estimate", "Garbage disposal quote", "", "", ""},
		{"Priya", 1, 15, 0, 15, "followup", "Text: quote reminder", "", "sms", "Hi Priya, friendly reminder that your disposal quote is good through the end of the month. Reply YES to get on the schedule."},
		{"Tom", 1, 9, 0, 180, "job", "Panel upgrade 200A", "Permit pulled. Utility disconnect scheduled 8:30.", "", ""},
		{"Kessler", 3, 7, 30, 240, "job", "Rough-in plumbing, new build", "", "", ""},
		{"Ray", 4, 13, 0, 60, "estimate", "AC not cooling", "", "", ""},
		{"Linda", -3, 9, 0, 120, "job", "Sump pump install", "", "", ""},
		{"Linda", -1, 18, 10, 15, "note", "Paid in full", "Check #4471", "", ""},
	}
	for _, s := range seeds {
		ev := store.Event{ContactID: ids[s.who], Type: s.typ, Title: s.title, Start: at(s.off, s.h, s.m),
			DurationMins: s.dur, Notes: s.notes, Channel: s.channel, Message: s.message}
		if err := t.SaveEvent(ctx, &ev); err != nil {
			return err
		}
		if err := a.ScheduleFollowup(ctx, biz.ID, ev); err != nil {
			return err
		}
	}
	a.Store.Audit(ctx, "system", "", biz.ID, "demo.seeded", "")
	fmt.Printf("Demo business created.\n  Sign in at %s/login\n  Email:    %s\n  Password: %s\n", a.Cfg.BaseURL, email, password)
	return nil
}
