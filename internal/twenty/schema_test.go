package twenty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObjectsEnvelopeAndPagination(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/rest/metadata/objects" || r.URL.Query().Get("limit") != "100" || r.Header.Get("Authorization") != "Bearer test" {
					t.Error("bad metadata request")
				}
				data := `[{"id":"one","nameSingular":"ticket","namePlural":"tickets","fields":[{"id":"f","name":"status","label":"Status","type":"SELECT","defaultValue":"'OPEN'","options":[{"label":"Open","value":"OPEN"}]}]}]`
				next := true
				cursor := "cursor1"
				if calls == 2 {
					if r.URL.Query().Get("starting_after") != "cursor1" {
						t.Error("cursor missing")
					}
					next = false
					data = `[{"id":"two","nameSingular":"task","namePlural":"tasks","fields":[]}]`
				}
				if legacy {
					data = `{"objects":` + data + `}`
				}
				fmt.Fprintf(w, `{"data":%s,"pageInfo":{"hasNextPage":%v,"endCursor":%q}}`, data, next, cursor)
			}))
			defer s.Close()
			c, _ := New(s.URL, "test")
			objects, err := c.Objects(context.Background())
			if err != nil || len(objects) != 2 || calls != 2 {
				t.Fatalf("objects=%v calls=%d err=%v", objects, calls, err)
			}
			if objects[0].Fields[0].Options[0].Value != "OPEN" {
				t.Fatal("select metadata lost")
			}
		})
	}
}
func TestObjectsRejectsBrokenResponse(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":{}}`, `{"data":[],"pageInfo":{"hasNextPage":true}}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c, _ := New(s.URL, "test")
		if _, err := c.Objects(context.Background()); err == nil {
			t.Errorf("accepted %s", body)
		}
		s.Close()
	}
}
