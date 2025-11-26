// internal/client/client.go
package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fleetctl/internal/config"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// Client encapsulates OCI auth provider and region.
// More service clients (e.g., compute) can be added as we implement functionality.
type Client struct {
	Provider common.ConfigurationProvider
	Region   string
}

// AuthInfo captures details discovered during auth validation.
type AuthInfo struct {
	Region            string
	TenancyOCID       string
	UserOCID          string
	RegionsCount      int
	SubscribedRegions []string
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

// Validate performs a lightweight API call to verify auth works.
func (c *Client) Validate(ctx context.Context) error {
	_, err := c.ValidateInfo(ctx)
	return err
}
