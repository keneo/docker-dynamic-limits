package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/keneo/docker-dynamic-limits/internal/model"
)

// DataStore defines the interface for persistent storage operations.
type DataStore interface {
	RegisterContainer(dockerID, name string) (*model.Container, error)
	GetContainer(id string) (*model.Container, error)
	ListContainers() ([]model.Container, error)
	RemoveContainer(id string) error
	SetLimit(containerID string, limitType model.LimitType, value int64) error
	GetLimit(containerID string, limitType model.LimitType) (int64, error)
	GetAllLimits(containerID string) (map[model.LimitType]int64, error)
	SetUsage(containerID string, limitType model.LimitType, value int64) error
	GetUsage(containerID string, limitType model.LimitType) (int64, error)
	GetAllUsage(containerID string) (map[model.LimitType]int64, error)
	AddUsage(containerID string, limitType model.LimitType, delta int64) error
	CopyLimits(fromID, toID string) error
	ResolveContainerID(query string) (string, error)
	SetGlobalLimit(limitType model.LimitType, value int64) error
	GetGlobalLimit(limitType model.LimitType) (int64, error)
	GetAllGlobalLimits() (map[model.LimitType]int64, error)
}

// Store provides persistent storage for container registrations, limits, and usage.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// New opens or creates a SQLite database at the given path.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS containers (
			id TEXT PRIMARY KEY,
			docker_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			registered_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS limits (
			container_id TEXT NOT NULL,
			type TEXT NOT NULL,
			value INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (container_id, type),
			FOREIGN KEY (container_id) REFERENCES containers(id)
		)`,
		`CREATE TABLE IF NOT EXISTS usage (
			container_id TEXT NOT NULL,
			type TEXT NOT NULL,
			value INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (container_id, type),
			FOREIGN KEY (container_id) REFERENCES containers(id)
		)`,
		`CREATE TABLE IF NOT EXISTS global_limits (
			type TEXT PRIMARY KEY,
			value INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:40], err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// RegisterContainer adds a container to the store.
func (s *Store) RegisterContainer(dockerID, name string) (*model.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := &model.Container{
		ID:           dockerID[:12],
		DockerID:     dockerID,
		Name:         name,
		RegisteredAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO containers (id, docker_id, name, registered_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.DockerID, c.Name, c.RegisteredAt.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("insert container: %w", err)
	}
	return c, nil
}

// GetContainer retrieves a container by its short ID.
func (s *Store) GetContainer(id string) (*model.Container, error) {
	row := s.db.QueryRow(`SELECT id, docker_id, name, registered_at FROM containers WHERE id = ?`, id)
	c := &model.Container{}
	var regAt string
	if err := row.Scan(&c.ID, &c.DockerID, &c.Name, &regAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("container %s not found", id)
		}
		return nil, err
	}
	c.RegisteredAt, _ = time.Parse(time.RFC3339, regAt)
	return c, nil
}

// ListContainers returns all managed containers.
func (s *Store) ListContainers() ([]model.Container, error) {
	rows, err := s.db.Query(`SELECT id, docker_id, name, registered_at FROM containers ORDER BY registered_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []model.Container
	for rows.Next() {
		var c model.Container
		var regAt string
		if err := rows.Scan(&c.ID, &c.DockerID, &c.Name, &regAt); err != nil {
			return nil, err
		}
		c.RegisteredAt, _ = time.Parse(time.RFC3339, regAt)
		result = append(result, c)
	}
	return result, rows.Err()
}

// RemoveContainer removes a container and its limits/usage from the store.
func (s *Store) RemoveContainer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tx.Exec(`DELETE FROM usage WHERE container_id = ?`, id)
	tx.Exec(`DELETE FROM limits WHERE container_id = ?`, id)
	tx.Exec(`DELETE FROM containers WHERE id = ?`, id)
	return tx.Commit()
}

// SetLimit sets or updates a limit for a container.
func (s *Store) SetLimit(containerID string, limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO limits (container_id, type, value) VALUES (?, ?, ?)`,
		containerID, string(limitType), value,
	)
	return err
}

