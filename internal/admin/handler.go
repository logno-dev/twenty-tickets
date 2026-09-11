// Package admin serves the authenticated, server-rendered configuration UI.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"twenty-tickets/internal/config"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
)

//go:embed page.html
var pageHTML string
var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

type Handler struct {
	store          *config.Store
	inbox          *inbox.Store
	user, password [32]byte
	csrf           string
	mux            *http.ServeMux
}
type fieldView struct {
	Field         twenty.Field
	Source, Value string
	Sources       []string
}
type draftView struct {
	ID, Subject, Status string
	Legacy              bool
	Deliveries          []string
}
type page struct {
	CSRF, Title, Error, Notice, View string
	Connections                      []config.Connection
	Routes                           []routing.Route
	Connection                       config.Connection
	Route                            routing.Route
	Objects                          []twenty.Object
	Fields                           []fieldView
	Drafts                           []draftView
}

func New(store *config.Store, inboxStore *inbox.Store, user, password string) (http.Handler, error) {
	if strings.TrimSpace(user) == "" || password == "" {
		return nil, fmt.Errorf("ADMIN_USERNAME and ADMIN_PASSWORD are required")
	}
	if strings.Contains(user, ":") {
		return nil, fmt.Errorf("ADMIN_USERNAME cannot contain a colon")
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	h := &Handler{store: store, inbox: inboxStore, user: sha256.Sum256([]byte(user)), password: sha256.Sum256([]byte(password)), csrf: hex.EncodeToString(bytes), mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /admin/{$}", h.home)
	h.mux.HandleFunc("GET /admin/connection", h.connection)
	h.mux.HandleFunc("POST /admin/connection", h.saveConnection)
	h.mux.HandleFunc("GET /admin/route", h.route)
	h.mux.HandleFunc("POST /admin/route", h.saveRoute)
	h.mux.HandleFunc("POST /admin/assign", h.assign)
	return h, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	user, password, ok := r.BasicAuth()
	u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))
	if !ok || subtle.ConstantTimeCompare(u[:], h.user[:])&subtle.ConstantTimeCompare(p[:], h.password[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="Twenty Tickets", charset="UTF-8"`)
		http.Error(w, "Sign in with your admin credentials", 401)
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid or oversized form", 400)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(h.csrf)) != 1 {
			http.Error(w, "invalid form token; reload the page", 403)
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}
func (h *Handler) render(w http.ResponseWriter, p page) {
	p.CSRF = h.csrf
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTemplate.Execute(w, p)
}
func (h *Handler) fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	h.render(w, page{Title: "Unable to save", Error: err.Error()})
}
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	connections, err := h.store.Connections()
	if err != nil {
		h.fail(w, err)
		return
	}
	routes, err := h.store.Routes()
	if err != nil {
		h.fail(w, err)
		return
	}
	var drafts []inbox.Draft
	err = h.inbox.EachDraft(r.Context(), func(d inbox.Draft) error {
		drafts = append(drafts, d)
		sort.Slice(drafts, func(i, j int) bool { return drafts[i].SavedAt.After(drafts[j].SavedAt) })
		if len(drafts) > 100 {
			drafts = drafts[:100]
		}
		return nil
	})
	var views []draftView
	for _, d := range drafts {
		v := draftView{ID: d.Email.ID, Subject: d.Email.Subject, Status: d.Status}
		destinations, e := h.inbox.Destinations(d)
		if e != nil {
			v.Deliveries = append(v.Deliveries, "Cannot read delivery state")
		} else {
			v.Legacy = d.Version == 1 && d.Status == "pending_twenty" && len(destinations) == 0
			if d.Version == 1 {
				state, e := h.inbox.LoadDelivery(d.Email.ID)
				if e == nil && state.Status == "delivered" {
					v.Legacy = false
					v.Deliveries = append(v.Deliveries, "Legacy delivery complete: "+state.TicketID)
				}
			}
			for _, destination := range destinations {
				state, e := h.inbox.LoadDestinationDelivery(d.Email.ID, destination.RouteID)
				if e != nil {
					v.Deliveries = append(v.Deliveries, destination.RouteName+": cannot read state")
					continue
				}
				status := state.Status
				if status == "" {
					status = "pending"
				}
				detail := destination.RouteName + ": " + status
				if state.TicketID != "" {
					detail += " · " + state.TicketID
				}
				if state.LastError != "" {
					detail += " · " + state.LastError + " · retry " + state.NextAttemptAt.Format(time.RFC3339)
				}
				v.Deliveries = append(v.Deliveries, detail)
			}
		}
		views = append(views, v)
	}
	p := page{Title: "Twenty Tickets", View: "home", Connections: connections, Routes: routes, Drafts: views, Notice: r.URL.Query().Get("notice")}
	if err != nil {
		p.Error = "Some drafts could not be read: " + err.Error()
	}
	h.render(w, p)
}
func (h *Handler) connection(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Twenty connection", View: "connection"}
	if id := r.URL.Query().Get("id"); id != "" {
		c, err := h.store.Connection(id)
		if err != nil {
			h.fail(w, err)
			return
		}
		p.Connection = c
	}
	h.render(w, p)
}
func (h *Handler) saveConnection(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	_, err := h.store.SaveConnection(ctx, r.PostForm.Get("id"), r.PostForm.Get("name"), r.PostForm.Get("url"), r.PostForm.Get("key"))
	if err != nil {
		h.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/?notice="+url.QueryEscape("Connection verified and object metadata refreshed."), http.StatusSeeOther)
}
func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Inbound route", View: "route"}
	var err error
	p.Connections, err = h.store.Connections()
	if err != nil {
		h.fail(w, err)
		return
	}
	if id := r.URL.Query().Get("id"); id != "" {
		p.Route, err = h.store.Route(id)
		if err != nil {
			h.fail(w, err)
			return
		}
	}
	if v := r.URL.Query().Get("connection"); v != "" {
		if v != p.Route.ConnectionID {
			p.Route.ObjectID = ""
			p.Route.Mappings = nil
		}
		p.Route.ConnectionID = v
	}
	if v := r.URL.Query().Get("object"); v != "" {
		if v != p.Route.ObjectID {
			p.Route.Mappings = nil
		}
		p.Route.ObjectID = v
	}
	if p.Route.ConnectionID != "" {
		p.Connection, err = h.store.Connection(p.Route.ConnectionID)
		if err != nil {
			h.fail(w, err)
			return
		}
		p.Objects = p.Connection.Objects
		for _, object := range p.Objects {
			if object.ID != p.Route.ObjectID {
				continue
			}
			p.Fields = fieldsFor(object, p.Route.Mappings)
		}
	}
	h.render(w, p)
}
func fieldsFor(object twenty.Object, mappings []routing.Mapping) []fieldView {
	var result []fieldView
	for _, f := range object.Fields {
		if !f.Editable() {
			continue
		}
		v := fieldView{Field: f, Source: "default", Sources: []string{"default"}}
		if f.Supported() {
			v.Sources = append(v.Sources, "fixed")
			if f.IsNullable != nil && *f.IsNullable {
				v.Sources = append(v.Sources, "empty")
			}
			if f.Type == "TEXT" || f.Type == "RICH_TEXT" {
				v.Sources = append(v.Sources, "subject", "body", "from", "email_id", "message_id", "received_at")
			} else if f.Type == "DATE" || f.Type == "DATE_TIME" {
				v.Sources = append(v.Sources, "received_at")
			}
		}
		if len(mappings) == 0 {
			if f.Name == "name" && f.Type == "TEXT" {
				v.Source = "subject"
			}
			if f.Name == "issueOrRequest" && f.Type == "RICH_TEXT" {
				v.Source = "body"
			}
		}
		for _, m := range mappings {
			if m.Field == f.Name {
				v.Source = m.Source
				v.Value = m.Value
			}
		}
		result = append(result, v)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Field.Label < result[j].Field.Label })
	return result
}
func (h *Handler) saveRoute(w http.ResponseWriter, r *http.Request) {
	route := routing.Route{ID: r.PostForm.Get("id"), Name: r.PostForm.Get("name"), Inbound: strings.TrimSpace(r.PostForm.Get("inbound")), ConnectionID: r.PostForm.Get("connection"), ObjectID: r.PostForm.Get("object"), Enabled: r.PostForm.Get("enabled") == "on"}
	c, err := h.store.Connection(route.ConnectionID)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, object := range c.Objects {
		if object.ID != route.ObjectID {
			continue
		}
		for _, field := range object.Fields {
			if !field.Editable() {
				continue
			}
			route.Mappings = append(route.Mappings, routing.Mapping{Field: field.Name, Source: r.PostForm.Get("source_" + field.ID), Value: r.PostForm.Get("value_" + field.ID)})
		}
	}
	if err := h.store.SaveRoute(route); err != nil {
		h.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/?notice="+url.QueryEscape("Route saved. Changes apply to newly accepted emails."), http.StatusSeeOther)
}
func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
	route, err := h.store.Route(r.PostForm.Get("route"))
	if err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.inbox.Draft(r.PostForm.Get("email"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !route.Matches(d.Email.To) || route.Singular != "ticket" || route.Plural != "tickets" {
		h.fail(w, fmt.Errorf("select an enabled tickets route matching the draft's To address"))
		return
	}
	if err := h.inbox.AssignLegacy(d.Email.ID, route.Snapshot()); err != nil {
		h.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/?notice="+url.QueryEscape("Legacy draft assigned. Its original stable ticket ID will be reused."), http.StatusSeeOther)
}
