package affinity

import (
	"encoding/json"
	"fmt"
	"time"

	"any-load/internal/store"
)

// Binding is the persisted (upstream, key) pair an affinity key is bound to.
// It is stored under the entry (original) group so the same client endpoint
// resolves to the same binding across requests.
type Binding struct {
	GroupID     uint   `json:"group_id"`      // entry group the binding is keyed under
	KeyID       uint   `json:"key_id"`        // bound API key id
	UpstreamIdx int    `json:"upstream_idx"`  // index into the group's Upstreams slice
	BaseURL     string `json:"base_url"`      // Upstreams[idx].URL.String(), for staleness validation
	SubGroup    string `json:"sub_group"`     // bound sub-group name (empty for standard groups)
}

// Manager stores and retrieves affinity bindings in the shared store (Redis in
// production, memory in single-instance mode). Bindings carry a TTL and are
// shared across instances, matching the leader-follower deployment model.
type Manager struct {
	store store.Store
}

// NewManager creates an affinity Manager backed by the given store.
func NewManager(s store.Store) *Manager {
	return &Manager{store: s}
}

// bindingKey returns the store key for a binding.
func bindingKey(groupID uint, affinityKey string) string {
	return fmt.Sprintf("affinity:%d:%s", groupID, affinityKey)
}

// GetBinding reads a binding. Returns (nil, nil) when no binding exists.
func (m *Manager) GetBinding(groupID uint, affinityKey string) (*Binding, error) {
	raw, err := m.store.Get(bindingKey(groupID, affinityKey))
	if err != nil {
		if err == store.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	var b Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// SetBinding writes (overwrites and renews) a binding with the given TTL.
func (m *Manager) SetBinding(groupID uint, affinityKey string, b *Binding, ttl time.Duration) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return m.store.Set(bindingKey(groupID, affinityKey), raw, ttl)
}

// DeleteBinding removes a binding (used when a bound key/upstream is invalid).
func (m *Manager) DeleteBinding(groupID uint, affinityKey string) error {
	return m.store.Delete(bindingKey(groupID, affinityKey))
}
