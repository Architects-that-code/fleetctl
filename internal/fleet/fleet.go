// internal/fleet/fleet.go
package fleet

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"fleetctl/internal/client"
	"fleetctl/internal/config"
	"fleetctl/internal/lb"
	"fleetctl/internal/metrics"
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

	// Determine remote actual count to avoid relying solely on local state.
	remoteCurrent := current
	if f.Client != nil {
		if inst, err := f.Client.ListInstancesByFleet(ctx, f.Config.Spec.CompartmentID, fleetName); err == nil {
			remoteCurrent = len(inst)
		} else {
			log.Printf("Scale: warning: could not list remote instances: %v (falling back to local state)", err)
		}
	}

	if desiredTotal == current && desiredTotal == remoteCurrent {
		log.Printf("Scale: desired=%d equals current(local=%d, remote=%d); no changes", desiredTotal, current, remoteCurrent)
		return nil
	}

	// Determine a group name (simple strategy: first configured group or "default")
	group := "default"
	if len(f.Config.Spec.Instances) > 0 && f.Config.Spec.Instances[0].Name != "" {
		group = f.Config.Spec.Instances[0].Name
	}

	if desiredTotal > remoteCurrent {
		// Scale up: launch missing instances in OCI (parallel with bounded concurrency)
		missing := desiredTotal - remoteCurrent

		metrics.Reset("scale-up")
		metrics.SetScaleTargets(remoteCurrent, desiredTotal)
		metrics.SetPhase("launch")
		metrics.IncLaunchRequested(missing)
		newInstances := make([]client.InstanceInfo, 0, missing)

		type launchRes struct {
			inst client.InstanceInfo
			err  error
		}
		resCh := make(chan launchRes, missing)
		var wg sync.WaitGroup
		// bounded concurrency from config (fallback to default 5)
		parLaunch := f.Config.Spec.Scaling.ParallelLaunch
		if parLaunch <= 0 {
			parLaunch = 5
		}
		sem := make(chan struct{}, parLaunch)

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
				metrics.IncLaunchFailed(r.err.Error())
				return fmt.Errorf("launch OCI instances: %w", r.err)
			}
			if err := f.Store.AddActiveRecord(fleetName, group, r.inst.ID, r.inst.DisplayName); err != nil {
				return fmt.Errorf("record instance %s: %w", r.inst.ID, err)
			}
			newInstances = append(newInstances, r.inst)
			metrics.IncLaunchSucceeded()
			count++
		}

		log.Printf("Scale: launched %d instances to reach %d", count, desiredTotal)

		// If LB enabled, ensure it exists and register new instances as backends
		if f.Config.Spec.LoadBalancer.Enabled && f.Client != nil {
			spec := f.Config.Spec.LoadBalancer
			lbs := lb.New(f.Client.Provider, f.Client.Region)
			lbID, bsName, lsn, err := lbs.Ensure(ctx, f.Config)
			if err != nil {
				log.Printf("LB ensure failed: %v", err)
			} else {
				for _, inst := range newInstances {
					ip, ierr := f.Client.InstancePrimaryPrivateIP(ctx, f.Config.Spec.CompartmentID, inst.ID)
					if ierr != nil {
						log.Printf("LB resolve IP for %s: %v", inst.ID, ierr)
						continue
					}
					if err := lbs.AddBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
						log.Printf("LB add backend %s:%d: %v", ip, spec.BackendPort, err)
					}
				}
				if n, cerr := lbs.CountBackends(ctx, lbID, bsName); cerr == nil {
					metrics.UpdateLB(true, lbID, n)
					if f.Store != nil {
						_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
						_ = f.Store.SetLBBackendsCount(fleetName, n)
					}
				} else {
					metrics.UpdateLB(true, lbID, 0)
					if f.Store != nil {
						_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
						_ = f.Store.SetLBBackendsCount(fleetName, 0)
					}
				}
			}
		}

		metrics.SetPhase("verify")
		if err := f.verifyActualMatches(ctx, desiredTotal); err != nil {
			metrics.SetError(err.Error())
			return err
		}
		if err := f.SyncState(); err != nil {
			return fmt.Errorf("sync state after scale up: %w", err)
		}
		metrics.Done()
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
	// If LB enabled, deregister targets before terminating instances
	if f.Config.Spec.LoadBalancer.Enabled && f.Client != nil && len(ids) > 0 {
		spec := f.Config.Spec.LoadBalancer
		lbs := lb.New(f.Client.Provider, f.Client.Region)
		if lbID, bsName, lsn, err := lbs.Ensure(ctx, f.Config); err != nil {
			log.Printf("LB ensure failed (scale-down): %v", err)
		} else {
			for _, id := range ids {
				ip, ierr := f.Client.InstancePrimaryPrivateIP(ctx, f.Config.Spec.CompartmentID, id)
				if ierr != nil {
					log.Printf("LB resolve IP for %s: %v", id, ierr)
					continue
				}
				if err := lbs.RemoveBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
					log.Printf("LB remove backend %s:%d: %v", ip, spec.BackendPort, err)
				}
			}
			if n, cerr := lbs.CountBackends(ctx, lbID, bsName); cerr == nil {
				metrics.UpdateLB(true, lbID, n)
				if f.Store != nil {
					_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
					_ = f.Store.SetLBBackendsCount(fleetName, n)
				}
			} else {
				metrics.UpdateLB(true, lbID, 0)
				if f.Store != nil {
					_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
					_ = f.Store.SetLBBackendsCount(fleetName, 0)
				}
			}
		}
	}

	metrics.Reset("scale-down")
	metrics.SetScaleTargets(remoteCurrent, desiredTotal)
	metrics.SetPhase("terminate")
	metrics.IncTerminateRequested(len(ids))

	var twg sync.WaitGroup
	terrCh := make(chan error, len(ids))
	// bounded concurrency from config (fallback to default 10)
	parTerminate := f.Config.Spec.Scaling.ParallelTerminate
	if parTerminate <= 0 {
		parTerminate = 10
	}
	tsem := make(chan struct{}, parTerminate)

	for _, id := range ids {
		id := id
		twg.Add(1)
		go func() {
			defer twg.Done()
			tsem <- struct{}{}
			defer func() { <-tsem }()
			if err := f.Client.TerminateInstances(ctx, []string{id}); err != nil {
				metrics.IncTerminateFailed(err.Error())
				terrCh <- fmt.Errorf("terminate %s: %w", id, err)
				return
			}
			metrics.IncTerminateSucceeded()
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
	metrics.SetPhase("verify")
	if err := f.verifyActualMatches(ctx, desiredTotal); err != nil {
		metrics.SetError(err.Error())
		return err
	}
	if err := f.SyncState(); err != nil {
		return fmt.Errorf("sync state after scale down: %w", err)
	}
	metrics.Done()
	return nil
}

// SyncState queries OCI for instances tagged to this fleet and rebuilds local state.
func (f *Fleet) verifyActualMatches(ctx context.Context, desired int) error {
	if f.Client == nil {
		return fmt.Errorf("OCI client not initialized")
	}
	fleetName := f.Config.Metadata.Name
	deadline := time.Now().Add(2 * time.Minute)
	for {
		instances, err := f.Client.ListInstancesByFleet(ctx, f.Config.Spec.CompartmentID, fleetName)
		if err != nil {
			return fmt.Errorf("verify actual count: %w", err)
		}
		actual := len(instances)
		if actual == desired {
			log.Printf("Scale verify: actual active=%d matches desired=%d", actual, desired)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("scale verify timeout: actual=%d desired=%d", actual, desired)
		}
		time.Sleep(2 * time.Second)
	}
}

func (f *Fleet) SyncState() error {
	if f.Client == nil {
		return fmt.Errorf("OCI client not initialized")
	}
	ctx := context.Background()
	fleetName := f.Config.Metadata.Name

	instances, err := f.Client.ListInstancesByFleet(ctx, f.Config.Spec.CompartmentID, fleetName)
	if err != nil {
		return fmt.Errorf("list fleet instances: %w", err)
	}

	now := time.Now()
	records := make([]state.InstanceRecord, 0, len(instances))
	for _, it := range instances {
		// Best-effort group parsing from display name: <fleet>-<group>-<timestamp>-<idx>
		group := "default"
		prefix := f.Config.Spec.DisplayNamePrefix
		if strings.TrimSpace(prefix) == "" {
			prefix = f.Config.Metadata.Name + "-"
		}
		if strings.HasPrefix(it.DisplayName, prefix) {
			rest := strings.TrimPrefix(it.DisplayName, prefix)
			if idx := strings.Index(rest, "-"); idx > 0 {
				group = rest[:idx]
			}
		}

		records = append(records, state.InstanceRecord{
			ID:        it.ID,
			Group:     group,
			Name:      it.DisplayName,
			Status:    state.StatusActive,
			CreatedAt: now, // unknown; set to now for reconstruction
			UpdatedAt: now,
		})
	}

	if err := f.Store.ResetFleetActive(fleetName, records); err != nil {
		return fmt.Errorf("reset state: %w", err)
	}
	log.Printf("SyncState: rebuilt state for fleet %q with %d active instances", fleetName, len(records))
	return nil
}

// StatusCompare returns a composite status including clearly labeled local and remote (OCI) counts,
// plus local detailed summary, and drift indication if counts differ.
func (f *Fleet) StatusCompare() (string, error) {
	if f.Client == nil {
		return "", fmt.Errorf("OCI client not initialized")
	}
	ctx := context.Background()
	fleetName := f.Config.Metadata.Name

	// Local summary and counts
	localSummary, err := f.Store.Summary(fleetName)
	if err != nil {
		return "", fmt.Errorf("local summary: %w", err)
	}
	localActive, err := f.Store.CountActive(fleetName)
	if err != nil {
		return "", fmt.Errorf("local active: %w", err)
	}

	// Remote/actual counts via fleet tag
	actual, err := f.Client.ListInstancesByFleet(ctx, f.Config.Spec.CompartmentID, fleetName)
	if err != nil {
		return "", fmt.Errorf("actual list: %w", err)
	}
	remoteActive := len(actual)

	out := fmt.Sprintf("Status for fleet %q:\n", fleetName)
	out += fmt.Sprintf("  Local active:  %d\n", localActive)
	out += fmt.Sprintf("  Remote active: %d\n\n", remoteActive)
	out += "Local state detail:\n" + localSummary

	if localActive != remoteActive {
		out += fmt.Sprintf("\n\nDrift detected: local=%d actual=%d", localActive, remoteActive)
	} else {
		out += "\n\nLocal and actual counts match."
	}

	// Append Load Balancer snapshot from local state (if available)
	if f.Store != nil {
		if lb, ok, _ := f.Store.GetLBInfo(fleetName); ok {
			out += "\n\nLoad Balancer:"
			out += fmt.Sprintf("\n  Enabled: %t", lb.Enabled)
			out += fmt.Sprintf("\n  ID: %s", lb.ID)
			out += fmt.Sprintf("\n  BackendSet: %s", lb.BackendSet)
			out += fmt.Sprintf("\n  Listener: %s", lb.Listener)
			out += fmt.Sprintf("\n  Backends: %d", lb.BackendsCount)
			out += fmt.Sprintf("\n  UpdatedAt: %s", lb.UpdatedAt.Format(time.RFC3339))
		} else {
			out += "\n\nLoad Balancer: (no snapshot)"
		}
	}
	return out, nil
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

	metrics.Reset("rolling-restart")
	metrics.SetRollingRestart(0, current)

	// Prepare LB context if enabled
	lbEnabled := f.Config.Spec.LoadBalancer.Enabled && f.Client != nil
	var (
		lbs    *lb.Service
		lbID   string
		bsName string
		lsn    string
	)
	if lbEnabled {
		lbs = lb.New(f.Client.Provider, f.Client.Region)
		if id, bs, l, err := lbs.Ensure(ctx, f.Config); err != nil {
			log.Printf("LB ensure failed (rolling-restart): %v", err)
			lbEnabled = false
		} else {
			lbID, bsName, lsn = id, bs, l
		}
	}

	for i := range recs {
		r := recs[i]
		metrics.SetRollingRestart(i+1, current)

		// If LB enabled, deregister this backend before termination
		if lbEnabled {
			spec := f.Config.Spec.LoadBalancer
			if ip, err := f.Client.InstancePrimaryPrivateIP(ctx, f.Config.Spec.CompartmentID, r.ID); err == nil {
				if err := lbs.RemoveBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
					log.Printf("LB remove backend %s:%d: %v", ip, spec.BackendPort, err)
				}
			} else {
				log.Printf("LB resolve IP for %s: %v", r.ID, err)
			}
		}

		// 1) Terminate this instance
		metrics.SetPhase("terminate")
		if err := f.Client.TerminateInstances(ctx, []string{r.ID}); err != nil {
			metrics.IncTerminateFailed(err.Error())
			return fmt.Errorf("terminate instance %s: %w", r.ID, err)
		}
		metrics.IncTerminateSucceeded()
		if err := f.Store.MarkTerminatedByIDs(fleetName, []string{r.ID}); err != nil {
			return fmt.Errorf("update state for %s: %w", r.ID, err)
		}
		log.Printf("RollingRestart: terminated %s (%s)", r.ID, r.Name)

		// 2) Launch a replacement in the same group
		metrics.SetPhase("launch")
		created, err := f.Client.LaunchInstances(ctx, f.Config, r.Group, 1)
		if err != nil {
			metrics.IncLaunchFailed(err.Error())
			return fmt.Errorf("launch replacement for %s: %w", r.ID, err)
		}
		for _, inst := range created {
			if err := f.Store.AddActiveRecord(fleetName, r.Group, inst.ID, inst.DisplayName); err != nil {
				return fmt.Errorf("record replacement %s: %w", inst.ID, err)
			}
			metrics.IncLaunchSucceeded()

			// If LB enabled, register the new instance backend
			if lbEnabled {
				spec := f.Config.Spec.LoadBalancer
				if ip, err := f.Client.InstancePrimaryPrivateIP(ctx, f.Config.Spec.CompartmentID, inst.ID); err == nil {
					if err := lbs.AddBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
						log.Printf("LB add backend %s:%d: %v", ip, spec.BackendPort, err)
					}
				} else {
					log.Printf("LB resolve IP for %s: %v", inst.ID, err)
				}
			}

			log.Printf("RollingRestart: launched replacement %s (%s)", inst.ID, inst.DisplayName)
		}
	}

	// Update LB snapshot after rolling restart completes
	if lbEnabled {
		if n, err := lbs.CountBackends(ctx, lbID, bsName); err == nil {
			metrics.UpdateLB(true, lbID, n)
			if f.Store != nil {
				_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
				_ = f.Store.SetLBBackendsCount(fleetName, n)
			}
		} else {
			metrics.UpdateLB(true, lbID, 0)
			if f.Store != nil {
				_ = f.Store.SetLBInfo(fleetName, true, lbID, bsName, lsn)
				_ = f.Store.SetLBBackendsCount(fleetName, 0)
			}
		}
	}

	metrics.Done()
	return nil
}

