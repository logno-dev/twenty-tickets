package routing

import (
	"encoding/json"
	"testing"

	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/twenty"
)

func schema(t *testing.T) twenty.Object {
	t.Helper()
	var o twenty.Object
	err := json.Unmarshal([]byte(`{
"id":"obj","nameSingular":"ticket","namePlural":"tickets","fields":[
{"id":"name","name":"name","label":"Name","type":"TEXT","isNullable":false},
{"id":"body","name":"issueOrRequest","label":"Issue","type":"RICH_TEXT","isNullable":true},
{"id":"status","name":"status","label":"Status","type":"SELECT","isNullable":false,"defaultValue":"'OPEN'","options":[{"label":"Open","value":"OPEN"}]},
{"id":"bool","name":"flag","label":"Flag","type":"BOOLEAN","isNullable":true},
{"id":"app","name":"app","label":"App","type":"RELATION","isNullable":true,"settings":{"relationType":"MANY_TO_ONE","joinColumnName":"appId"}},
{"id":"count","name":"count","label":"Count","type":"NUMBER","isNullable":true,"settings":{"dataType":"int"}},
{"id":"id","name":"id","label":"ID","type":"UUID","isNullable":false}
]}`), &o)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func TestTypedMapping(t *testing.T) {
	r := Route{Name: "Support", Inbound: "support@example.com", ObjectID: "obj", Enabled: true, Mappings: []Mapping{
		{Field: "name", Source: "subject", Type: "BOOLEAN", Target: "id"},
		{Field: "issueOrRequest", Source: "body"}, {Field: "status", Source: "default"},
		{Field: "flag", Source: "fixed", Value: "false"},
		{Field: "app", Source: "fixed", Value: "123e4567-e89b-12d3-a456-426614174000"},
		{Field: "count", Source: "fixed", Value: "12"},
	}}
	if err := Validate(&r, schema(t)); err != nil {
		t.Fatal(err)
	}
	fields, err := r.Snapshot().Payload(resend.Email{Subject: ""}, "Note\n\nForward")
	if err != nil {
		t.Fatal(err)
	}
	if fields["name"] != "(No subject)" || fields["flag"] != false || fields["appId"] != "123e4567-e89b-12d3-a456-426614174000" || fields["count"] != json.Number("12") {
		t.Fatalf("wrong typed payload: %#v", fields)
	}
	if _, ok := fields["status"]; ok {
		t.Fatal("default sent")
	}
	if _, ok := fields["id"]; ok {
		t.Fatal("client-controlled target accepted")
	}
	if fields["issueOrRequest"].(map[string]string)["markdown"] != "Note\n\nForward" {
		t.Fatal("rich text lost")
	}
	if !r.Matches([]string{"Support <SUPPORT@example.com>"}) || r.Matches([]string{"other@example.com"}) {
		t.Fatal("bad routing")
	}
}
func TestInvalidMappings(t *testing.T) {
	for _, mapping := range []Mapping{{Field: "status", Source: "fixed", Value: "BAD"}, {Field: "flag", Source: "fixed", Value: "yes"}, {Field: "count", Source: "fixed", Value: "1.5"}, {Field: "app", Source: "fixed", Value: "not-uuid"}, {Field: "status", Source: "body"}, {Field: "id", Source: "fixed", Value: "x"}, {Field: "name", Source: "empty"}, {Field: "name", Source: "default"}} {
		r := Route{Name: "Support", Inbound: "support@example.com", ObjectID: "obj", Mappings: []Mapping{mapping}}
		if mapping.Field != "name" {
			r.Mappings = append(r.Mappings, Mapping{Field: "name", Source: "subject"})
		}
		if err := Validate(&r, schema(t)); err == nil {
			t.Errorf("accepted invalid mapping: %+v", mapping)
		}
	}
}

func TestSnapshotDoesNotFollowRouteEdits(t *testing.T) {
	r := Route{ID: "route-a", Mappings: []Mapping{{Field: "name", Source: "subject", Type: "TEXT"}}}
	snapshot := r.Snapshot()
	r.Mappings[0].Source = "fixed"
	r.Mappings[0].Value = "changed"
	if snapshot.Mappings[0].Source != "subject" {
		t.Fatal("snapshot changed with route")
	}
	if snapshot.RecordID("email") == (Destination{RouteID: "route-b"}).RecordID("email") {
		t.Fatal("destinations share identity")
	}
}

func TestCleanedAndRawBodySources(t *testing.T) {
	raw := "Please handle.\n\n---------- Forwarded message ---------\nFrom: A <a@example.com>\nDate: Tue\nSubject: Help\nTo: B\n\nPrinter is broken.\n\nOn Mon, B wrote:\n> Older"
	email := resend.Email{Text: &raw}
	destination := Destination{Mappings: []Mapping{
		{Field: "cleaned", Type: "TEXT", Source: "cleaned_body"},
		{Field: "raw", Type: "TEXT", Source: "raw_body"},
		{Field: "legacy", Type: "TEXT", Source: "body"},
	}}
	fields, err := destination.Payload(email, "Please handle.\n\nPrinter is broken.")
	if err != nil {
		t.Fatal(err)
	}
	if fields["cleaned"] != "Please handle.\n\nPrinter is broken." || fields["legacy"] != fields["cleaned"] {
		t.Fatalf("cleaned body mapping changed: %#v", fields)
	}
	if fields["raw"] != raw {
		t.Fatal("raw body was cleaned")
	}
}

func TestCleanedAndRawSubjectSources(t *testing.T) {
	destination := Destination{Mappings: []Mapping{
		{Field: "cleaned", Type: "TEXT", Source: "cleaned_subject"},
		{Field: "raw", Type: "TEXT", Source: "raw_subject"},
		{Field: "legacy", Type: "TEXT", Source: "subject"},
	}}
	fields, err := destination.Payload(resend.Email{Subject: "Re: Fwd: Printer problem"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if fields["cleaned"] != "Printer problem" || fields["legacy"] != fields["cleaned"] {
		t.Fatalf("subject was not cleaned: %#v", fields)
	}
	if fields["raw"] != "Re: Fwd: Printer problem" {
		t.Fatal("raw subject was changed")
	}
}
