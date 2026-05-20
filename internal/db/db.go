package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spam-observer/internal/logstream"
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
	ChatID  int64     `json:"chat_id"`
	AddedAt time.Time `json:"added_at"`
}

type NewUserInfo struct {
	UserID      int64     `json:"user_id"`
	ChatID      int64     `json:"chat_id"`
	DisplayName string    `json:"display_name"`
	Username    string    `json:"username"`
	Bio         string    `json:"bio"`
	JoinedAt    time.Time `json:"joined_at"`
}

type VerificationBot struct {
	BotID    int64     `json:"bot_id"`
	Label    string    `json:"label"`
	AddedAt  time.Time `json:"added_at"`
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
			password_hash TEXT NOT NULL,
			bot_enabled INTEGER NOT NULL DEFAULT 1
		);

		CREATE TABLE IF NOT EXISTS monitored_groups (
			chat_id INTEGER PRIMARY KEY,
			added_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		);
	`)
	if err != nil {
		return err
	}

	_, _ = s.db.Exec("ALTER TABLE admin_settings ADD COLUMN bot_enabled INTEGER NOT NULL DEFAULT 1")
	_, _ = s.db.Exec("ALTER TABLE admin_settings ADD COLUMN bot_token TEXT NOT NULL DEFAULT ''")
	_, _ = s.db.Exec("ALTER TABLE admin_settings ADD COLUMN ai_base_url TEXT NOT NULL DEFAULT ''")
	_, _ = s.db.Exec("ALTER TABLE admin_settings ADD COLUMN ai_api_key TEXT NOT NULL DEFAULT ''")
	_, _ = s.db.Exec("ALTER TABLE admin_settings ADD COLUMN ai_model TEXT NOT NULL DEFAULT ''")

	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS new_users (
			user_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			bio TEXT NOT NULL DEFAULT '',
			joined_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			PRIMARY KEY (user_id, chat_id)
		);

		CREATE TABLE IF NOT EXISTS verification_bots (
			bot_id INTEGER PRIMARY KEY,
			label TEXT NOT NULL DEFAULT '',
			added_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		);
	`)
	if err != nil {
		return err
	}

	_, _ = s.db.Exec("UPDATE monitored_groups SET added_at = added_at || 'Z' WHERE added_at NOT LIKE '%Z'")
	_, _ = s.db.Exec("UPDATE new_users SET joined_at = joined_at || 'Z' WHERE joined_at NOT LIKE '%Z'")
	_, _ = s.db.Exec("UPDATE verification_bots SET added_at = added_at || 'Z' WHERE added_at NOT LIKE '%Z'")

	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS event_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			level TEXT NOT NULL,
			category TEXT NOT NULL,
			chat_id INTEGER NOT NULL DEFAULT 0,
			user_id INTEGER NOT NULL DEFAULT 0,
			username TEXT NOT NULL DEFAULT '',
			is_new INTEGER NOT NULL DEFAULT 0,
			mutual_groups INTEGER NOT NULL DEFAULT 0,
			message TEXT NOT NULL DEFAULT '',
			raw TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_event_logs_timestamp ON event_logs(timestamp);
	`)
	if err != nil {
		return err
	}

	return nil
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

func (s *Store) UpdateAdminUsername(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("UPDATE admin_settings SET username = ? WHERE id = 1", username)
	return err
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

func (s *Store) GetBotEnabled() (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var enabled int
	err := s.db.QueryRow("SELECT bot_enabled FROM admin_settings WHERE id = 1").Scan(&enabled)
	if err != nil {
		return true, err
	}
	return enabled == 1, nil
}

func (s *Store) SetBotEnabled(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec("UPDATE admin_settings SET bot_enabled = ? WHERE id = 1", v)
	return err
}

func (s *Store) GetBotToken() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var token string
	err := s.db.QueryRow("SELECT bot_token FROM admin_settings WHERE id = 1").Scan(&token)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) SetBotToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("UPDATE admin_settings SET bot_token = ? WHERE id = 1", token)
	return err
}

func (s *Store) HasBotToken() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var token string
	_ = s.db.QueryRow("SELECT bot_token FROM admin_settings WHERE id = 1").Scan(&token)
	return token != ""
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

func (s *Store) AddNewUser(userID, chatID int64, displayName, username, bio string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO new_users (user_id, chat_id, display_name, username, bio, joined_at) VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))",
		userID, chatID, displayName, username, bio,
	)
	return err
}

func (s *Store) GetNewUsers() ([]NewUserInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT user_id, chat_id, display_name, username, bio, joined_at FROM new_users WHERE joined_at >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-24 hours') ORDER BY joined_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []NewUserInfo
	for rows.Next() {
		var u NewUserInfo
		if err := rows.Scan(&u.UserID, &u.ChatID, &u.DisplayName, &u.Username, &u.Bio, &u.JoinedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) GetNewUserIDs() (map[int64]struct{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT DISTINCT user_id FROM new_users WHERE joined_at >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-24 hours')")
	if err != nil {
		return nil, err
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
	return m, rows.Err()
}

func (s *Store) PurgeExpiredNewUsers() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM new_users WHERE joined_at < strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-24 hours')")
	return err
}

func (s *Store) AddVerificationBot(botID int64, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("INSERT OR REPLACE INTO verification_bots (bot_id, label) VALUES (?, ?)", botID, label)
	return err
}

func (s *Store) RemoveVerificationBot(botID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM verification_bots WHERE bot_id = ?", botID)
	return err
}

func (s *Store) ListVerificationBots() ([]VerificationBot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT bot_id, label, added_at FROM verification_bots ORDER BY added_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []VerificationBot
	for rows.Next() {
		var b VerificationBot
		if err := rows.Scan(&b.BotID, &b.Label, &b.AddedAt); err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return bots, rows.Err()
}

func (s *Store) GetVerificationBotIDs() map[int64]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT bot_id FROM verification_bots")
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

type AIConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

func (s *Store) GetAIConfig() (*AIConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cfg := &AIConfig{}
	err := s.db.QueryRow("SELECT ai_base_url, ai_api_key, ai_model FROM admin_settings WHERE id = 1").Scan(&cfg.BaseURL, &cfg.APIKey, &cfg.Model)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *Store) SetAIConfig(cfg AIConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("UPDATE admin_settings SET ai_base_url = ?, ai_api_key = ?, ai_model = ? WHERE id = 1",
		cfg.BaseURL, cfg.APIKey, cfg.Model)
	return err
}

func (s *Store) GetAIConfigMasked() *AIConfig {
	cfg, err := s.GetAIConfig()
	if err != nil {
		return &AIConfig{}
	}
	masked := *cfg
	if len(masked.APIKey) > 8 {
		masked.APIKey = masked.APIKey[:4] + "..." + masked.APIKey[len(masked.APIKey)-4:]
	} else if masked.APIKey != "" {
		masked.APIKey = "***"
	}
	return &masked
}

func (s *Store) InsertLog(e logstream.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	isNew := 0
	if e.IsNew {
		isNew = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO event_logs (timestamp, level, category, chat_id, user_id, username, is_new, mutual_groups, message, raw)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.Level, e.Category, e.ChatID, e.UserID, e.Username,
		isNew, e.MutualGroups, e.Message, e.Raw,
	)
	return err
}

func (s *Store) GetRecentLogs(limit int) ([]logstream.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT timestamp, level, category, chat_id, user_id, username, is_new, mutual_groups, message, raw
		 FROM event_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []logstream.Entry
	for rows.Next() {
		var e logstream.Entry
		var ts string
		var isNew int
		if err := rows.Scan(&ts, &e.Level, &e.Category, &e.ChatID, &e.UserID, &e.Username,
			&isNew, &e.MutualGroups, &e.Message, &e.Raw); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		e.IsNew = isNew == 1
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

func (s *Store) PurgeOldLogs(olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec("DELETE FROM event_logs WHERE timestamp < ?", cutoff)
	return err
}

func (s *Store) GetRecentLogsByDuration(d time.Duration) ([]logstream.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-d).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.Query(
		`SELECT timestamp, level, category, chat_id, user_id, username, is_new, mutual_groups, message, raw
		 FROM event_logs WHERE timestamp >= ? ORDER BY id ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []logstream.Entry
	for rows.Next() {
		var e logstream.Entry
		var ts string
		var isNew int
		if err := rows.Scan(&ts, &e.Level, &e.Category, &e.ChatID, &e.UserID, &e.Username,
			&isNew, &e.MutualGroups, &e.Message, &e.Raw); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		e.IsNew = isNew == 1
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Store) GetBannedCount24h() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT user_id) FROM event_logs
		 WHERE category IN ('BAN', 'VERIFY_BAN') AND timestamp >= ?`, cutoff).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
