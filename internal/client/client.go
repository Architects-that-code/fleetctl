// internal/client/client.go
package client

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fleetctl/internal/config"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/workrequests"
)

// Client encapsulates OCI auth provider and region.
// More service clients (e.g., compute) can be added as we implement functionality.
type Client struct {
	Provider common.ConfigurationProvider
	Region   string
}

// FleetTagKey is the freeform tag key used to mark instances for a given fleet.
const FleetTagKey = "fleetctl-fleet"

// AuthInfo captures details discovered during auth validation.
type AuthInfo struct {
	Region            string
	TenancyOCID       string
	UserOCID          string
	RegionsCount      int
	SubscribedRegions []string
}

// InstanceInfo represents minimal details for an OCI instance we manage.
type InstanceInfo struct {
	ID          string
	DisplayName string
	Lifecycle   string
}

// New initializes an OCI client using either:
// - User principal (OCI config file) when auth.Method == "user"
// - Instance principal when auth.Method == "instance" or empty
// Region resolution order: auth.Region -> OCI_REGION env -> provider.Region() -> ""
func New(a config.Auth) (*Client, error) {
	method := strings.ToLower(strings.TrimSpace(a.Method))

	var (
		provider common.ConfigurationProvider
		err      error
	)

	switch method {
	case "", "instance":
		provider, err = auth.InstancePrincipalConfigurationProvider()
		if err != nil {
			return nil, fmt.Errorf("instance principal provider: %w; if running locally, set spec.auth.method to 'user' and configure configFile/profile", err)
		}

	case "user":
		cfgPath := strings.TrimSpace(a.ConfigFile)
		if cfgPath == "" {
			if envPath := strings.TrimSpace(os.Getenv("OCI_CLI_CONFIG_FILE")); envPath != "" {
				cfgPath = envPath
			} else {
				home, herr := os.UserHomeDir()
				if herr != nil {
					return nil, fmt.Errorf("determine home directory for default OCI config: %w", herr)
				}
				cfgPath = filepath.Join(home, ".oci", "config")
			}
		}
		profile := strings.TrimSpace(a.Profile)
		if profile == "" {
			profile = "DEFAULT"
		}
		// Expand env vars and leading ~ in configFile path for local usability
		cfgPath = os.ExpandEnv(cfgPath)
		if strings.HasPrefix(cfgPath, "~") {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, fmt.Errorf("expand ~ in OCI config path %q: %w", cfgPath, herr)
			}
			if cfgPath == "~" {
				cfgPath = home
			} else if strings.HasPrefix(cfgPath, "~/") {
				cfgPath = filepath.Join(home, cfgPath[2:])
			} else {
				// Basic handling for paths beginning with ~ (not ~user)
				cfgPath = filepath.Join(home, cfgPath[1:])
			}
		}
		if _, statErr := os.Stat(cfgPath); statErr != nil {
			if os.IsNotExist(statErr) {
				return nil, fmt.Errorf("OCI config file not found at %s; set spec.auth.configFile or ensure the file exists", cfgPath)
			}
			return nil, fmt.Errorf("accessing OCI config file %s: %w", cfgPath, statErr)
		}
		provider, err = common.ConfigurationProviderFromFileWithProfile(cfgPath, profile, "")
		if err != nil {
			return nil, fmt.Errorf("user principal from %s (profile %s): %w", cfgPath, profile, err)
		}

	default:
		return nil, fmt.Errorf("unknown auth.method %q (expected 'user' or 'instance')", a.Method)
	}

	region := strings.TrimSpace(a.Region)
	if region == "" {
		if env := os.Getenv("OCI_REGION"); env != "" {
			region = env
		} else if r, rerr := provider.Region(); rerr == nil {
			region = r
		}
	}

	return &Client{
		Provider: provider,
		Region:   region,
	}, nil
}

