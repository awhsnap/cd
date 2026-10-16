# Calendar Dynamics

The event-based CRM for the trades. Every job, estimate, call, note and
follow-up is an **event on the calendar**, attached to a **contact**, and each
contact's events form their **timeline**. Follow-up events can send a text or
email automatically at their scheduled time.

It's built to run cheaply with very little to manage: **one Go binary, one
SQLite file, one small Fly.io machine**, with continuous backups to object
storage. Roughly $5–10/month to run for your first ~100 businesses.

```
                 your-app.fly.dev (HTTPS)
                          │
   ┌──────────────────────▼───────────────────────┐
   │  One Fly machine — /app/server               │
   │   • customer app + JSON API   :8080 (public) │
   │   • staff admin panel         :9090 (private │
   │       network only — never on the internet)  │
   │   • job queue workers (texts, emails,        │
   │       replies, imports)                      │
   │            │                                 │
   │      /data/app.db  (SQLite on a Fly volume)  │
   └────────────┼─────────────────────────────────┘
                │ Litestream streams every change (≈1s)
                ▼
         Tigris / S3 bucket  — backups + disaster recovery
```

## What's in the box

| | |
|---|---|
| **Calendar dashboard** | Month and week views, day agenda, contact list, contact timeline drawer, event and contact editing. Plain HTML/CSS/JS, served by the Go app. |
| **Automated follow-ups** | A follow-up event with *Text* or *Email* selected is queued to send at its start time. It respects quiet hours, requires texting consent, adds "Reply STOP to opt out" to a customer's first text, and never double-sends. |
| **Inbound texts** | Replies land on the contact's timeline and pause their pending follow-ups. STOP / START opt out and back in. |
| **Staff admin panel** | Phone onboarding checklist: create business → verify owner with an emailed code → payment link → texting (A2P) status → CSV contact import → owner invite → go live. Plus suspend, support sessions, export, delete, audit log. |
| **Data ownership** | Owners can download all their data any time (sidebar → *Export my data*). |
| **Job queue** | Durable, inside SQLite. Separate lanes (`messages`, `default`, `bulk`), retries with backoff, unique jobs, graceful shutdown. Jobs survive restarts and deploys. |

## Security model

- **Tenant isolation.** Customer data is reachable only through a `store.Tenant`, which is bound to one business; every query filters on its ID. `internal/store/tenant_test.go` and `internal/web/server_test.go` prove one business can't read, edit, delete or attach to another's data.
- **Encryption.** Each business has its own data key, stored only wrapped by `MASTER_KEY` (a Fly secret). Event notes and message bodies are encrypted with it. **Deleting a business destroys its key**, so its encrypted data is unreadable even in old backups.
- **Staff can't see customer data by default.** They open a 30-minute *support session* with a reason; it's audited and the business owner sees a banner saying who accessed their account and why.
- **Admin panel is private.** It binds to Fly's private network address, so it's unreachable from the internet. Staff connect with `fly proxy`.
- **Passwords** use PBKDF2-SHA256 (600k iterations). Sessions are random tokens; only their hashes are stored. Staff never set customer passwords — owners set their own via an invite link.
- **Web hardening.** Strict CSP (no inline scripts), CSRF protection (origin check + required header on API writes), HttpOnly/SameSite cookies, login rate limiting, Twilio webhook signature verification.
- **Audit log** is append-only — the database rejects updates and deletes to it.
- **Payments.** The panel only sends a Stripe link; card numbers never pass through the app or the phone call.

## Run it locally

Requires Go 1.24+ and a C compiler (for SQLite).

```bash
cp .env.example .env
make seed      # creates a demo business and prints its login
make staff EMAIL=you@example.com NAME="Your Name"   # prompts for a password (12+ chars)
make dev
```

- Customer app: http://localhost:8080 (sign in with the demo login)
- Staff admin: http://127.0.0.1:9090

Without Twilio/Postmark credentials, texts and emails are **printed to the
console** instead of sent — including staff verification codes and invite links.

Run the tests: `make test`

## Deploy to Fly.io

```bash
# 1. Create the app (don't deploy yet) — pick your own app name
fly launch --no-deploy --copy-config --name calendar-dynamics
#    then set BASE_URL in fly.toml to https://<your-app>.fly.dev

# 2. A 1 GB volume for the database
fly volumes create data --region ord --size 1

# 3. Backup bucket (sets BUCKET_NAME + AWS_* secrets automatically)
fly storage create

# 4. Secrets
fly secrets set MASTER_KEY="$(openssl rand -base64 32)"
#    ...and store that same value in your password manager.
fly secrets set POSTMARK_SERVER_TOKEN=... TWILIO_ACCOUNT_SID=... TWILIO_AUTH_TOKEN=... TWILIO_FROM_NUMBER=+1...

# 5. Deploy as a single machine (SQLite has one writer)
fly deploy --ha=false

# 6. Create your staff login on the machine
fly ssh console -C "/app/server create-staff -email you@company.com -name 'Your Name' -role owner"
```

**Open the admin panel:** `fly proxy 9090:9090`, then browse to http://localhost:9090.

**Twilio:** point your number's incoming-message webhook to
`https://<your-app>.fly.dev/webhooks/twilio/sms` (HTTP POST).

**Demo mode (pennies a month):** in `fly.toml` set `auto_stop_machines = "stop"`
and `min_machines_running = 0`. The machine sleeps when idle and wakes on the
next visit; scheduled follow-ups send once it's awake.

### Important: keep it to one machine

SQLite allows one writer, so this app runs as exactly one machine. Never
`fly scale count 2`. Deploys restart the machine (a few seconds of downtime);
in-flight requests and jobs finish first, and queued jobs resume after.

### Restoring from backup

If a volume is lost, create a new one and redeploy — the entrypoint restores
the latest data from the bucket automatically before the app starts. To roll
back to a point in time, use `litestream restore -timestamp ...` from
`fly ssh console`.

## Code map

```
cmd/server/          entrypoint: serve, migrate, create-staff, seed-demo
internal/config/     environment settings
internal/db/         SQLite setup + migrations (migrations/*.sql)
internal/keys/       per-business envelope encryption
internal/auth/       password hashing, tokens, IDs
internal/store/      ALL database access; Tenant = one business's data
internal/jobs/       durable job queue
internal/messaging/  Twilio, Postmark, log sender; quiet hours; STOP keywords
internal/app/        job handlers (send follow-up, inbound text, import)
internal/web/        customer app, JSON API, dashboard assets
internal/admin/      staff admin panel
deploy/              Litestream config + container entrypoint
```

## When to outgrow this

Move to Postgres (and River for jobs) when you need two or more machines for
zero-downtime deploys, or a customer requires database-level isolation. All
data access is already isolated in `internal/store` and the job handlers don't
depend on the queue's storage, so the move is contained.

## Before going live

- Register each business for **A2P 10DLC** with your SMS provider (the panel tracks status). Unregistered business texting in the US gets filtered.
- Set up **SPF, DKIM and DMARC** for your sending domain in Postmark.
- Add a **dedicated-instance tier** later by deploying this same image as a separate Fly app per customer.
- Litestream is pinned to 0.3.13; test newer releases in staging before upgrading.
