// internal/lb/lb.go
package lb

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"fleetctl/internal/config"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/loadbalancer"
)

// Service wraps OCI Load Balancer operations needed by fleetctl.
type Service struct {
	Provider common.ConfigurationProvider
	Region   string
}

// New constructs a load balancer Service.
func New(provider common.ConfigurationProvider, region string) *Service {
	return &Service{Provider: provider, Region: region}
}

func (s *Service) lbClient() (loadbalancer.LoadBalancerClient, error) {
	c, err := loadbalancer.NewLoadBalancerClientWithConfigurationProvider(s.Provider)
	if err != nil {
		return loadbalancer.LoadBalancerClient{}, fmt.Errorf("lb client init: %w", err)
	}
	if s.Region != "" {
		c.SetRegion(s.Region)
	}
	return c, nil
}

// Backoff/retry helpers for transient throttling or ephemeral LB failures.
func isThrottleError(err error) bool {
	if err == nil {
		return false
	}
	le := strings.ToLower(err.Error())
	return strings.Contains(le, "too many requests") || strings.Contains(le, "429") || strings.Contains(le, "rate limit")
}

func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 500 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= 8*time.Second {
			d = 8 * time.Second
			break
		}
	}
	return d
}

func isTransientLBError(err error) bool {
	if err == nil {
		return false
	}
	le := strings.ToLower(err.Error())
	// Treat throttling and generic "failed"/timeouts as transient for backend ops
	if isThrottleError(err) {
		return true
	}
	return strings.Contains(le, "failed") || strings.Contains(le, "timeout") || strings.Contains(le, "temporar")
}

func (s *Service) waitWorkRequest(ctx context.Context, id string, label string) error {
	lbc, err := s.lbClient()
	if err != nil {
		return err
	}
	for {
		resp, err := lbc.GetWorkRequest(ctx, loadbalancer.GetWorkRequestRequest{WorkRequestId: &id})
		if err != nil {
			// Some environments may not permit reading LB WorkRequests (404 NotAuthorizedOrNotFound).
			// Treat as best-effort complete and let subsequent ensure/reconcile logic verify resources.
			le := strings.ToLower(err.Error())
			if strings.Contains(le, "notauthorizedornotfound") || strings.Contains(le, "404") {
				log.Printf("%s: work request %s not accessible via API (treat as complete): %v", label, id, err)
				return nil
			}
			return fmt.Errorf("%s work request %s: %w", label, id, err)
		}
		state := strings.ToUpper(string(resp.WorkRequest.LifecycleState))
		log.Printf("%s: work request %s status=%s", label, id, state)
		if state == "SUCCEEDED" {
			return nil
		}
		if state == "FAILED" || state == "CANCELED" {
			return fmt.Errorf("%s failed (state=%s)", label, state)
		}
		time.Sleep(2 * time.Second)
	}
}

// derived resource names
func (s *Service) names(cfg config.FleetConfig) (displayName, backendSet, listener string) {
	displayName = fmt.Sprintf("%s-lb", cfg.Metadata.Name)
	backendSet = "fleet-backendset"
	listener = "http-listener"
	return
}

