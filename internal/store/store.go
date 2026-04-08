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
	SetFrozen(containerID string, frozen bool) error
	IsFrozen(containerID string) (bool, error)

	// Scope-aware limits (replaces old Global* methods)
	SetScopeLimit(scope model.Scope, limitType model.LimitType, value int64) error
	GetScopeLimit(scope model.Scope, limitType model.LimitType) (int64, error)
	GetAllScopeLimits(scope model.Scope) (map[model.LimitType]int64, error)
	GetScopeUsageAccum(scope model.Scope) (map[model.LimitType]int64, error)

	// Segment management
	CreateSegment(id, name string) (*model.Segment, error)
	GetSegment(id string) (*model.Segment, error)
	ListSegments() ([]model.Segment, error)
	DeleteSegment(id string) error
	SetContainerSegment(containerID string, segmentID *string) error
	ListContainersByScope(scope model.Scope) ([]model.Container, error)
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
		// Keep global_limits for backward compat during migration
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

	// Add frozen column (ignore error if already exists)
	s.db.Exec(`ALTER TABLE containers ADD COLUMN frozen INTEGER NOT NULL DEFAULT 0`)

	// Add segment_id column (ignore error if already exists)
	s.db.Exec(`ALTER TABLE containers ADD COLUMN segment_id TEXT DEFAULT NULL`)

	// Global usage accumulator (legacy, kept for migration)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS global_usage_accum (
		type TEXT PRIMARY KEY,
		value INTEGER NOT NULL DEFAULT 0
	)`)

	// Scope-aware limits table (replaces global_limits)
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS scope_limits (
		scope TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL,
		value INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (scope, type)
	)`); err != nil {
		return fmt.Errorf("create scope_limits: %w", err)
	}

	// Scope-aware usage accumulator (replaces global_usage_accum)
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS scope_usage_accum (
		scope TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL,
		value INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (scope, type)
	)`); err != nil {
		return fmt.Errorf("create scope_usage_accum: %w", err)
	}

	// Migrate data from old tables to new scope-aware tables (host scope = "")
	s.db.Exec(`INSERT OR IGNORE INTO scope_limits (scope, type, value) SELECT '', type, value FROM global_limits`)
	s.db.Exec(`INSERT OR IGNORE INTO scope_usage_accum (scope, type, value) SELECT '', type, value FROM global_usage_accum`)

	// Segments table
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS segments (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create segments: %w", err)
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
	row := s.db.QueryRow(`SELECT id, docker_id, name, registered_at, COALESCE(segment_id, '') FROM containers WHERE id = ?`, id)
	c := &model.Container{}
	var regAt string
	if err := row.Scan(&c.ID, &c.DockerID, &c.Name, &regAt, &c.SegmentID); err != nil {
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
	rows, err := s.db.Query(`SELECT id, docker_id, name, registered_at, COALESCE(segment_id, '') FROM containers ORDER BY registered_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []model.Container
	for rows.Next() {
		var c model.Container
		var regAt string
		if err := rows.Scan(&c.ID, &c.DockerID, &c.Name, &regAt, &c.SegmentID); err != nil {
			return nil, err
		}
		c.RegisteredAt, _ = time.Parse(time.RFC3339, regAt)
		result = append(result, c)
	}
	return result, rows.Err()
}

// RemoveContainer removes a container and its limits/usage from the store.
// Cumulative usage is accumulated into the host scope (and segment scope if applicable)
// so that removing a container doesn't reduce totals for irreversible resources.
func (s *Store) RemoveContainer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Look up segment before deleting
	var segmentID sql.NullString
	tx.QueryRow(`SELECT segment_id FROM containers WHERE id = ?`, id).Scan(&segmentID)

	// Accumulate cumulative usage types into scope accumulators before deleting
	for _, lt := range model.CumulativeLimitTypes {
		row := tx.QueryRow(`SELECT value FROM usage WHERE container_id = ? AND type = ?`, id, string(lt))
		var val int64
		if err := row.Scan(&val); err == nil && val > 0 {
			// Always accumulate into host scope
			tx.Exec(`INSERT INTO scope_usage_accum (scope, type, value) VALUES ('', ?, ?)
				ON CONFLICT(scope, type) DO UPDATE SET value = value + ?`, string(lt), val, val)
			// Keep legacy table in sync
			tx.Exec(`INSERT INTO global_usage_accum (type, value) VALUES (?, ?)
				ON CONFLICT(type) DO UPDATE SET value = value + ?`, string(lt), val, val)
			// Also accumulate into segment scope if container was in a segment
			if segmentID.Valid && segmentID.String != "" {
				scope := string(model.SegmentScope(segmentID.String))
				tx.Exec(`INSERT INTO scope_usage_accum (scope, type, value) VALUES (?, ?, ?)
					ON CONFLICT(scope, type) DO UPDATE SET value = value + ?`, scope, string(lt), val, val)
			}
		}
	}

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

