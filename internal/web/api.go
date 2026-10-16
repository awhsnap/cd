package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"calendardynamics/internal/store"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request, sess session) error {
	b := sess.Tenant.Biz
	access, err := s.app.Store.RecentSupportAccess(r.Context(), b.ID, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]string{"name": sess.User.Name, "email": sess.User.Email, "role": sess.User.Role},
		"business": map[string]any{
			"name": b.Name, "timezone": b.Timezone, "status": b.Status,
			"textingReady": b.SMSFrom != "" || s.app.Cfg.TwilioFrom != "" || s.app.Cfg.TwilioSID == "",
		},
		"supportAccess": access,
	})
	return nil
}

func parseRange(r *http.Request) (time.Time, time.Time, error) {
	from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 400*24*time.Hour {
		return from, to, fmt.Errorf("%w: from and to must be RFC 3339 times, at most 400 days apart", errBadRequest)
	}
	return from, to, nil
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, sess session) error {
	from, to, err := parseRange(r)
	if err != nil {
		return err
	}
	evs, err := sess.Tenant.ListEvents(r.Context(), from, to)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(evs))
	return nil
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request, sess session) error {
	ev, err := sess.Tenant.GetEvent(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ev)
	return nil
}

type eventInput struct {
	ContactID    string    `json:"contactId"`
	ContactName  string    `json:"contactName"` // used when contactId is empty: finds or creates the contact
	Type         string    `json:"type"`
	Title        string    `json:"title"`
	Start        time.Time `json:"start"`
	DurationMins int       `json:"durationMins"`
	Notes        string    `json:"notes"`
	Channel      string    `json:"channel"`
	Message      string    `json:"message"`
}

func (s *Server) saveEvent(w http.ResponseWriter, r *http.Request, sess session) error {
	var in eventInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	t := sess.Tenant
	ctx := r.Context()
	if in.ContactID == "" {
		name := strings.TrimSpace(in.ContactName)
		if name == "" {
			return fmt.Errorf("%w: pick a contact or type a new name", store.ErrInvalid)
		}
		c, err := t.FindContactByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			c = store.Contact{Name: name}
			if err := t.SaveContact(ctx, &c, ""); err != nil {
				return err
			}
			s.app.Store.Audit(ctx, "user", sess.User.ID, t.ID(), "contact.created", c.ID)
		} else if err != nil {
			return err
		}
		in.ContactID = c.ID
	}
	ev := store.Event{
		ID: r.PathValue("id"), ContactID: in.ContactID, Type: in.Type, Title: in.Title, Start: in.Start,
		DurationMins: in.DurationMins, Notes: in.Notes, Channel: in.Channel, Message: in.Message,
	}
	if err := t.SaveEvent(ctx, &ev); err != nil {
		return err
	}
	if err := s.app.ScheduleFollowup(ctx, t.ID(), ev); err != nil {
		return err
	}
	saved, err := t.GetEvent(ctx, ev.ID)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
	return nil
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request, sess session) error {
	id := r.PathValue("id")
	if err := sess.Tenant.DeleteEvent(r.Context(), id); err != nil {
		return err
	}
	s.app.Store.Audit(r.Context(), "user", sess.User.ID, sess.Tenant.ID(), "event.deleted", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request, sess session) error {
	cs, err := sess.Tenant.ListContacts(r.Context(), r.URL.Query().Get("q"), 500)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(cs))
	return nil
}

func (s *Server) getContact(w http.ResponseWriter, r *http.Request, sess session) error {
	c, err := sess.Tenant.GetContact(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, c)
	return nil
}

func (s *Server) contactEvents(w http.ResponseWriter, r *http.Request, sess session) error {
	id := r.PathValue("id")
	if _, err := sess.Tenant.GetContact(r.Context(), id); err != nil {
		return err
	}
	evs, err := sess.Tenant.ListContactEvents(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(evs))
	return nil
}

func (s *Server) saveContact(w http.ResponseWriter, r *http.Request, sess session) error {
	var c store.Contact
	if err := readJSON(r, &c); err != nil {
		return err
	}
	c.ID = r.PathValue("id")
	if err := sess.Tenant.SaveContact(r.Context(), &c, "Recorded by "+sess.User.Email); err != nil {
		return err
	}
	saved, err := sess.Tenant.GetContact(r.Context(), c.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, saved)
	return nil
}

// export downloads everything the business owns as one JSON file.
func (s *Server) export(w http.ResponseWriter, r *http.Request, sess session) error {
	x, err := sess.Tenant.Export(r.Context())
	if err != nil {
		return err
	}
	s.app.Store.Audit(r.Context(), "user", sess.User.ID, sess.Tenant.ID(), "data.exported", "")
	name := fmt.Sprintf("calendar-dynamics-export-%s.json", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(x)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