// ValidateInfo performs lightweight calls to verify auth and returns useful details.
func (c *Client) ValidateInfo(ctx context.Context) (AuthInfo, error) {
	if c == nil || c.Provider == nil {
		return AuthInfo{}, fmt.Errorf("client not initialized")
	}

	idc, err := identity.NewIdentityClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return AuthInfo{}, fmt.Errorf("identity client init: %w", err)
	}
	if c.Region != "" {
		idc.SetRegion(c.Region)
	}

	// 1) Global regions list (simple ping)
	regionsResp, err := idc.ListRegions(ctx)
	if err != nil {
		return AuthInfo{}, fmt.Errorf("auth validation failed (ListRegions): %w", err)
	}

	info := AuthInfo{
		Region:       c.Region,
		RegionsCount: len(regionsResp.Items),
	}

	// 2) Tenancy and user context if available
	if ten, err := c.Provider.TenancyOCID(); err == nil {
		info.TenancyOCID = ten

		// Try to list region subscriptions for the tenancy
		req := identity.ListRegionSubscriptionsRequest{TenancyId: &ten}
		if subsResp, e := idc.ListRegionSubscriptions(ctx, req); e == nil {
			for _, s := range subsResp.Items {
				if s.RegionName != nil {
					info.SubscribedRegions = append(info.SubscribedRegions, *s.RegionName)
				}
			}
		}
	}
	if u, err := c.Provider.UserOCID(); err == nil {
		info.UserOCID = u
	}

	return info, nil
}

