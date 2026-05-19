package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")
)

type AdminSettings struct {
	Username     string
	PasswordHash string
}

type MonitoredGroup struct {
	ChatID    int64
	AddedAt   time.Time
}

type Store struct {
	mu sync.RWMutex
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=10000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS admin_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL DEFAULT 'admin',
			password_hash TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS monitored_groups (
			chat_id INTEGER PRIMARY KEY,
			added_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
	`)
	return err
}

func (s *Store) InitAdmin(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM admin_settings").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	_, err = s.db.Exec(
		"INSERT INTO admin_settings (id, username, password_hash) VALUES (1, ?, ?)",
		username, string(hash),
	)
	return err
}

func (s *Store) GetAdmin() (*AdminSettings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a := &AdminSettings{}
	err := s.db.QueryRow("SELECT username, password_hash FROM admin_settings WHERE id = 1").Scan(&a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Store) UpdateAdminPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	_, err = s.db.Exec("UPDATE admin_settings SET password_hash = ? WHERE id = 1", string(hash))
	return err
}

func (s *Store) VerifyAdminPassword(password string) (bool, error) {
	admin, err := s.GetAdmin()
	if err != nil {
		return false, err
	}
	err = bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) AddGroup(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("INSERT OR IGNORE INTO monitored_groups (chat_id) VALUES (?)", chatID)
	return err
}

func (s *Store) RemoveGroup(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM monitored_groups WHERE chat_id = ?", chatID)
	return err
}

func (s *Store) ListGroups() ([]MonitoredGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT chat_id, added_at FROM monitored_groups ORDER BY added_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []MonitoredGroup
	for rows.Next() {
		var g MonitoredGroup
		if err := rows.Scan(&g.ChatID, &g.AddedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *Store) IsMonitored(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM monitored_groups WHERE chat_id = ?", chatID).Scan(&count)
	return count > 0
}

func (s *Store) GetMonitoredIDs() map[int64]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT chat_id FROM monitored_groups")
	if err != nil {
		return nil
	}
	defer rows.Close()

	m := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		m[id] = struct{}{}
	}
	return m
}
