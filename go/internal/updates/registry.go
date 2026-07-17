package updates

import "sync"

// Registry lazily creates and caches one *Store per track name. Tracks are
// not pre-declared anywhere — the first request (GET or publish) for a
// track name creates its Store on demand. The registry exists so that
// concurrent publishes to the same track keep serializing through the same
// *Store's internal mutex and in-memory active-release cache; read-only
// handlers (GET manifest / GET build file) don't need it and can construct
// a transient *Store directly, since Store.Get/ReadBuild don't take the
// lock (see store.go).
type Registry struct {
	root string
	mu   sync.Mutex
	// stores caches one *Store per track, created on first Get.
	stores map[string]*Store
}

// NewRegistry builds a Registry rooted at root (the configured updates
// storage directory). It does not touch the filesystem until Get is called
// for a specific track.
func NewRegistry(root string) *Registry {
	return &Registry{root: root, stores: map[string]*Store{}}
}

// Root returns the configured storage root.
func (r *Registry) Root() string { return r.root }

// Get returns the cached *Store for track, creating and Ensure()-ing it on
// first use. Safe for concurrent use; the same *Store instance is always
// returned for a given track name so its internal mutex correctly
// serializes concurrent publishes.
func (r *Registry) Get(track string) (*Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stores[track]; ok {
		return s, nil
	}
	s := NewStore(r.root, track)
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	r.stores[track] = s
	return s, nil
}