// Ensure creates or ensures existence of LB, backend set and listener.
// Returns LB OCID, backend set name and listener name.
func (s *Service) Ensure(ctx context.Context, cfg config.FleetConfig) (string, string, string, error) {
	if s == nil || s.Provider == nil {
		return "", "", "", fmt.Errorf("lb service not initialized")
	}
	spec := cfg.Spec.LoadBalancer
	if !spec.Enabled {
		return "", "", "", fmt.Errorf("load balancer is disabled in config")
	}

	lbc, err := s.lbClient()
	if err != nil {
		return "", "", "", err
	}

	displayName, backendSet, listener := s.names(cfg)

	// 1) Find or create Load Balancer
	var lbID string
	{
		req := loadbalancer.ListLoadBalancersRequest{
			CompartmentId: &cfg.Spec.CompartmentID,
		}
		resp, err := lbc.ListLoadBalancers(ctx, req)
		if err != nil {
			return "", "", "", fmt.Errorf("list load balancers: %w", err)
		}
		for _, item := range resp.Items {
			if item.DisplayName != nil && *item.DisplayName == displayName {
				if item.Id != nil {
					lbID = *item.Id
					break
				}
			}
		}
	}

	if lbID == "" {
		shapeName := "flexible"
		minBw := spec.MinBandwidthMbps
		maxBw := spec.MaxBandwidthMbps
		subnetIds := []string{strings.TrimSpace(spec.SubnetID)}
		if subnetIds[0] == "" {
			return "", "", "", fmt.Errorf("loadBalancer.subnetId must be set")
		}
		// Freeform tags: tag LB with the fleet name for traceability
		ftags := map[string]string{
			"fleetctl-fleet": cfg.Metadata.Name,
		}
		details := loadbalancer.CreateLoadBalancerDetails{
			CompartmentId: &cfg.Spec.CompartmentID,
			DisplayName:   &displayName,
			IsPrivate:     &spec.IsPrivate,
			ShapeName:     &shapeName,
			ShapeDetails: &loadbalancer.ShapeDetails{
				MinimumBandwidthInMbps: &minBw,
				MaximumBandwidthInMbps: &maxBw,
			},
			SubnetIds:    subnetIds,
			FreeformTags: ftags,
		}
		resp, err := lbc.CreateLoadBalancer(ctx, loadbalancer.CreateLoadBalancerRequest{
			CreateLoadBalancerDetails: details,
		})
		if err != nil {
			return "", "", "", fmt.Errorf("create load balancer: %w", err)
		}
		if resp.OpcWorkRequestId != nil {
			if err := s.waitWorkRequest(ctx, *resp.OpcWorkRequestId, "create load balancer"); err != nil {
				return "", "", "", err
			}
		}
		// Fetch again to obtain the ID by name
		listResp, err := lbc.ListLoadBalancers(ctx, loadbalancer.ListLoadBalancersRequest{
			CompartmentId: &cfg.Spec.CompartmentID,
		})
		if err != nil {
			return "", "", "", fmt.Errorf("list after create: %w", err)
		}
		for _, item := range listResp.Items {
			if item.DisplayName != nil && *item.DisplayName == displayName && item.Id != nil {
				lbID = *item.Id
				break
			}
		}
		if lbID == "" {
			return "", "", "", fmt.Errorf("created load balancer but could not resolve its ID")
		}
	}

	// 2) Ensure Backend Set
	{
		_, err := lbc.GetBackendSet(ctx, loadbalancer.GetBackendSetRequest{
			LoadBalancerId: &lbID,
			BackendSetName: &backendSet,
		})
		if err != nil {
			// If not found, create
			if !strings.Contains(strings.ToLower(err.Error()), "notfound") &&
				!strings.Contains(strings.ToLower(err.Error()), "404") {
				return "", "", "", fmt.Errorf("get backend set: %w", err)
			}
			policy := spec.Policy
			if strings.TrimSpace(policy) == "" {
				policy = "ROUND_ROBIN"
			}
			proto := "HTTP"
			hp := strings.TrimSpace(spec.HealthPath)
			port := spec.BackendPort
			hc := loadbalancer.HealthCheckerDetails{
				Protocol: &proto,
				UrlPath:  &hp,
				Port:     &port,
			}
			cbs := loadbalancer.CreateBackendSetDetails{
				Name:                                    &backendSet,
				Policy:                                  &policy,
				HealthChecker:                           &hc,
				SessionPersistenceConfiguration:         nil,
				LbCookieSessionPersistenceConfiguration: nil,
				SslConfiguration:                        nil,
				Backends:                                nil,
			}
			resp, err := lbc.CreateBackendSet(ctx, loadbalancer.CreateBackendSetRequest{
				LoadBalancerId:          &lbID,
				CreateBackendSetDetails: cbs,
			})
			if err != nil {
				return "", "", "", fmt.Errorf("create backend set: %w", err)
			}
			if resp.OpcWorkRequestId != nil {
				if err := s.waitWorkRequest(ctx, *resp.OpcWorkRequestId, "create backend set"); err != nil {
					return "", "", "", err
				}
			}
		}
	}

	// 3) Ensure Listener
	{
		// Some SDKs do not expose GetListener; fetch LB and inspect Listeners map instead.
		lbResp, err := lbc.GetLoadBalancer(ctx, loadbalancer.GetLoadBalancerRequest{
			LoadBalancerId: &lbID,
		})
		if err != nil {
			return "", "", "", fmt.Errorf("get load balancer for listeners: %w", err)
		}
		has := false
		if lbResp.LoadBalancer.Listeners != nil {
			if _, ok := lbResp.LoadBalancer.Listeners[listener]; ok {
				has = true
			}
		}
		if !has {
			proto := "HTTP"
			lport := spec.ListenerPort
			cld := loadbalancer.CreateListenerDetails{
				Name:                  &listener,
				DefaultBackendSetName: &backendSet,
				Port:                  &lport,
				Protocol:              &proto,
			}
			resp, err := lbc.CreateListener(ctx, loadbalancer.CreateListenerRequest{
				LoadBalancerId:        &lbID,
				CreateListenerDetails: cld,
			})
			if err != nil {
				// If already exists, treat as success
				if strings.Contains(strings.ToLower(err.Error()), "already exists") {
					// noop
				} else {
					return "", "", "", fmt.Errorf("create listener: %w", err)
				}
			} else if resp.OpcWorkRequestId != nil {
				if err := s.waitWorkRequest(ctx, *resp.OpcWorkRequestId, "create listener"); err != nil {
					return "", "", "", err
				}
			}
		}
	}

	return lbID, backendSet, listener, nil
}

