package logstream

import (
	"fmt"
	"sync"
	"time"
)

const (
	MaxRingSize  = 200
	MaxBroadcast = 64
)

type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Category  string    `json:"category"`
	ChatID    int64     `json:"chat_id,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	Username  string    `json:"username,omitempty"`
	IsNew        bool      `json:"is_new,omitempty"`
	MutualGroups int       `json:"mutual_groups,omitempty"`
	Message      string    `json:"message"`
	Raw       string    `json:"raw,omitempty"`
}

type Broker struct {
	mu         sync.RWMutex
	ring       []Entry
	head       int
	count      int
	clients    map[chan Entry]struct{}
	register   chan chan Entry
	unregister chan chan Entry
	publish    chan Entry
	stop       chan struct{}
	OnPublish  func(Entry)
}

func NewBroker() *Broker {
	b := &Broker{
		ring:       make([]Entry, MaxRingSize),
		clients:    make(map[chan Entry]struct{}),
		register:   make(chan chan Entry, 4),
		unregister: make(chan chan Entry, 4),
		publish:    make(chan Entry, MaxBroadcast),
		stop:       make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *Broker) run() {
	for {
		select {
		case ch := <-b.register:
			b.mu.Lock()
			b.clients[ch] = struct{}{}
			b.mu.Unlock()
		case ch := <-b.unregister:
			b.mu.Lock()
			delete(b.clients, ch)
			close(ch)
			b.mu.Unlock()
		case entry := <-b.publish:
			b.mu.Lock()
			b.ring[b.head] = entry
			b.head = (b.head + 1) % MaxRingSize
			if b.count < MaxRingSize {
				b.count++
			}
			for ch := range b.clients {
				select {
				case ch <- entry:
				default:
					delete(b.clients, ch)
					close(ch)
				}
			}
			onPub := b.OnPublish
			b.mu.Unlock()
			if onPub != nil {
				onPub(entry)
			}
		case <-b.stop:
			b.mu.Lock()
			for ch := range b.clients {
				close(ch)
			}
			b.clients = nil
			b.mu.Unlock()
			return
		}
	}
}

func (b *Broker) Publish(entry Entry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	select {
	case b.publish <- entry:
	default:
	}
}

func (b *Broker) Subscribe() chan Entry {
	ch := make(chan Entry, MaxBroadcast)
	b.register <- ch
	return ch
}

func (b *Broker) Unsubscribe(ch chan Entry) {
	b.unregister <- ch
}

func (b *Broker) Recent() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 {
		return nil
	}

	result := make([]Entry, 0, b.count)
	if b.count < MaxRingSize {
		for i := 0; i < b.count; i++ {
			result = append(result, b.ring[i])
		}
	} else {
		for i := 0; i < MaxRingSize; i++ {
			idx := (b.head + i) % MaxRingSize
			result = append(result, b.ring[idx])
		}
	}
	return result
}

func (b *Broker) Close() {
	close(b.stop)
}

func Info(category, msg string, fields ...interface{}) Entry {
	return Entry{
		Timestamp: time.Now(),
		Level:     "INFO",
		Category:  category,
		Message:   fmt.Sprintf(msg, fields...),
	}
}

func Warn(category, msg string, fields ...interface{}) Entry {
	return Entry{
		Timestamp: time.Now(),
		Level:     "WARN",
		Category:  category,
		Message:   fmt.Sprintf(msg, fields...),
	}
}

func Error(category, msg string, fields ...interface{}) Entry {
	return Entry{
		Timestamp: time.Now(),
		Level:     "ERROR",
		Category:  category,
		Message:   fmt.Sprintf(msg, fields...),
	}
}
