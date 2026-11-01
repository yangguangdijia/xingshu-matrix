package tunnel

import (
	"context"
	"sync"
	"time"
)

// NewMemoryRegistry returns an in-process Registry for tests and
// single-replica development. Secrets stay in process memory only; it does
// not persist anything, so the encryption requirement does not apply.
func NewMemoryRegistry() Registry {
	return &memoryRegistry{owners: map[int64]Owner{}}
}

type memoryRegistry struct {
	mu     sync.Mutex
	owners map[int64]Owner
}

func (m *memoryRegistry) Claim(_ context.Context, clusterID int64, o Owner) error {
	if o.SeenAt.IsZero() {
		o.SeenAt = time.Now()
	}
	m.mu.Lock()
	m.owners[clusterID] = o
	m.mu.Unlock()
	return nil
}

func (m *memoryRegistry) Heartbeat(_ context.Context, clusterID int64, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.owners[clusterID]
	if !ok || o.SessionID != sessionID {
		return ErrSuperseded
	}
	o.SeenAt = time.Now()
	m.owners[clusterID] = o
	return nil
}

func (m *memoryRegistry) Release(_ context.Context, clusterID int64, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.owners[clusterID]; ok && o.SessionID == sessionID {
		delete(m.owners, clusterID)
	}
	return nil
}

func (m *memoryRegistry) Lookup(_ context.Context, clusterID int64) (Owner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.owners[clusterID]
	if !ok {
		return Owner{}, ErrOffline
	}
	return o, nil
}