func (f *Fleet) ReconcileLoadBalancer(ctx context.Context) error {
	if f.Client == nil {
		return fmt.Errorf("OCI client not initialized")
	}
	spec := f.Config.Spec.LoadBalancer
	if !spec.Enabled {
		metrics.UpdateLB(false, "", 0)
		if f.Store != nil {
			_ = f.Store.ClearLB(f.Config.Metadata.Name)
		}
		return nil
	}
	lbs := lb.New(f.Client.Provider, f.Client.Region)

	lbID, bsName, lsn, err := lbs.Ensure(ctx, f.Config)
	if err != nil {
		metrics.SetError(fmt.Sprintf("lb ensure: %v", err))
		return err
	}

	// Desired backends from active instances
	insts, err := f.Client.ListInstancesByFleet(ctx, f.Config.Spec.CompartmentID, f.Config.Metadata.Name)
	if err != nil {
		return fmt.Errorf("list instances for lb reconcile: %w", err)
	}
	desired := map[string]struct{}{}
	for _, it := range insts {
		ip, ierr := f.Client.InstancePrimaryPrivateIP(ctx, f.Config.Spec.CompartmentID, it.ID)
		if ierr != nil {
			log.Printf("LB ip for %s: %v", it.ID, ierr)
			continue
		}
		desired[ip] = struct{}{}
	}

	// Current backends
	backends, err := lbs.ListBackends(ctx, lbID, bsName)
	if err != nil {
		return fmt.Errorf("list backends: %w", err)
	}
	current := map[string]struct{}{}
	for _, b := range backends {
		if b.IpAddress != nil {
			current[*b.IpAddress] = struct{}{}
		}
	}

	// Remove stale
	for ip := range current {
		if _, ok := desired[ip]; !ok {
			if err := lbs.RemoveBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
				log.Printf("LB remove stale %s: %v", ip, err)
			}
		}
	}
	// Add missing
	for ip := range desired {
		if _, ok := current[ip]; !ok {
			if err := lbs.AddBackend(ctx, lbID, bsName, ip, spec.BackendPort); err != nil {
				log.Printf("LB add missing %s: %v", ip, err)
			}
		}
	}

	// Refresh backend list for state and metrics
	if items, e := lbs.ListBackends(ctx, lbID, bsName); e == nil {
		ips := make([]string, 0, len(items))
		for _, b := range items {
			if b.IpAddress != nil {
				ips = append(ips, *b.IpAddress)
			}
		}
		metrics.UpdateLB(true, lbID, len(ips))
		if f.Store != nil {
			_ = f.Store.SetLBInfo(f.Config.Metadata.Name, true, lbID, bsName, lsn)
			_ = f.Store.SetLBBackends(f.Config.Metadata.Name, ips)
		}
	} else {
		metrics.UpdateLB(true, lbID, 0)
		if f.Store != nil {
			_ = f.Store.SetLBInfo(f.Config.Metadata.Name, true, lbID, bsName, lsn)
			_ = f.Store.SetLBBackendsCount(f.Config.Metadata.Name, 0)
		}
	}
	return nil
}

// Summary returns a simple string describing the loaded config
func (f *Fleet) Summary() string {
	return fmt.Sprintf("Fleet(kind=%s, name=%s, instances=%d)",
		f.Config.Kind, f.Config.Metadata.Name, len(f.Config.Spec.Instances))
}
