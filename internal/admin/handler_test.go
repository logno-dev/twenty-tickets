package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/config"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
)

func TestAdminFlowAuthenticationCSRFAndSchemaForm(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	drafts, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(store, drafts, "admin", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, values url.Values, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth {
			r.SetBasicAuth("admin", "correct-password")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/admin/", nil, false); w.Code != 401 {
		t.Fatal("dashboard exposed")
	}
	if w := request("POST", "/admin/connection", nil, false); w.Code != 401 {
		t.Fatal("unauthenticated write")
	}
	if w := request("POST", "/admin/connection", nil, true); w.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	w := request("GET", "/admin/connection", nil, true)
	token := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || len(token) != 2 {
		t.Fatalf("form not rendered: %d %s", w.Code, w.Body.String())
	}
	const key = "private-api-key"
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"data":{"objects":[{"id":"obj","nameSingular":"ticket","namePlural":"tickets","labelSingular":"Ticket","fields":[{"id":"f-name","name":"name","label":"Name","type":"TEXT","isNullable":false},{"id":"f-body","name":"issueOrRequest","label":"Issue","type":"RICH_TEXT","isNullable":true},{"id":"f-status","name":"status","label":"Status","type":"SELECT","isNullable":false,"defaultValue":"'OPEN'","options":[{"label":"Open","value":"OPEN"}]}]}]}}`)
	}))
	defer crm.Close()
	w = request("POST", "/admin/connection", url.Values{"csrf": {token[1]}, "name": {"<script>alert(1)</script>"}, "url": {crm.URL}, "key": {key}}, true)
	if w.Code != 303 {
		t.Fatalf("connection save: %d %s", w.Code, w.Body.String())
	}
	connections, err := store.Connections()
	if err != nil || len(connections) != 1 {
		t.Fatal("connection missing")
	}
	id := connections[0].ID
	w = request("GET", "/admin/", nil, true)
	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("unescaped admin content")
	}
	w = request("GET", "/admin/connection?id="+id, nil, true)
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("saved API key exposed")
	}
	w = request("GET", "/admin/route?connection="+id+"&object=obj", nil, true)
	for _, want := range []string{`name="from_domain"`, `value="cleaned_subject" selected`, `>Cleaned subject</option>`, `>Raw subject</option>`, `value="cleaned_body" selected`, `>Cleaned body</option>`, `>Raw body</option>`, `value="OPEN"`, `name="source_f-status"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("schema-aware form missing %s: %s", want, w.Body.String())
		}
	}
	w = request("POST", "/admin/route", url.Values{"csrf": {token[1]}, "name": {"Support"}, "connection": {id}, "object": {"obj"}, "inbound": {"support@example.com"}, "from_domain": {"SomeDomain.COM, @Partner.org, somedomain.com"}, "enabled": {"on"}, "source_f-name": {"cleaned_subject"}, "source_f-body": {"cleaned_body"}, "source_f-status": {"default"}}, true)
	if w.Code != 303 {
		t.Fatalf("route save: %d %s", w.Code, w.Body.String())
	}
	routes, err := store.Match([]string{"support@example.com"}, "User <user@somedomain.com>")
	if err != nil || len(routes) != 1 {
		t.Fatalf("route not active: %v %v", routes, err)
	}
	if routes[0].FromDomain != "somedomain.com, partner.org" {
		t.Fatal("sender domain was not normalized and snapshotted")
	}
	savedRoutes, err := store.Routes()
	if err != nil || len(savedRoutes) != 1 {
		t.Fatal("saved route unavailable", err)
	}
	w = request("GET", "/admin/route?id="+savedRoutes[0].ID, nil, true)
	if !strings.Contains(w.Body.String(), `name="from_domain" value="somedomain.com, partner.org"`) {
		t.Fatal("sender restriction missing from edit form")
	}
	if partner, err := store.Match([]string{"support@example.com"}, "user@partner.org"); err != nil || len(partner) != 1 {
		t.Fatal("second allowed domain did not match")
	}
	if blocked, err := store.Match([]string{"support@example.com"}, "user@other.com"); err != nil || len(blocked) != 0 {
		t.Fatal("disallowed sender matched route")
	}
}

func TestActivityUIShowsFailuresWithoutDraft(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	drafts, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(store, drafts, "admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	for n := range 52 {
		err = store.RecordActivity(activity.Event{Email: activity.Email{ID: "failed-email", Subject: "Printer <script>alert(1)</script>", From: "person@example.com", To: "support@example.com"}, AttemptID: fmt.Sprintf("attempt-%d", n), Stage: "resend_fetch", Status: "failure", Detail: "context deadline exceeded"})
		if err != nil {
			t.Fatal(err)
		}
	}
	request := func(path string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if auth {
			r.SetBasicAuth("admin", "password")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/admin/activity", "/admin/activity/email?id=failed-email"} {
		if w := request(path, false); w.Code != 401 {
			t.Fatal("activity accessible without authentication")
		}
	}
	w := request("/admin/activity?q=Printer", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "failed-email") || !strings.Contains(w.Body.String(), "Retrieve from Resend") || !strings.Contains(w.Body.String(), "failure") {
		t.Fatalf("missing pre-draft failure: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("unescaped email subject")
	}
	w = request("/admin/activity?q=does-not-match", true)
	if strings.Contains(w.Body.String(), "failed-email") {
		t.Fatal("search ignored")
	}
	w = request("/admin/activity/email?id=failed-email", true)
	for _, want := range []string{"person@example.com", "support@example.com", "context deadline exceeded", "Older", "attempt-51"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("detail missing %q", want)
		}
	}
	if strings.Contains(w.Body.String(), "title=\"attempt-0\"") {
		t.Fatal("event pagination not applied")
	}
	w = request("/admin/activity/email?id=failed-email&page=1", true)
	if !strings.Contains(w.Body.String(), "attempt-0") || !strings.Contains(w.Body.String(), "Newer") {
		t.Fatal("older event history inaccessible")
	}
	w = request("/admin/", true)
	if !strings.Contains(w.Body.String(), "failed-email") {
		t.Fatal("homepage hides email without draft")
	}
	if w := request("/admin/activity/email?id=unknown", true); w.Code != 404 {
		t.Fatal("unknown email should be 404")
	}
}

func TestFieldsForLegacyBodyMapping(t *testing.T) {
	nullable := true
	object := twenty.Object{Fields: []twenty.Field{{ID: "body", Name: "issueOrRequest", Label: "Issue", Type: "RICH_TEXT", IsNullable: &nullable}, {ID: "name", Name: "name", Label: "Name", Type: "TEXT", IsNullable: &nullable}}}
	fields := fieldsFor(object, []routing.Mapping{{Field: "issueOrRequest", Source: "body"}, {Field: "name", Source: "subject"}})
	byName := map[string]fieldView{}
	for _, field := range fields {
		byName[field.Field.Name] = field
	}
	if byName["issueOrRequest"].Source != "cleaned_body" || byName["name"].Source != "cleaned_subject" {
		t.Fatalf("legacy mappings were not presented as cleaned values: %+v", fields)
	}
	options := map[string]bool{}
	for _, option := range byName["issueOrRequest"].Sources {
		options[option.Value] = true
	}
	for _, option := range byName["name"].Sources {
		options[option.Value] = true
	}
	if !options["cleaned_body"] || !options["raw_body"] || !options["cleaned_subject"] || !options["raw_subject"] {
		t.Fatal("body source choices missing")
	}
}