// GetLimit retrieves the limit for a container and type. Returns 0 if not set.
func (s *Store) GetLimit(containerID string, limitType model.LimitType) (int64, error) {
	row := s.db.QueryRow(
		`SELECT value FROM limits WHERE container_id = ? AND type = ?`,
		containerID, string(limitType),
	)
	var val int64
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return val, nil
}

// GetAllLimits returns all limits for a container.
func (s *Store) GetAllLimits(containerID string) (map[model.LimitType]int64, error) {
	rows, err := s.db.Query(
		`SELECT type, value FROM limits WHERE container_id = ?`, containerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[model.LimitType]int64)
	for rows.Next() {
		var t string
		var v int64
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		result[model.LimitType(t)] = v
	}
	return result, rows.Err()
}

// SetUsage sets the usage counter for a container and type.
func (s *Store) SetUsage(containerID string, limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO usage (container_id, type, value) VALUES (?, ?, ?)`,
		containerID, string(limitType), value,
	)
	return err
}

// GetUsage retrieves usage for a container and type.
func (s *Store) GetUsage(containerID string, limitType model.LimitType) (int64, error) {
	row := s.db.QueryRow(
		`SELECT value FROM usage WHERE container_id = ? AND type = ?`,
		containerID, string(limitType),
	)
	var val int64
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return val, nil
}

// GetAllUsage returns all usage for a container.
func (s *Store) GetAllUsage(containerID string) (map[model.LimitType]int64, error) {
	rows, err := s.db.Query(
		`SELECT type, value FROM usage WHERE container_id = ?`, containerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[model.LimitType]int64)
	for rows.Next() {
		var t string
		var v int64
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		result[model.LimitType(t)] = v
	}
	return result, rows.Err()
}

// AddUsage atomically adds to the usage counter for a container and type.
func (s *Store) AddUsage(containerID string, limitType model.LimitType, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO usage (container_id, type, value) VALUES (?, ?, ?)
		 ON CONFLICT(container_id, type) DO UPDATE SET value = value + ?`,
		containerID, string(limitType), delta, delta,
	)
	return err
}

// CopyLimits copies all limits from one container to another.
func (s *Store) CopyLimits(fromID, toID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO limits (container_id, type, value)
		 SELECT ?, type, value FROM limits WHERE container_id = ?`,
		toID, fromID,
	)
	return err
}

// ResolveContainerID tries to find a container by short ID prefix or name.
func (s *Store) ResolveContainerID(query string) (string, error) {
	// Try exact match on ID first
	row := s.db.QueryRow(`SELECT id FROM containers WHERE id = ?`, query)
	var id string
	if err := row.Scan(&id); err == nil {
		return id, nil
	}

	// Try prefix match on docker_id
	row = s.db.QueryRow(`SELECT id FROM containers WHERE docker_id LIKE ? LIMIT 1`, query+"%")
	if err := row.Scan(&id); err == nil {
		return id, nil
	}

	// Try name match
	row = s.db.QueryRow(`SELECT id FROM containers WHERE name = ?`, query)
	if err := row.Scan(&id); err == nil {
		return id, nil
	}

	return "", fmt.Errorf("container %q not found", query)
}

// SetGlobalLimit sets or updates a global limit for a limit type.
func (s *Store) SetGlobalLimit(limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO global_limits (type, value) VALUES (?, ?)`,
		string(limitType), value,
	)
	return err
}

// GetGlobalLimit retrieves the global limit for a type. Returns 0 if not set.
func (s *Store) GetGlobalLimit(limitType model.LimitType) (int64, error) {
	row := s.db.QueryRow(
		`SELECT value FROM global_limits WHERE type = ?`,
		string(limitType),
	)
	var val int64
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return val, nil
}

// GetAllGlobalLimits returns all global limits.
func (s *Store) GetAllGlobalLimits() (map[model.LimitType]int64, error) {
	rows, err := s.db.Query(`SELECT type, value FROM global_limits`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[model.LimitType]int64)
	for rows.Next() {
		var t string
		var v int64
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		result[model.LimitType(t)] = v
	}
	return result, rows.Err()
}
