package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"twenty-tickets/internal/config"
	"twenty-tickets/internal/inbox"
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
	for _, want := range []string{`value="subject" selected`, `value="body" selected`, `value="OPEN"`, `name="source_f-status"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("schema-aware form missing %s: %s", want, w.Body.String())
		}
	}
	w = request("POST", "/admin/route", url.Values{"csrf": {token[1]}, "name": {"Support"}, "connection": {id}, "object": {"obj"}, "inbound": {"support@example.com"}, "enabled": {"on"}, "source_f-name": {"subject"}, "source_f-body": {"body"}, "source_f-status": {"default"}}, true)
	if w.Code != 303 {
		t.Fatalf("route save: %d %s", w.Code, w.Body.String())
	}
	routes, err := store.Match([]string{"support@example.com"})
	if err != nil || len(routes) != 1 {
		t.Fatalf("route not active: %v %v", routes, err)
	}
}
