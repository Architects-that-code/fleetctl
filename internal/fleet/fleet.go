// internal/fleet/fleet.go
package fleet

import (
	"context"
	"fmt"
	"log"
	"sync"

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

// Scale scales the fleet to the desired total number of instances using OCI
func (f *Fleet) Scale(desiredTotal int) error {
	if desiredTotal < 0 {
		return fmt.Errorf("desiredTotal must be >= 0")
	}
	if f.Client == nil {
		return fmt.Errorf("OCI client not initialized")
	}

	ctx := context.Background()
	fleetName := f.Config.Metadata.Name

	current, err := f.Store.CountActive(fleetName)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	if desiredTotal == current {
		log.Printf("Scale: desired=%d equals current=%d; no changes", desiredTotal, current)
		return nil
	}

	// Determine a group name (simple strategy: first configured group or "default")
	group := "default"
	if len(f.Config.Spec.Instances) > 0 && f.Config.Spec.Instances[0].Name != "" {
		group = f.Config.Spec.Instances[0].Name
	}

	if desiredTotal > current {
		// Scale up: launch missing instances in OCI (parallel with bounded concurrency)
		missing := desiredTotal - current

		type launchRes struct {
			inst client.InstanceInfo
			err  error
		}
		resCh := make(chan launchRes, missing)
		var wg sync.WaitGroup
		sem := make(chan struct{}, 5) // limit parallel launches

		for i := 0; i < missing; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				created, err := f.Client.LaunchInstances(ctx, f.Config, group, 1)
				if err != nil {
					resCh <- launchRes{err: err}
					return
				}
				if len(created) == 0 {
					resCh <- launchRes{err: fmt.Errorf("launch returned no instance")}
					return
				}
				resCh <- launchRes{inst: created[0]}
			}()
		}

		wg.Wait()
		close(resCh)

		count := 0
		for r := range resCh {
			if r.err != nil {
				return fmt.Errorf("launch OCI instances: %w", r.err)
			}
			if err := f.Store.AddActiveRecord(fleetName, group, r.inst.ID, r.inst.DisplayName); err != nil {
				return fmt.Errorf("record instance %s: %w", r.inst.ID, err)
			}
			count++
		}

		log.Printf("Scale: launched %d instances to reach %d", count, desiredTotal)
		return nil
	}

	// Scale down: terminate excess instances in OCI (LIFO from local state)
	toRemove := current - desiredTotal
	recs, err := f.Store.ActiveRecordsLIFO(fleetName, toRemove)
	if err != nil {
		return fmt.Errorf("select instances to remove: %w", err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	// Terminate instances in parallel with bounded concurrency, then mark terminated
	var twg sync.WaitGroup
	terrCh := make(chan error, len(ids))
	tsem := make(chan struct{}, 10)

	for _, id := range ids {
		id := id
		twg.Add(1)
		go func() {
			defer twg.Done()
			tsem <- struct{}{}
			defer func() { <-tsem }()
			if err := f.Client.TerminateInstances(ctx, []string{id}); err != nil {
				terrCh <- fmt.Errorf("terminate %s: %w", id, err)
				return
			}
		}()
	}

	twg.Wait()
	close(terrCh)
	for e := range terrCh {
		if e != nil {
			return fmt.Errorf("terminate instances: %w", e)
		}
	}

	if err := f.Store.MarkTerminatedByIDs(fleetName, ids); err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	log.Printf("Scale: terminated %d instances to reach %d", len(ids), desiredTotal)
	return nil
}

// RollingRestart performs a simple one-by-one replacement of active instances.
func (f *Fleet) RollingRestart() error {
	if f.Client == nil {
		return fmt.Errorf("OCI client not initialized")
	}
	ctx := context.Background()
	fleetName := f.Config.Metadata.Name

	current, err := f.Store.CountActive(fleetName)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}
	if current == 0 {
		log.Printf("RollingRestart: no active instances to restart")
		return nil
	}

	recs, err := f.Store.ActiveRecordsLIFO(fleetName, current)
	if err != nil {
		return fmt.Errorf("list instances to restart: %w", err)
	}

	for i := range recs {
		r := recs[i]
		// 1) Terminate this instance
		if err := f.Client.TerminateInstances(ctx, []string{r.ID}); err != nil {
			return fmt.Errorf("terminate instance %s: %w", r.ID, err)
		}
		if err := f.Store.MarkTerminatedByIDs(fleetName, []string{r.ID}); err != nil {
			return fmt.Errorf("update state for %s: %w", r.ID, err)
		}
		log.Printf("RollingRestart: terminated %s (%s)", r.ID, r.Name)

		// 2) Launch a replacement in the same group
		created, err := f.Client.LaunchInstances(ctx, f.Config, r.Group, 1)
		if err != nil {
			return fmt.Errorf("launch replacement for %s: %w", r.ID, err)
		}
		for _, inst := range created {
			if err := f.Store.AddActiveRecord(fleetName, r.Group, inst.ID, inst.DisplayName); err != nil {
				return fmt.Errorf("record replacement %s: %w", inst.ID, err)
			}
			log.Printf("RollingRestart: launched replacement %s (%s)", inst.ID, inst.DisplayName)
		}
	}

	return nil
}

// Summary returns a simple string describing the loaded config
func (f *Fleet) Summary() string {
	return fmt.Sprintf("Fleet(kind=%s, name=%s, instances=%d)",
		f.Config.Kind, f.Config.Metadata.Name, len(f.Config.Spec.Instances))
}
