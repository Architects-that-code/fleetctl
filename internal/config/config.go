// internal/config/config.go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// FleetConfig is the root configuration structure
type FleetConfig struct {
	Kind     string   `yaml:"kind"`
	Metadata Metadata `yaml:"metadata"`
	Spec     Spec     `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type Spec struct {
	CompartmentID      string           `yaml:"compartmentId"`
	ImageID            string           `yaml:"imageId"`
	AvailabilityDomain string           `yaml:"availabilityDomain"`
	Shape              string           `yaml:"shape"`
	ShapeConfig        *ShapeConfig     `yaml:"shapeConfig"` // required when using Flex shapes
	SubnetID           string           `yaml:"subnetId"`
	DisplayNamePrefix  string           `yaml:"displayNamePrefix"`
	Scaling            Scaling          `yaml:"scaling"` // optional scaling configuration (bounded concurrency)
	LoadBalancer       LoadBalancerSpec `yaml:"loadBalancer"`
	Auth               Auth             `yaml:"auth"`
	// ... other fields
	DefinedTags  map[string]string `yaml:"definedTags"` // or a more complex type
	FreeformTags map[string]string `yaml:"freeformTags"`
	Instances    []InstanceSpec    `yaml:"instances"`
}

type InstanceSpec struct {
	Name     string `yaml:"name"`
	Count    int    `yaml:"count"`
	SubnetID string `yaml:"subnetId"` // optional per-group override; falls back to spec.subnetId
}

// ShapeConfig config for Flexible shapes (e.g., VM.Standard.*.Flex)
type ShapeConfig struct {
	OCPUs       float32 `yaml:"ocpus"`
	MemoryInGBs float32 `yaml:"memoryInGBs"`
}

// Scaling controls bounded concurrency for scale operations.
type Scaling struct {
	ParallelLaunch    int `yaml:"parallelLaunch"`    // max concurrent launches; default applied if zero
	ParallelTerminate int `yaml:"parallelTerminate"` // max concurrent terminations; default applied if zero
}

// LoadBalancerSpec defines configuration for the OCI Load Balancer.
type LoadBalancerSpec struct {
	Enabled          bool   `yaml:"enabled"`
	SubnetID         string `yaml:"subnetId"`
	IsPrivate        bool   `yaml:"isPrivate"`
	ListenerPort     int    `yaml:"listenerPort"`
	BackendPort      int    `yaml:"backendPort"`
	MinBandwidthMbps int    `yaml:"minBandwidthMbps"`
	MaxBandwidthMbps int    `yaml:"maxBandwidthMbps"`
	HealthPath       string `yaml:"healthPath"`
	Policy           string `yaml:"policy"`
}

type Auth struct {
	Method     string `yaml:"method"`     // "user" or "instance"
	ConfigFile string `yaml:"configFile"` // path to OCI config file when method=user
	Profile    string `yaml:"profile"`    // profile name in OCI config when method=user
	Region     string `yaml:"region"`     // optional region override
}

// ParseFile reads and parses a YAML configuration file
func ParseFile(filename string) (*FleetConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", filename, err)
	}

	var cfg FleetConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML in %q: %w", filename, err)
	}

	return &cfg, nil
}
