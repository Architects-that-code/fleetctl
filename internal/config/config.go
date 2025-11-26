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
	CompartmentID      string `yaml:"compartmentId"`
	ImageID            string `yaml:"imageId"`
	AvailabilityDomain string `yaml:"availabilityDomain"`
	Auth               Auth   `yaml:"auth"`
	// ... other fields
	DefinedTags  map[string]string `yaml:"definedTags"` // or a more complex type
	FreeformTags map[string]string `yaml:"freeformTags"`
	Instances    []InstanceSpec    `yaml:"instances"`
}

type InstanceSpec struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
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