// waitWorkRequest polls a Work Request until it reaches Succeeded or Failed.
func (c *Client) waitWorkRequest(ctx context.Context, id string, label string) error {
	if c == nil || c.Provider == nil {
		return fmt.Errorf("client not initialized")
	}
	wrc, err := workrequests.NewWorkRequestClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return fmt.Errorf("workrequests client init: %w", err)
	}
	if c.Region != "" {
		wrc.SetRegion(c.Region)
	}
	for {
		resp, err := wrc.GetWorkRequest(ctx, workrequests.GetWorkRequestRequest{WorkRequestId: &id})
		if err != nil {
			return fmt.Errorf("work request %s: %w", id, err)
		}
		status := resp.WorkRequest.Status
		pct := 0
		if resp.WorkRequest.PercentComplete != nil {
			pct = int(*resp.WorkRequest.PercentComplete)
		}
		log.Printf("%s: work request %s status=%s (%d%%)", label, id, status, pct)
		switch status {
		case workrequests.WorkRequestStatusSucceeded:
			return nil
		case workrequests.WorkRequestStatusFailed:
			ers, _ := wrc.ListWorkRequestErrors(ctx, workrequests.ListWorkRequestErrorsRequest{WorkRequestId: &id})
			if len(ers.Items) > 0 && ers.Items[0].Message != nil {
				return fmt.Errorf("%s failed: %s", label, *ers.Items[0].Message)
			}
			return fmt.Errorf("%s failed", label)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitInstanceState polls an instance until it reaches the target lifecycle state.
// For Terminated, a NotAuthorizedOrNotFound response is also treated as success.
func (c *Client) waitInstanceState(ctx context.Context, id string, target core.InstanceLifecycleStateEnum) error {
	if c == nil || c.Provider == nil {
		return fmt.Errorf("client not initialized")
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return fmt.Errorf("compute client init: %w", err)
	}
	if c.Region != "" {
		cc.SetRegion(c.Region)
	}
	for {
		resp, err := cc.GetInstance(ctx, core.GetInstanceRequest{InstanceId: &id})
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "notauthorizedornotfound") && target == core.InstanceLifecycleStateTerminated {
				log.Printf("wait %s: instance %s not found (treat as done)", target, id)
				return nil
			}
			return fmt.Errorf("get instance %s: %w", id, err)
		}
		state := resp.Instance.LifecycleState
		log.Printf("wait %s: instance %s state=%s", target, id, state)
		if state == target {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// LaunchInstances creates n instances in OCI using details from cfg and returns their basic info.
func (c *Client) LaunchInstances(ctx context.Context, cfg config.FleetConfig, group string, n int) ([]InstanceInfo, error) {
	if n <= 0 {
		return nil, nil
	}
	if c == nil || c.Provider == nil {
		return nil, fmt.Errorf("client not initialized")
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return nil, fmt.Errorf("compute client init: %w", err)
	}
	if c.Region != "" {
		cc.SetRegion(c.Region)
	}

	var out []InstanceInfo
	prefix := cfg.Spec.DisplayNamePrefix
	if strings.TrimSpace(prefix) == "" {
		prefix = fmt.Sprintf("%s-%s", cfg.Metadata.Name, group)
	}

	// Resolve availability domain to a full AD name in the current region.
	// Accepts either a full name (e.g., "kIdk:US-ASHBURN-AD-1") or a suffix (e.g., "US-ASHBURN-AD-1" or "PHX-AD-1").
	reqAD := strings.TrimSpace(cfg.Spec.AvailabilityDomain)
	idc, err := identity.NewIdentityClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return nil, fmt.Errorf("identity client init: %w", err)
	}
	if c.Region != "" {
		idc.SetRegion(c.Region)
	}
	ten, err := c.Provider.TenancyOCID()
	if err != nil {
		return nil, fmt.Errorf("resolve tenancy for availability domains: %w", err)
	}
	ads, err := idc.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId: &ten,
	})
	if err != nil {
		return nil, fmt.Errorf("list availability domains: %w", err)
	}
	if len(ads.Items) == 0 || ads.Items[0].Name == nil {
		return nil, fmt.Errorf("no availability domains found in region %s", c.Region)
	}
	// Default to the first AD, but override if we can match the requested value.
	ad := *ads.Items[0].Name
	if reqAD != "" {
		for _, item := range ads.Items {
			if item.Name == nil {
				continue
			}
			full := *item.Name
			if strings.EqualFold(full, reqAD) || strings.HasSuffix(full, reqAD) {
				ad = full
				break
			}
		}
	}

	// Resolve subnet ID with optional per-group override
	subnetID := strings.TrimSpace(cfg.Spec.SubnetID)
	for _, g := range cfg.Spec.Instances {
		if g.Name == group && strings.TrimSpace(g.SubnetID) != "" {
			subnetID = strings.TrimSpace(g.SubnetID)
			break
		}
	}
	if subnetID == "" {
		return nil, fmt.Errorf("no subnetId specified (spec.subnetId or instances[%s].subnetId)", group)
	}

	// Preflight: verify subnet exists and is accessible, and ensure compartment alignment
	vnc, err := core.NewVirtualNetworkClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return nil, fmt.Errorf("virtual network client init: %w", err)
	}
	if c.Region != "" {
		vnc.SetRegion(c.Region)
	}
	subnetResp, err := vnc.GetSubnet(ctx, core.GetSubnetRequest{SubnetId: &subnetID})
	if err != nil {
		return nil, fmt.Errorf("verify subnet %s: %w", subnetID, err)
	}
	if subnetResp.Subnet.CompartmentId != nil && *subnetResp.Subnet.CompartmentId != cfg.Spec.CompartmentID {
		return nil, fmt.Errorf("subnet %s is in compartment %s but spec.compartmentId is %s", subnetID, *subnetResp.Subnet.CompartmentId, cfg.Spec.CompartmentID)
	}

	// Preflight: verify image exists in this region
	if _, err := cc.GetImage(ctx, core.GetImageRequest{ImageId: &cfg.Spec.ImageID}); err != nil {
		return nil, fmt.Errorf("verify image %s: %w", cfg.Spec.ImageID, err)
	}

	// ShapeConfig for Flexible shapes
	var shapeCfg *core.LaunchInstanceShapeConfigDetails
	if strings.Contains(strings.ToLower(cfg.Spec.Shape), "flex") {
		if cfg.Spec.ShapeConfig == nil {
			return nil, fmt.Errorf("shape %q requires shapeConfig (ocpus, memoryInGBs)", cfg.Spec.Shape)
		}
		oc := cfg.Spec.ShapeConfig.OCPUs
		mem := cfg.Spec.ShapeConfig.MemoryInGBs
		shapeCfg = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       &oc,
			MemoryInGBs: &mem,
		}
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), i)
		// Merge user-provided freeform tags with our fleet tag
		ftags := map[string]string{}
		for k, v := range cfg.Spec.FreeformTags {
			ftags[k] = v
		}
		ftags[FleetTagKey] = cfg.Metadata.Name

		details := core.LaunchInstanceDetails{
			CompartmentId:      &cfg.Spec.CompartmentID,
			AvailabilityDomain: &ad,
			Shape:              &cfg.Spec.Shape,
			ShapeConfig:        shapeCfg,
			SourceDetails: &core.InstanceSourceViaImageDetails{
				ImageId: &cfg.Spec.ImageID,
			},
			CreateVnicDetails: &core.CreateVnicDetails{
				SubnetId: &subnetID,
			},
			DisplayName:  &name,
			FreeformTags: ftags,
			// NOTE: DefinedTags in OCI SDK require map[string]map[string]interface{}; skipped initially.
		}
		log.Printf("Launch: requesting %s (group=%s, shape=%s, ad=%s, subnet=%s)", name, group, cfg.Spec.Shape, ad, subnetID)
		req := core.LaunchInstanceRequest{LaunchInstanceDetails: details}
		resp, err := cc.LaunchInstance(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("launch instance %d/%d: %w", i+1, n, err)
		}
		ii := InstanceInfo{DisplayName: name}
		if resp.Instance.Id != nil {
			ii.ID = *resp.Instance.Id
		}
		log.Printf("Launch: requested %s id=%s", name, ii.ID)
		// Wait for completion: prefer Work Request if present; otherwise poll until RUNNING
		if resp.OpcWorkRequestId != nil {
			if err := c.waitWorkRequest(ctx, *resp.OpcWorkRequestId, fmt.Sprintf("launch %s", ii.ID)); err != nil {
				return nil, fmt.Errorf("wait for launch %s: %w", ii.ID, err)
			}
		} else if ii.ID != "" {
			if err := c.waitInstanceState(ctx, ii.ID, core.InstanceLifecycleStateRunning); err != nil {
				return nil, fmt.Errorf("wait running %s: %w", ii.ID, err)
			}
		}
		if resp.Instance.LifecycleState != "" {
			ii.Lifecycle = string(resp.Instance.LifecycleState)
		}
		out = append(out, ii)
	}
	return out, nil
}

