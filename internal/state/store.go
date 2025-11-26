// internal/state/store.go
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	StatusActive     = "Active"
	StatusTerminated = "Terminated"
)

// InstanceRecord represents a single tracked instance under our control.
type InstanceRecord struct {
	ID        string    `json:"id"`
	Group     string    `json:"group"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// FleetState captures tracked instances for a named fleet.
type FleetState struct {
	FleetName string           `json:"fleetName"`
	Instances []InstanceRecord `json:"instances"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// root is the top-level JSON format to allow multiple fleets in one file.
type root struct {
	Fleets map[string]FleetState `json:"fleets"`
}

// Store persists tracking state to a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
}

// New creates a new state Store at the provided path.
func New(path string) *Store {
	return &Store{path: path}
}

// ensureDir ensures the directory for the state file exists.
func (s *Store) ensureDir() error {
	dir := filepath.Dir(s.path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// load reads the state file, returning an initialized root if missing.
func (s *Store) load() (*root, error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &root{Fleets: map[string]FleetState{}}, nil
		}
		return nil, fmt.Errorf("reading state file %q: %w", s.path, err)
	}

	var r root
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing state file %q: %w", s.path, err)
	}
	if r.Fleets == nil {
		r.Fleets = map[string]FleetState{}
	}
	return &r, nil
}

// save writes the state back to disk atomically.
func (s *Store) save(r *root) error {
	if err := s.ensureDir(); err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp state file: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// AddActiveInstances appends n active instances for fleetName and group.
func (s *Store) AddActiveInstances(fleetName, group string, n int) error {
	if n <= 0 {
		return nil
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.load()
	if err != nil {
		return err
	}
	fs := r.Fleets[fleetName]
	fs.FleetName = fleetName

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("local-%d-%d", now.UnixNano(), i)
		name := fmt.Sprintf("%s-%d-%d", group, now.UnixNano(), i)
		rec := InstanceRecord{
			ID:        id,
			Group:     group,
			Name:      name,
			Status:    StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		fs.Instances = append(fs.Instances, rec)
	}

	fs.UpdatedAt = now
	r.Fleets[fleetName] = fs
	return s.save(r)
}

// RemoveActiveInstances marks up to n active instances as terminated (LIFO).
func (s *Store) RemoveActiveInstances(fleetName string, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.load()
	if err != nil {
		return 0, err
	}
	fs := r.Fleets[fleetName]

	// Find indexes of active instances
	activeIdx := make([]int, 0, len(fs.Instances))
	for idx, inst := range fs.Instances {
		if inst.Status == StatusActive {
			activeIdx = append(activeIdx, idx)
		}
	}
	// Remove from the end for deterministic behavior
	sort.Ints(activeIdx)

	removed := 0
	for i := len(activeIdx) - 1; i >= 0 && removed < n; i-- {
		idx := activeIdx[i]
		fs.Instances[idx].Status = StatusTerminated
		fs.Instances[idx].UpdatedAt = now
		removed++
	}

	fs.UpdatedAt = now
	r.Fleets[fleetName] = fs
	return removed, s.save(r)
}

// CountActive returns the number of active instances tracked for the fleet.
func (s *Store) CountActive(fleetName string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.load()
	if err != nil {
		return 0, err
	}
	fs := r.Fleets[fleetName]
	active := 0
	for _, inst := range fs.Instances {
		if inst.Status == StatusActive {
			active++
		}
	}
	return active, nil
}

// Summary returns a human-readable summary for the fleet.
func (s *Store) Summary(fleetName string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.load()
	if err != nil {
		return "", err
	}
	fs := r.Fleets[fleetName]
	byGroup := map[string]int{}
	active := 0
	total := len(fs.Instances)
	for _, inst := range fs.Instances {
		if inst.Status == StatusActive {
			active++
			byGroup[inst.Group]++
		}
	}

	type kv struct {
		Group string
		Count int
	}
	var groups []kv
	for g, c := range byGroup {
		groups = append(groups, kv{Group: g, Count: c})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Group < groups[j].Group })

	out := fmt.Sprintf("Tracked state for fleet %q: active=%d total=%d updated=%s",
		fleetName, active, total, fs.UpdatedAt.Format(time.RFC3339))
	if len(groups) > 0 {
		out += "\nGroups:"
		for _, g := range groups {
			out += fmt.Sprintf("\n  - %s: %d", g.Group, g.Count)
		}
	}
	return out, nil
}