// ListBackends returns the current backends in the named backend set.
func (s *Service) ListBackends(ctx context.Context, lbID, backendSet string) ([]loadbalancer.Backend, error) {
	lbc, err := s.lbClient()
	if err != nil {
		return nil, err
	}
	// Not all SDKs expose ListBackends; GetBackendSet includes current backends.
	resp, err := lbc.GetBackendSet(ctx, loadbalancer.GetBackendSetRequest{
		LoadBalancerId: &lbID,
		BackendSetName: &backendSet,
	})
	if err != nil {
		return nil, fmt.Errorf("get backend set %s: %w", backendSet, err)
	}
	items := resp.BackendSet.Backends
	return items, nil
}

// CountBackends returns the number of backends in the named backend set.
func (s *Service) CountBackends(ctx context.Context, lbID, backendSet string) (int, error) {
	items, err := s.ListBackends(ctx, lbID, backendSet)
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

// AddBackend registers an IP:port as a backend.
func (s *Service) AddBackend(ctx context.Context, lbID, backendSet, ip string, port int) error {
	lbc, err := s.lbClient()
	if err != nil {
		return err
	}
	details := loadbalancer.CreateBackendDetails{
		IpAddress: &ip,
		Port:      &port,
	}
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		resp, err := lbc.CreateBackend(ctx, loadbalancer.CreateBackendRequest{
			LoadBalancerId:       &lbID,
			BackendSetName:       &backendSet,
			CreateBackendDetails: details,
		})
		if err != nil {
			le := strings.ToLower(err.Error())
			// Already exists => idempotent success
			if strings.Contains(le, "already exists") {
				return nil
			}
			if !isTransientLBError(err) {
				return fmt.Errorf("create backend %s:%d: %w", ip, port, err)
			}
			lastErr = err
			time.Sleep(backoffDelay(attempt))
			continue
		}
		if resp.OpcWorkRequestId != nil {
			if err := s.waitWorkRequest(ctx, *resp.OpcWorkRequestId, "create backend"); err != nil {
				// Treat failed/timeout as transient; retry a few times
				if !isTransientLBError(err) {
					return err
				}
				lastErr = err
				time.Sleep(backoffDelay(attempt))
				continue
			}
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("create backend %s:%d after retries: %w", ip, port, lastErr)
	}
	return fmt.Errorf("create backend %s:%d failed after retries", ip, port)
}

// RemoveBackend deregisters an IP:port backend.
func (s *Service) RemoveBackend(ctx context.Context, lbID, backendSet, ip string, port int) error {
	lbc, err := s.lbClient()
	if err != nil {
		return err
	}
	// BackendName format is "IP:port"
	name := fmt.Sprintf("%s:%d", ip, port)
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		resp, err := lbc.DeleteBackend(ctx, loadbalancer.DeleteBackendRequest{
			LoadBalancerId: &lbID,
			BackendSetName: &backendSet,
			BackendName:    &name,
		})
		if err != nil {
			le := strings.ToLower(err.Error())
			// Not found => idempotent success
			if strings.Contains(le, "notfound") || strings.Contains(le, "404") {
				return nil
			}
			if !isTransientLBError(err) {
				return fmt.Errorf("delete backend %s: %w", name, err)
			}
			lastErr = err
			time.Sleep(backoffDelay(attempt))
			continue
		}
		if resp.OpcWorkRequestId != nil {
			if err := s.waitWorkRequest(ctx, *resp.OpcWorkRequestId, "delete backend"); err != nil {
				if !isTransientLBError(err) {
					return err
				}
				lastErr = err
				time.Sleep(backoffDelay(attempt))
				continue
			}
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("delete backend %s after retries: %w", name, lastErr)
	}
	return fmt.Errorf("delete backend %s failed after retries", name)
}
