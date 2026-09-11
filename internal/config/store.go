// Package config stores admin-managed routing configuration in SQLite. API
// keys are encrypted with an installation key kept on the persistent volume.
package config

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
)

type Connection struct {
	ID       string
	Name     string
	URL      string
	Objects  []twenty.Object
	SyncedAt string
}
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "config.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		// Never silently generate a replacement key for an existing database.
		if _, statErr := os.Stat(filepath.Join(dir, "config.db")); statErr == nil {
			return nil, fmt.Errorf("config.key missing for existing config.db; restore it from backup")
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid config.key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(dir, "config.db"))
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
	CREATE TABLE IF NOT EXISTS connections (id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL, secret BLOB NOT NULL, objects BLOB NOT NULL, synced_at TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS routes (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), definition BLOB NOT NULL);
	CREATE TABLE IF NOT EXISTS activity_emails (email_id TEXT PRIMARY KEY, subject TEXT NOT NULL, sender TEXT NOT NULL, recipients TEXT NOT NULL, first_seen INTEGER NOT NULL, updated_at INTEGER NOT NULL, last_event INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS activity_events (id INTEGER PRIMARY KEY AUTOINCREMENT, email_id TEXT NOT NULL REFERENCES activity_emails(email_id), occurred_at INTEGER NOT NULL, attempt_id TEXT NOT NULL, webhook_id TEXT NOT NULL, route_id TEXT NOT NULL, route_name TEXT NOT NULL, stage TEXT NOT NULL, status TEXT NOT NULL, detail TEXT NOT NULL);
	CREATE INDEX IF NOT EXISTS activity_email_order ON activity_emails(last_event DESC);
	CREATE INDEX IF NOT EXISTS activity_event_email ON activity_events(email_id,id DESC);
	CREATE INDEX IF NOT EXISTS activity_event_scope ON activity_events(email_id,route_id,id DESC);
	PRAGMA user_version=2;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Connections() ([]Connection, error) {
	rows, err := s.db.Query("SELECT id,name,url,objects,synced_at FROM connections ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		var c Connection
		var raw []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.URL, &raw, &c.SyncedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &c.Objects); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) Connection(id string) (Connection, error) {
	var c Connection
	var raw []byte
	err := s.db.QueryRow("SELECT id,name,url,objects,synced_at FROM connections WHERE id=?", id).Scan(&c.ID, &c.Name, &c.URL, &raw, &c.SyncedAt)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c.Objects)
	return c, err
}
func (s *Store) Client(id string) (*twenty.Client, error) {
	var base string
	var secret []byte
	if err := s.db.QueryRow("SELECT url,secret FROM connections WHERE id=?", id).Scan(&base, &secret); err != nil {
		return nil, err
	}
	if len(secret) < s.aead.NonceSize() {
		return nil, fmt.Errorf("invalid encrypted credential")
	}
	plain, err := s.aead.Open(nil, secret[:s.aead.NonceSize()], secret[s.aead.NonceSize():], []byte(id))
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt connection credential")
	}
	return twenty.New(base, string(plain))
}

// SaveConnection tests the credentials and fetches the complete schema before
// committing. URLs are immutable; changing destination means a new connection.
func (s *Store) SaveConnection(ctx context.Context, id, name, base, key string) (string, error) {
	name, base, key = strings.TrimSpace(name), strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(key)
	if name == "" {
		return "", fmt.Errorf("connection name is required")
	}
	var client *twenty.Client
	var err error
	if id != "" {
		old, err := s.Connection(id)
		if err != nil {
			return "", err
		}
		if base != old.URL {
			return "", fmt.Errorf("connection URL is immutable; add a new connection for a different instance")
		}
		if key == "" {
			client, err = s.Client(id)
			if err != nil {
				return "", err
			}
		}
	} else {
		id = uuid.NewString()
		if key == "" {
			return "", fmt.Errorf("API key is required")
		}
	}
	if client == nil {
		client, err = twenty.New(base, key)
		if err != nil {
			return "", err
		}
	}
	objects, err := client.Objects(ctx)
	if err != nil {
		return "", fmt.Errorf("test connection / retrieve schema: %w", err)
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	if key == "" {
		_, err = s.db.Exec("UPDATE connections SET name=?,objects=?,synced_at=? WHERE id=?", name, raw, stamp, id)
		return id, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	secret := s.aead.Seal(nonce, nonce, []byte(key), []byte(id))
	_, err = s.db.Exec(`INSERT INTO connections(id,name,url,secret,objects,synced_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,secret=excluded.secret,objects=excluded.objects,synced_at=excluded.synced_at`, id, name, base, secret, raw, stamp)
	return id, err
}
func (s *Store) Routes() ([]routing.Route, error) {
	rows, err := s.db.Query("SELECT definition FROM routes ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []routing.Route
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var route routing.Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}
func (s *Store) Route(id string) (routing.Route, error) {
	var raw []byte
	var route routing.Route
	if err := s.db.QueryRow("SELECT definition FROM routes WHERE id=?", id).Scan(&raw); err != nil {
		return route, err
	}
	err := json.Unmarshal(raw, &route)
	return route, err
}
func (s *Store) SaveRoute(route routing.Route) error {
	c, err := s.Connection(route.ConnectionID)
	if err != nil {
		return err
	}
	var object twenty.Object
	for _, o := range c.Objects {
		if o.ID == route.ObjectID {
			object = o
			break
		}
	}
	if err := routing.Validate(&route, object); err != nil {
		return err
	}
	if route.ID == "" {
		route.ID = uuid.NewString()
	} else {
		if _, err := s.Route(route.ID); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(route)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO routes(id,connection_id,definition) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET connection_id=excluded.connection_id,definition=excluded.definition`, route.ID, route.ConnectionID, raw)
	return err
}
func (s *Store) Match(to []string) ([]routing.Destination, error) {
	routes, err := s.Routes()
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no inbound routes configured; finish setup in /admin/")
	}
	var destinations []routing.Destination
	for _, r := range routes {
		if r.Matches(to) {
			destinations = append(destinations, r.Snapshot())
		}
	}
	return destinations, nil
}
