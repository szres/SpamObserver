package tracker

import (
	"log"
	"sync"
	"time"

	"github.com/spam-observer/internal/db"
)

type UserInfo struct {
	UserID      int64     `json:"user_id"`
	ChatID      int64     `json:"chat_id"`
	DisplayName string    `json:"display_name"`
	Username    string    `json:"username"`
	Bio         string    `json:"bio"`
	JoinedAt    time.Time `json:"joined_at"`
}

type Tracker struct {
	mu    sync.RWMutex
	store *db.Store
	users map[int64]*UserInfo
	stop  chan struct{}
}

func New(store *db.Store) *Tracker {
	t := &Tracker{
		store: store,
		users: make(map[int64]*UserInfo),
		stop:  make(chan struct{}),
	}
	t.loadFromDB()
	go t.cleanupLoop()
	return t
}

func (t *Tracker) loadFromDB() {
	users, err := t.store.GetNewUsers()
	if err != nil {
		log.Printf("tracker: failed to load new users from DB: %v", err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range users {
		t.users[u.UserID] = &UserInfo{
			UserID:      u.UserID,
			ChatID:      u.ChatID,
			DisplayName: u.DisplayName,
			Username:    u.Username,
			Bio:         u.Bio,
			JoinedAt:    u.JoinedAt,
		}
	}
}

func (t *Tracker) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.purgeExpired()
		case <-t.stop:
			return
		}
	}
}

func (t *Tracker) purgeExpired() {
	if err := t.store.PurgeExpiredNewUsers(); err != nil {
		log.Printf("tracker: failed to purge expired users: %v", err)
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, info := range t.users {
		if info.JoinedAt.Before(cutoff) {
			delete(t.users, id)
		}
	}
}

func (t *Tracker) MarkNew(userID, chatID int64, displayName, username, bio string) {
	now := time.Now()
	info := &UserInfo{
		UserID:      userID,
		ChatID:      chatID,
		DisplayName: displayName,
		Username:    username,
		Bio:         bio,
		JoinedAt:    now,
	}
	t.mu.Lock()
	t.users[userID] = info
	t.mu.Unlock()

	if err := t.store.AddNewUser(userID, chatID, displayName, username, bio); err != nil {
		log.Printf("tracker: failed to persist new user %d: %v", userID, err)
	}
}

func (t *Tracker) IsNew(userID int64) bool {
	t.mu.RLock()
	info, ok := t.users[userID]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Since(info.JoinedAt) > 24*time.Hour {
		return false
	}
	return true
}

func (t *Tracker) GetInfo(userID int64) *UserInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	info, ok := t.users[userID]
	if !ok {
		return nil
	}
	if time.Since(info.JoinedAt) > 24*time.Hour {
		return nil
	}
	return info
}

func (t *Tracker) GetAllNew() []UserInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	var result []UserInfo
	for _, info := range t.users {
		if info.JoinedAt.After(cutoff) {
			result = append(result, *info)
		}
	}
	return result
}

func (t *Tracker) Stop() {
	close(t.stop)
}
