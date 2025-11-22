// internal/config/config.go
package config

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
	CompartmentID string `yaml:"compartmentId"`
	ImageID       string `yaml:"imageId"`
	// ... other fields
	DefinedTags  map[string]string `yaml:"definedTags"` // or a more complex type
	FreeformTags map[string]string `yaml:"freeformTags"`
	Instances    []InstanceSpec    `yaml:"instances"`
}

type InstanceSpec struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

// ParseFile reads and parses a YAML configuration file
func ParseFile(filename string) (*FleetConfig, error) {
	// ... implement file reading and YAML unmarshalling
}