// SetScopeLimit sets or updates a scope-level limit.
func (s *Store) SetScopeLimit(scope model.Scope, limitType model.LimitType, value int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO scope_limits (scope, type, value) VALUES (?, ?, ?)`,
		string(scope), string(limitType), value,
	)
	if err != nil {
		return err
	}
	// Keep global_limits in sync for backward compatibility (host scope only)
	if scope == model.ScopeHost {
		s.db.Exec(`INSERT OR REPLACE INTO global_limits (type, value) VALUES (?, ?)`,
			string(limitType), value)
	}
	return nil
}

// GetScopeLimit retrieves a scope-level limit. Returns 0 if not set.
func (s *Store) GetScopeLimit(scope model.Scope, limitType model.LimitType) (int64, error) {
	row := s.db.QueryRow(
		`SELECT value FROM scope_limits WHERE scope = ? AND type = ?`,
		string(scope), string(limitType),
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

// GetAllScopeLimits returns all limits for a scope.
func (s *Store) GetAllScopeLimits(scope model.Scope) (map[model.LimitType]int64, error) {
	rows, err := s.db.Query(`SELECT type, value FROM scope_limits WHERE scope = ?`, string(scope))
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

// GetScopeUsageAccum returns accumulated usage from removed containers for a scope.
func (s *Store) GetScopeUsageAccum(scope model.Scope) (map[model.LimitType]int64, error) {
	rows, err := s.db.Query(`SELECT type, value FROM scope_usage_accum WHERE scope = ?`, string(scope))
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

// --- Segment management ---

// CreateSegment creates a new segment.
func (s *Store) CreateSegment(id, name string) (*model.Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seg := &model.Segment{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO segments (id, name, created_at) VALUES (?, ?, ?)`,
		seg.ID, seg.Name, seg.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("create segment: %w", err)
	}
	return seg, nil
}

// GetSegment retrieves a segment by ID.
func (s *Store) GetSegment(id string) (*model.Segment, error) {
	row := s.db.QueryRow(`SELECT id, name, created_at FROM segments WHERE id = ?`, id)
	seg := &model.Segment{}
	var createdAt string
	if err := row.Scan(&seg.ID, &seg.Name, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("segment %q not found", id)
		}
		return nil, err
	}
	seg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return seg, nil
}

// ListSegments returns all segments.
func (s *Store) ListSegments() ([]model.Segment, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM segments ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []model.Segment
	for rows.Next() {
		var seg model.Segment
		var createdAt string
		if err := rows.Scan(&seg.ID, &seg.Name, &createdAt); err != nil {
			return nil, err
		}
		seg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		result = append(result, seg)
	}
	return result, rows.Err()
}

// DeleteSegment removes a segment. Fails if any containers are assigned to it.
func (s *Store) DeleteSegment(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM containers WHERE segment_id = ?`, id).Scan(&count)
	if count > 0 {
		return fmt.Errorf("segment %q has %d containers; remove them first", id, count)
	}

	// Clean up scope data
	scope := string(model.SegmentScope(id))
	s.db.Exec(`DELETE FROM scope_limits WHERE scope = ?`, scope)
	s.db.Exec(`DELETE FROM scope_usage_accum WHERE scope = ?`, scope)
	_, err := s.db.Exec(`DELETE FROM segments WHERE id = ?`, id)
	return err
}

// SetContainerSegment assigns a container to a segment (or removes from segment if nil).
func (s *Store) SetContainerSegment(containerID string, segmentID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`UPDATE containers SET segment_id = ? WHERE id = ?`, segmentID, containerID)
	return err
}

// ListContainersByScope returns containers for a scope.
// ScopeHost returns ALL containers (host sees everything).
// SegmentScope returns only containers assigned to that segment.
func (s *Store) ListContainersByScope(scope model.Scope) ([]model.Container, error) {
	var rows *sql.Rows
	var err error

	if scope == model.ScopeHost {
		rows, err = s.db.Query(`SELECT id, docker_id, name, registered_at, COALESCE(segment_id, '') FROM containers ORDER BY registered_at`)
	} else {
		segID := scope.SegmentID()
		rows, err = s.db.Query(`SELECT id, docker_id, name, registered_at, COALESCE(segment_id, '') FROM containers WHERE segment_id = ? ORDER BY registered_at`, segID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []model.Container
	for rows.Next() {
		var c model.Container
		var regAt string
		if err := rows.Scan(&c.ID, &c.DockerID, &c.Name, &regAt, &c.SegmentID); err != nil {
			return nil, err
		}
		c.RegisteredAt, _ = time.Parse(time.RFC3339, regAt)
		result = append(result, c)
	}
	return result, rows.Err()
}

// SetFrozen sets the frozen state for a container.
func (s *Store) SetFrozen(containerID string, frozen bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	val := 0
	if frozen {
		val = 1
	}
	_, err := s.db.Exec(`UPDATE containers SET frozen = ? WHERE id = ?`, val, containerID)
	return err
}

// IsFrozen returns whether a container is frozen.
func (s *Store) IsFrozen(containerID string) (bool, error) {
	row := s.db.QueryRow(`SELECT frozen FROM containers WHERE id = ?`, containerID)
	var val int
	if err := row.Scan(&val); err != nil {
		return false, err
	}
	return val != 0, nil
}
