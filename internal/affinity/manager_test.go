package affinity

import (
	"testing"
	"time"

	"any-load/internal/store"
)

func TestBindingSetGetDelete(t *testing.T) {
	m := NewManager(store.NewMemoryStore())
	const gid uint = 1
	const key = "hdr:s1"

	// No binding initially.
	b, err := m.GetBinding(gid, key)
	if err != nil || b != nil {
		t.Fatalf("GetBinding on empty: b=%v err=%v", b, err)
	}

	// Store a binding.
	want := &Binding{GroupID: gid, KeyID: 7, UpstreamIdx: 1, BaseURL: "https://up.example.com", SubGroup: "child"}
	if err := m.SetBinding(gid, key, want, time.Hour); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}

	// Read it back.
	got, err := m.GetBinding(gid, key)
	if err != nil || got == nil {
		t.Fatalf("GetBinding after set: got=%v err=%v", got, err)
	}
	if *got != *want {
		t.Fatalf("binding round-trip mismatch: got=%+v want=%+v", *got, *want)
	}

	// Delete and confirm gone.
	if err := m.DeleteBinding(gid, key); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	b, err = m.GetBinding(gid, key)
	if err != nil || b != nil {
		t.Fatalf("GetBinding after delete: b=%v err=%v", b, err)
	}
}

func TestBindingTTLExpiry(t *testing.T) {
	m := NewManager(store.NewMemoryStore())
	const gid uint = 2
	const key = "body:k1"
	want := &Binding{GroupID: gid, KeyID: 3, UpstreamIdx: 0, BaseURL: "https://up.example.com"}

	if err := m.SetBinding(gid, key, want, 50*time.Millisecond); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	if b, _ := m.GetBinding(gid, key); b == nil {
		t.Fatal("binding should exist before TTL")
	}
	time.Sleep(80 * time.Millisecond)
	if b, _ := m.GetBinding(gid, key); b != nil {
		t.Fatal("binding should be expired after TTL")
	}
}
