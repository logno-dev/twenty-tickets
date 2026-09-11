package twenty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"twenty-tickets/internal/api"
)

var apiName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type Field struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Label        string          `json:"label"`
	Type         string          `json:"type"`
	IsActive     *bool           `json:"isActive"`
	IsNullable   *bool           `json:"isNullable"`
	IsUIReadOnly bool            `json:"isUIReadOnly"`
	IsUIEditable *bool           `json:"isUIEditable"`
	Writability  string          `json:"writability"`
	DefaultValue json.RawMessage `json:"defaultValue"`
	Options      []Option        `json:"options"`
	Settings     struct {
		RelationType   string `json:"relationType"`
		JoinColumnName string `json:"joinColumnName"`
		DataType       string `json:"dataType"`
	} `json:"settings"`
}

func (f Field) APIName() string {
	if f.Type == "RELATION" && f.Settings.RelationType == "MANY_TO_ONE" && apiName.MatchString(f.Settings.JoinColumnName) {
		return f.Settings.JoinColumnName
	}
	return f.Name
}

func (f Field) Editable() bool {
	if !apiName.MatchString(f.Name) || (f.IsActive != nil && !*f.IsActive) || f.IsUIReadOnly || (f.IsUIEditable != nil && !*f.IsUIEditable) || f.Writability == "READ_ONLY" {
		return false
	}
	switch f.Name {
	case "id", "createdAt", "updatedAt", "deletedAt", "createdBy", "updatedBy", "position":
		return false
	}
	return true
}
func (f Field) Required() bool {
	return f.IsNullable != nil && !*f.IsNullable && (len(f.DefaultValue) == 0 || string(f.DefaultValue) == "null")
}
func (f Field) Supported() bool {
	if f.Type == "RELATION" {
		return f.Settings.RelationType == "MANY_TO_ONE" && apiName.MatchString(f.Settings.JoinColumnName)
	}
	switch f.Type {
	case "TEXT", "RICH_TEXT", "SELECT", "MULTI_SELECT", "BOOLEAN", "NUMBER", "NUMERIC", "DATE", "DATE_TIME", "UUID":
		return true
	}
	return false
}

type Object struct {
	ID            string  `json:"id"`
	NameSingular  string  `json:"nameSingular"`
	NamePlural    string  `json:"namePlural"`
	LabelSingular string  `json:"labelSingular"`
	IsActive      *bool   `json:"isActive"`
	Fields        []Field `json:"fields"`
}

func (o Object) Valid() bool {
	return o.ID != "" && apiName.MatchString(o.NameSingular) && apiName.MatchString(o.NamePlural) && (o.IsActive == nil || *o.IsActive)
}

// Objects supports both the legacy {data:{objects:[]}} and newer {data:[]}
// REST metadata envelopes. All pages are fetched before replacing cached schema.
func (c *Client) Objects(ctx context.Context) ([]Object, error) {
	var objects []Object
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		path := "/rest/metadata/objects?limit=100"
		if cursor != "" {
			path += "&starting_after=" + url.QueryEscape(cursor)
		}
		var response struct {
			Data     json.RawMessage `json:"data"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		if err := c.api.DoJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		var batch []Object
		if len(response.Data) == 0 || string(response.Data) == "null" {
			return nil, fmt.Errorf("missing metadata data")
		}
		if response.Data[0] == '[' {
			if err := json.Unmarshal(response.Data, &batch); err != nil {
				return nil, fmt.Errorf("invalid object metadata")
			}
		} else {
			var legacy struct {
				Objects *[]Object `json:"objects"`
			}
			if err := json.Unmarshal(response.Data, &legacy); err != nil || legacy.Objects == nil {
				return nil, fmt.Errorf("unrecognized metadata response")
			}
			batch = *legacy.Objects
		}
		for _, object := range batch {
			if object.Valid() {
				objects = append(objects, object)
			}
		}
		if !response.PageInfo.HasNextPage {
			return objects, nil
		}
		cursor = response.PageInfo.EndCursor
		if cursor == "" || seen[cursor] {
			return nil, fmt.Errorf("invalid metadata pagination cursor")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("metadata exceeds 100 pages")
}

// EnsureRecord uses an immutable, caller-supplied identity and never updates a
// record during recovery. Field mappings are validated before reaching here.
func (c *Client) EnsureRecord(ctx context.Context, singular, plural, id string, fields map[string]any) (string, error) {
	return c.EnsureRecordTracked(ctx, singular, plural, id, fields, nil)
}

// EnsureRecordTracked reports lookup/creation outcomes without exposing the
// request payload or credentials. Progress reporting does not control delivery.
func (c *Client) EnsureRecordTracked(ctx context.Context, singular, plural, id string, fields map[string]any, progress func(string, string, string)) (string, error) {
	emit := func(stage, status, detail string) {
		if progress != nil {
			progress(stage, status, detail)
		}
	}
	if !apiName.MatchString(singular) || !apiName.MatchString(plural) {
		return "", fmt.Errorf("invalid object API names")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid record ID")
	}
	var found struct {
		Data map[string]ticketRecord `json:"data"`
	}
	emit("twenty_lookup", "running", "Checking for an existing record with the stable delivery ID.")
	err := c.REST(ctx, http.MethodGet, "/"+plural+"/"+id+"?depth=0", nil, &found)
	if err == nil {
		if err := found.Data[singular].validate(id); err != nil {
			emit("twenty_lookup", "failure", err.Error())
			return "", err
		}
		emit("twenty_lookup", "success", "Found existing record "+id)
		emit("twenty_create", "skipped", "Recovered existing record; no duplicate created.")
		return id, nil
	}
	var status *api.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		emit("twenty_lookup", "failure", err.Error())
		return "", err
	}
	emit("twenty_lookup", "success", "No existing record found; creation is needed.")
	payload := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		if !apiName.MatchString(key) || key == "id" {
			return "", fmt.Errorf("invalid mapped field")
		}
		payload[key] = value
	}
	payload["id"] = id
	var created struct {
		Data map[string]ticketRecord `json:"data"`
	}
	emit("twenty_create", "running", "Creating a record in "+plural+".")
	if err := c.REST(ctx, http.MethodPost, "/"+plural+"?depth=0", payload, &created); err != nil {
		emit("twenty_create", "failure", err.Error())
		return "", err
	}
	key := "create" + strings.ToUpper(singular[:1]) + singular[1:]
	if err := created.Data[key].validate(id); err != nil {
		emit("twenty_create", "failure", err.Error())
		return "", err
	}
	emit("twenty_create", "success", "Created record "+id)
	return id, nil
}
