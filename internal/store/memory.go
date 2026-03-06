package store

import (
	"context"
	"sync"
)

type MemoryStore struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string]string)}
}

func (m *MemoryStore) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.items[key]
	return val, ok, nil
}

func (m *MemoryStore) Set(_ context.Context, key string, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = value
	return nil
}

func (m *MemoryStore) Close() error {
	return nil
}
