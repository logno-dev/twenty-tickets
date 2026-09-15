package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"twenty-tickets/internal/routing"
)

const metadata = `{"data":{"objects":[{"id":"obj","nameSingular":"ticket","namePlural":"tickets","labelSingular":"Ticket","fields":[{"id":"name","name":"name","label":"Name","type":"TEXT","isNullable":false}]}]}}`

func TestConfigurationPersistenceEncryptionAndSnapshots(t *testing.T) {
	const key = "very-secret-twenty-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, metadata)
	}))
	defer srv.Close()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Match([]string{"support@example.com"}, "user@example.com"); err == nil {
		t.Fatal("unconfigured installation should request a retry")
	}
	id, err := s.SaveConnection(context.Background(), "", "Company A", srv.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	route := routing.Route{Name: "Support", Inbound: "support@example.com", FromDomain: "somedomain.com", ConnectionID: id, ObjectID: "obj", Enabled: true, Mappings: []routing.Mapping{{Field: "name", Source: "subject"}}}
	if err := s.SaveRoute(route); err != nil {
		t.Fatal(err)
	}
	matched, err := s.Match([]string{"Support <SUPPORT@example.com>"}, "User <user@somedomain.com>")
	if err != nil || len(matched) != 1 {
		t.Fatalf("match: %v %v", matched, err)
	}
	snapshot, _ := json.Marshal(matched)
	routes, _ := s.Routes()
	route = routes[0]
	route.Mappings[0] = routing.Mapping{Field: "name", Source: "fixed", Value: "Changed"}
	route.Enabled = false
	if err := s.SaveRoute(route); err != nil {
		t.Fatal(err)
	}
	if current, err := s.Match([]string{"support@example.com"}, "user@example.com"); err != nil || len(current) != 0 {
		t.Fatal("disabled route still matches")
	}
	if got, _ := json.Marshal(matched); !bytes.Equal(got, snapshot) {
		t.Fatal("accepted snapshot mutated")
	}
	if matched[0].FromDomain != "somedomain.com" {
		t.Fatal("sender restriction missing from snapshot")
	}
	if _, err := s.SaveConnection(context.Background(), id, "Changed", srv.URL+"/other", key); err == nil {
		t.Fatal("existing destination redirected")
	}
	if _, err := s.SaveConnection(context.Background(), id, "Company A", srv.URL, ""); err != nil {
		t.Fatal("blank key did not preserve credential:", err)
	}
	if _, err := s.SaveConnection(context.Background(), id, "Company A", srv.URL, "wrong"); err == nil {
		t.Fatal("failed verification committed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key)) {
		t.Fatal("plaintext API key in database")
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := s.Client(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Objects(context.Background()); err != nil {
		t.Fatal("credential lost on restart:", err)
	}
	connections, err := s.Connections()
	if err != nil || len(connections) != 1 || len(connections[0].Objects) != 1 {
		t.Fatal("schema/config lost")
	}
}
func TestMissingEncryptionKeyFails(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(filepath.Join(dir, "config.key")); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("silently replaced missing encryption key")
	}
}
