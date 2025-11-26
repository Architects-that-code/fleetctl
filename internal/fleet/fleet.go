// internal/fleet/fleet.go
package fleet

import (
	"fmt"
	"log"

	"fleetctl/internal/client"
	"fleetctl/internal/config"
	"fleetctl/internal/state"
)

// Fleet holds the current state and configuration of the fleet
type Fleet struct {
	Config config.FleetConfig
	Client *client.Client
	Store  *state.Store
}

// New creates a new Fleet instance
func New(cfg config.FleetConfig, c *client.Client, s *state.Store) *Fleet {
	return &Fleet{
		Config: cfg,
		Client: c,
		Store:  s,
	}
}

// Scale scales the fleet to the desired total number of instances (stub)
func (f *Fleet) Scale(desiredTotal int) error {
	if desiredTotal < 0 {
		return fmt.Errorf("desiredTotal must be >= 0")
	}
	fleetName := f.Config.Metadata.Name

	current, err := f.Store.CountActive(fleetName)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	if desiredTotal == current {
		log.Printf("Scale: desired=%d equals current=%d; no changes", desiredTotal, current)
		return nil
	}

	if desiredTotal > current {
		missing := desiredTotal - current
		group := "default"
		if len(f.Config.Spec.Instances) > 0 && f.Config.Spec.Instances[0].Name != "" {
			group = f.Config.Spec.Instances[0].Name
		}
		if err := f.Store.AddActiveInstances(fleetName, group, missing); err != nil {
			return fmt.Errorf("adding %d instances: %w", missing, err)
		}
		log.Printf("Scale: added %d instances to reach %d", missing, desiredTotal)
		return nil
	}

	// desired < current
	toRemove := current - desiredTotal
	removed, err := f.Store.RemoveActiveInstances(fleetName, toRemove)
	if err != nil {
		return fmt.Errorf("removing %d instances: %w", toRemove, err)
	}
	log.Printf("Scale: removed %d instances to reach %d", removed, desiredTotal)
	return nil
}

// RollingRestart performs a rolling restart on the fleet (stub)
func (f *Fleet) RollingRestart() error {
	log.Printf("Rolling restart requested (stubbed)")
	// TODO: implement rolling restart logic using f.Client
	return nil
}

// Summary returns a simple string describing the loaded config
func (f *Fleet) Summary() string {
	return fmt.Sprintf("Fleet(kind=%s, name=%s, instances=%d)",
		f.Config.Kind, f.Config.Metadata.Name, len(f.Config.Spec.Instances))
}
