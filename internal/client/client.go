// internal/client/client.go
package client

import (
	"context"
	"time"

	"github.com/oracle/oci-go-sdk/common"
	"github.com/oracle/oci-go-sdk/identity"
	"github.com/oracle/oci-go-sdk/core"
	"gopkg.in/yaml.v3"
)

// OCIConfig holds the parsed configuration for connecting to OCI
type OCIConfig struct {
	// ... fields for tenancy ID, region, etc.
}

// FleetConfig holds the parsed fleet configuration (from fleet.yaml)
type FleetConfig struct {
	// ... fields parsed from fleet.yaml
}

// Client manages interaction with OCI
type Client struct {
	// ... client methods
	CreateInstance(ctx context.Context, config FleetConfig, instanceDef InstanceDef) (*core.Instance, error)
	TerminateInstance(ctx context.Context, instanceID string) error
	GetInstanceStatus(ctx context.Context, instanceID string) (*core.LifecycleState, error)
	ListInstances(ctx context.Context) ([]*core.Instance, error)
	// ... other methods
}
