package diagram

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testProjectRoot returns the project root directory relative to the test file.
func testProjectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file location")
	}
	// filename is like /path/to/project/internal/diagram/diagram_test.go
	// We need to go up two directories to get to project root
	dir := filepath.Dir(filename)
	return filepath.Join(dir, "..", "..")
}

func TestNewGenerator(t *testing.T) {
	rootDir := testProjectRoot(t)
	gen, err := NewGenerator(rootDir)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	if gen.moduleName != "fleetctl" {
		t.Errorf("expected module name 'fleetctl', got %q", gen.moduleName)
	}
}

func TestNewGeneratorMissingGoMod(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := NewGenerator(tmpDir)
	if err == nil {
		t.Error("expected error for missing go.mod")
	}
}

func TestScanPackages(t *testing.T) {
	rootDir := testProjectRoot(t)
	gen, err := NewGenerator(rootDir)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	pkgs, err := gen.scanPackages()
	if err != nil {
		t.Fatalf("scanPackages failed: %v", err)
	}

	// Check that we found expected packages
	expectedPkgs := []string{
		"fleetctl/internal/config",
		"fleetctl/internal/client",
		"fleetctl/internal/fleet",
		"fleetctl/internal/state",
		"fleetctl/internal/metrics",
		"fleetctl/internal/diagram",
	}

	for _, expected := range expectedPkgs {
		if _, ok := pkgs[expected]; !ok {
			t.Errorf("expected to find package %q", expected)
		}
	}

	// Check that config package has expected types
	configPkg, ok := pkgs["fleetctl/internal/config"]
	if !ok {
		t.Fatal("config package not found")
	}

	if configPkg.Name != "config" {
		t.Errorf("expected package name 'config', got %q", configPkg.Name)
	}

	// Should have FleetConfig type
	hasFleetConfig := false
	for _, typ := range configPkg.Types {
		if typ == "FleetConfig" {
			hasFleetConfig = true
			break
		}
	}
	if !hasFleetConfig {
		t.Error("expected FleetConfig type in config package")
	}
}

func TestGeneratePackageDeps(t *testing.T) {
	rootDir := testProjectRoot(t)
	gen, err := NewGenerator(rootDir)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	diagram, err := gen.Generate(PackageDeps)
	if err != nil {
		t.Fatalf("Generate(PackageDeps) failed: %v", err)
	}

	// Check that output contains mermaid markers
	if !strings.HasPrefix(diagram, "```mermaid") {
		t.Error("expected diagram to start with ```mermaid")
	}
	if !strings.HasSuffix(diagram, "```\n") {
		t.Error("expected diagram to end with ```")
	}

	// Check for expected content
	if !strings.Contains(diagram, "graph TD") {
		t.Error("expected diagram to contain 'graph TD'")
	}
	if !strings.Contains(diagram, "Internal Packages") {
		t.Error("expected diagram to contain 'Internal Packages' subgraph")
	}
}

func TestGenerateArchitecture(t *testing.T) {
	rootDir := testProjectRoot(t)
	gen, err := NewGenerator(rootDir)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	diagram, err := gen.Generate(Architecture)
	if err != nil {
		t.Fatalf("Generate(Architecture) failed: %v", err)
	}

	// Check that output contains mermaid markers
	if !strings.HasPrefix(diagram, "```mermaid") {
		t.Error("expected diagram to start with ```mermaid")
	}

	// Check for expected content
	if !strings.Contains(diagram, "flowchart TB") {
		t.Error("expected diagram to contain 'flowchart TB'")
	}
	if !strings.Contains(diagram, "OCI Cloud") {
		t.Error("expected diagram to mention OCI Cloud")
	}
	if !strings.Contains(diagram, "Fleet Operations") {
		t.Error("expected diagram to mention Fleet Operations")
	}
}

func TestGenerateUnknownType(t *testing.T) {
	rootDir := testProjectRoot(t)
	gen, err := NewGenerator(rootDir)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	_, err = gen.Generate(DiagramType("unknown"))
	if err == nil {
		t.Error("expected error for unknown diagram type")
	}
}

func TestAvailableDiagramTypes(t *testing.T) {
	types := AvailableDiagramTypes()
	if len(types) != 2 {
		t.Errorf("expected 2 diagram types, got %d", len(types))
	}

	hasPackages := false
	hasArch := false
	for _, dt := range types {
		if dt == PackageDeps {
			hasPackages = true
		}
		if dt == Architecture {
			hasArch = true
		}
	}

	if !hasPackages {
		t.Error("expected PackageDeps in available types")
	}
	if !hasArch {
		t.Error("expected Architecture in available types")
	}
}

func TestShortLabel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"fleetctl/internal/config", "internal/config"},
		{"fleetctl/internal/fleet", "internal/fleet"},
		{"fleetctl/cmd/fleetctl", "cmd/fleetctl"},
		{"fleetctl", "fleetctl"},
	}

	for _, tc := range tests {
		result := shortLabel(tc.input)
		if result != tc.expected {
			t.Errorf("shortLabel(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestReadModuleName(t *testing.T) {
	// Create a temporary directory with a go.mod
	tmpDir := t.TempDir()
	modContent := "module example.com/myproject\n\ngo 1.23\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(modContent), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	name, err := readModuleName(tmpDir)
	if err != nil {
		t.Fatalf("readModuleName failed: %v", err)
	}
	if name != "example.com/myproject" {
		t.Errorf("expected 'example.com/myproject', got %q", name)
	}
}