// TerminateInstances terminates the specified OCI instances.
func (c *Client) ListInstancesByFleet(ctx context.Context, compartmentId, fleetName string) ([]InstanceInfo, error) {
	if c == nil || c.Provider == nil {
		return nil, fmt.Errorf("client not initialized")
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return nil, fmt.Errorf("compute client init: %w", err)
	}
	if c.Region != "" {
		cc.SetRegion(c.Region)
	}

	var out []InstanceInfo
	var page *string
	for {
		resp, err := cc.ListInstances(ctx, core.ListInstancesRequest{
			CompartmentId: &compartmentId,
			Page:          page,
		})
		if err != nil {
			return nil, fmt.Errorf("list instances: %w", err)
		}
		for _, it := range resp.Items {
			// Skip terminated
			if it.LifecycleState == core.InstanceLifecycleStateTerminated {
				continue
			}
			// Match our fleet tag
			if it.FreeformTags != nil {
				if val, ok := it.FreeformTags[FleetTagKey]; ok && val == fleetName {
					info := InstanceInfo{}
					if it.Id != nil {
						info.ID = *it.Id
					}
					if it.DisplayName != nil {
						info.DisplayName = *it.DisplayName
					}
					if it.LifecycleState != "" {
						info.Lifecycle = string(it.LifecycleState)
					}
					out = append(out, info)
				}
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			break
		}
		page = resp.OpcNextPage
	}
	return out, nil
}

// TerminateInstances terminates the specified OCI instances.
func (c *Client) TerminateInstances(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if c == nil || c.Provider == nil {
		return fmt.Errorf("client not initialized")
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(c.Provider)
	if err != nil {
		return fmt.Errorf("compute client init: %w", err)
	}
	if c.Region != "" {
		cc.SetRegion(c.Region)
	}

	for _, id := range ids {
		if id == "" {
			continue
		}
		log.Printf("Terminate: requesting instance %s", id)
		_, err := cc.TerminateInstance(ctx, core.TerminateInstanceRequest{InstanceId: &id})
		if err != nil {
			return fmt.Errorf("terminate instance %s: %w", id, err)
		}
		log.Printf("Terminate: requested instance %s", id)
		// Wait for completion by polling lifecycle to TERMINATED
		if err := c.waitInstanceState(ctx, id, core.InstanceLifecycleStateTerminated); err != nil {
			return fmt.Errorf("wait terminated %s: %w", id, err)
		}
	}
	return nil
}

// Validate performs a lightweight API call to verify auth works.
func (c *Client) Validate(ctx context.Context) error {
	_, err := c.ValidateInfo(ctx)
	return err
}
