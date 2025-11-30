// Package diagram generates Mermaid diagrams from Go source code analysis.
package diagram

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiagramType represents the type of diagram to generate.
type DiagramType string

const (
	// PackageDeps generates a package dependency diagram.
	PackageDeps DiagramType = "packages"
	// Architecture generates a high-level architecture diagram.
	Architecture DiagramType = "architecture"
)

// Generator generates diagrams from Go source code.
type Generator struct {
	rootDir    string
	moduleName string
}

// NewGenerator creates a new diagram generator for the given root directory.
func NewGenerator(rootDir string) (*Generator, error) {
	modName, err := readModuleName(rootDir)
	if err != nil {
		return nil, fmt.Errorf("read module name: %w", err)
	}
	return &Generator{
		rootDir:    rootDir,
		moduleName: modName,
	}, nil
}

// readModuleName reads the module name from go.mod.
func readModuleName(rootDir string) (string, error) {
	modPath := filepath.Join(rootDir, "go.mod")
	data, err := os.ReadFile(modPath)
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

// PackageInfo holds information about a Go package.
type PackageInfo struct {
	Name       string
	ImportPath string
	Dir        string
	Imports    []string
	Files      []string
	Types      []string
	Functions  []string
}

// Generate generates a Mermaid diagram of the specified type.
func (g *Generator) Generate(diagType DiagramType) (string, error) {
	switch diagType {
	case PackageDeps:
		return g.generatePackageDeps()
	case Architecture:
		return g.generateArchitecture()
	default:
		return "", fmt.Errorf("unknown diagram type: %s", diagType)
	}
}

// scanPackages scans the directory tree for Go packages.
func (g *Generator) scanPackages() (map[string]*PackageInfo, error) {
	pkgs := make(map[string]*PackageInfo)

	err := filepath.Walk(g.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden directories and vendor
		if info.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "testdata" {
				return filepath.SkipDir
			}
		}

		// Only process .go files
		if !info.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			dir := filepath.Dir(path)
			relDir, err := filepath.Rel(g.rootDir, dir)
			if err != nil {
				return err
			}

			importPath := g.moduleName
			if relDir != "." {
				importPath = filepath.Join(g.moduleName, relDir)
			}
			importPath = filepath.ToSlash(importPath)

			if _, ok := pkgs[importPath]; !ok {
				pkgs[importPath] = &PackageInfo{
					ImportPath: importPath,
					Dir:        dir,
				}
			}

			pkg := pkgs[importPath]
			pkg.Files = append(pkg.Files, filepath.Base(path))

			// Parse the file for imports, types, and functions
			if err := g.parseFile(path, pkg); err != nil {
				// Log but don't fail on parse errors
				return nil
			}
		}

		return nil
	})

	return pkgs, err
}

// parseFile parses a Go file and extracts imports, types, and functions.
func (g *Generator) parseFile(path string, pkg *PackageInfo) error {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	// Set package name
	if pkg.Name == "" {
		pkg.Name = node.Name.Name
	}

	// Collect imports
	for _, imp := range node.Imports {
		impPath := strings.Trim(imp.Path.Value, `"`)
		if strings.HasPrefix(impPath, g.moduleName) {
			// Only track internal imports
			found := false
			for _, existing := range pkg.Imports {
				if existing == impPath {
					found = true
					break
				}
			}
			if !found {
				pkg.Imports = append(pkg.Imports, impPath)
			}
		}
	}

	// Collect types and functions
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						found := false
						for _, existing := range pkg.Types {
							if existing == ts.Name.Name {
								found = true
								break
							}
						}
						if !found && ast.IsExported(ts.Name.Name) {
							pkg.Types = append(pkg.Types, ts.Name.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Name != nil && ast.IsExported(d.Name.Name) {
				funcName := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					// Method - skip for now to reduce clutter
					continue
				}
				found := false
				for _, existing := range pkg.Functions {
					if existing == funcName {
						found = true
						break
					}
				}
				if !found {
					pkg.Functions = append(pkg.Functions, funcName)
				}
			}
		}
	}

	return nil
}

// generatePackageDeps generates a package dependency diagram.
func (g *Generator) generatePackageDeps() (string, error) {
	pkgs, err := g.scanPackages()
	if err != nil {
		return "", fmt.Errorf("scan packages: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("```mermaid\ngraph TD\n")
	sb.WriteString("    %% Package Dependency Diagram\n")
	sb.WriteString("    %% Generated by fleetctl --diagram packages\n\n")

	// Create nodes for each package
	sortedPkgs := make([]string, 0, len(pkgs))
	for k := range pkgs {
		sortedPkgs = append(sortedPkgs, k)
	}
	sort.Strings(sortedPkgs)

	// Map import paths to short IDs for mermaid
	idMap := make(map[string]string)
	for i, path := range sortedPkgs {
		id := fmt.Sprintf("pkg%d", i)
		idMap[path] = id
	}

	// Write subgraphs for organization
	cmdPkgs := []string{}
	internalPkgs := []string{}
	rootPkgs := []string{}

	for _, path := range sortedPkgs {
		if strings.Contains(path, "/cmd/") {
			cmdPkgs = append(cmdPkgs, path)
		} else if strings.Contains(path, "/internal/") {
			internalPkgs = append(internalPkgs, path)
		} else {
			rootPkgs = append(rootPkgs, path)
		}
	}

	// Write command packages subgraph
	if len(cmdPkgs) > 0 {
		sb.WriteString("    subgraph CMD[\"Command Layer\"]\n")
		for _, path := range cmdPkgs {
			pkg := pkgs[path]
			label := shortLabel(path)
			sb.WriteString(fmt.Sprintf("        %s[\"%s\"]\n", idMap[path], label))
			if len(pkg.Types) > 0 || len(pkg.Functions) > 0 {
				sb.WriteString(fmt.Sprintf("        %s_note[\"%s\"]\n", idMap[path], pkgNotes(pkg)))
			}
		}
		sb.WriteString("    end\n\n")
	}

	// Write internal packages subgraph
	if len(internalPkgs) > 0 {
		sb.WriteString("    subgraph INTERNAL[\"Internal Packages\"]\n")
		for _, path := range internalPkgs {
			pkg := pkgs[path]
			label := shortLabel(path)
			sb.WriteString(fmt.Sprintf("        %s[\"%s\"]\n", idMap[path], label))
			_ = pkg // Notes omitted for cleaner diagram
		}
		sb.WriteString("    end\n\n")
	}

	// Write root packages
	if len(rootPkgs) > 0 {
		sb.WriteString("    subgraph ROOT[\"Root\"]\n")
		for _, path := range rootPkgs {
			label := shortLabel(path)
			sb.WriteString(fmt.Sprintf("        %s[\"%s\"]\n", idMap[path], label))
		}
		sb.WriteString("    end\n\n")
	}

	// Write dependency edges
	sb.WriteString("    %% Dependencies\n")
	for _, path := range sortedPkgs {
		pkg := pkgs[path]
		for _, imp := range pkg.Imports {
			if targetID, ok := idMap[imp]; ok {
				sb.WriteString(fmt.Sprintf("    %s --> %s\n", idMap[path], targetID))
			}
		}
	}

	sb.WriteString("```\n")
	return sb.String(), nil
}

// generateArchitecture generates a high-level architecture diagram.
func (g *Generator) generateArchitecture() (string, error) {
	pkgs, err := g.scanPackages()
	if err != nil {
		return "", fmt.Errorf("scan packages: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("```mermaid\nflowchart TB\n")
	sb.WriteString("    %% FleetCTL Architecture Diagram\n")
	sb.WriteString("    %% Generated by fleetctl --diagram architecture\n\n")

	// Identify key components
	hasClient := false
	hasFleet := false
	hasConfig := false
	hasState := false
	hasLB := false
	hasMetrics := false
	hasDiagram := false
	hasMain := false

	for path, pkg := range pkgs {
		switch {
		case strings.HasSuffix(path, "/client"):
			hasClient = true
		case strings.HasSuffix(path, "/fleet"):
			hasFleet = true
		case strings.HasSuffix(path, "/config"):
			hasConfig = true
		case strings.HasSuffix(path, "/state"):
			hasState = true
		case strings.HasSuffix(path, "/lb"):
			hasLB = true
		case strings.HasSuffix(path, "/metrics"):
			hasMetrics = true
		case strings.HasSuffix(path, "/diagram"):
			hasDiagram = true
		case strings.Contains(path, "/cmd/"):
			hasMain = true
		}
		_ = pkg
	}

	// External systems
	sb.WriteString("    subgraph EXTERNAL[\"External Systems\"]\n")
	sb.WriteString("        OCI[(\"OCI Cloud\")]\n")
	sb.WriteString("        USER((\"User\"))\n")
	sb.WriteString("    end\n\n")

	// CLI Layer
	sb.WriteString("    subgraph CLI[\"CLI Layer\"]\n")
	if hasMain {
		sb.WriteString("        MAIN[\"cmd/fleetctl<br/>Entry Point\"]\n")
		sb.WriteString("        HTTP[\"HTTP Server<br/>REST API & UI\"]\n")
	}
	sb.WriteString("    end\n\n")

	// Core Layer
	sb.WriteString("    subgraph CORE[\"Core Logic\"]\n")
	if hasFleet {
		sb.WriteString("        FLEET[\"fleet<br/>Fleet Operations\"]\n")
	}
	if hasConfig {
		sb.WriteString("        CONFIG[\"config<br/>YAML Parser\"]\n")
	}
	sb.WriteString("    end\n\n")

	// Infrastructure Layer
	sb.WriteString("    subgraph INFRA[\"Infrastructure\"]\n")
	if hasClient {
		sb.WriteString("        CLIENT[\"client<br/>OCI SDK Wrapper\"]\n")
	}
	if hasLB {
		sb.WriteString("        LB[\"lb<br/>Load Balancer\"]\n")
	}
	sb.WriteString("    end\n\n")

	// Support Layer
	sb.WriteString("    subgraph SUPPORT[\"Support Services\"]\n")
	if hasState {
		sb.WriteString("        STATE[\"state<br/>Local State Store\"]\n")
	}
	if hasMetrics {
		sb.WriteString("        METRICS[\"metrics<br/>Observability\"]\n")
	}
	if hasDiagram {
		sb.WriteString("        DIAGRAM[\"diagram<br/>Code Analysis\"]\n")
	}
	sb.WriteString("    end\n\n")

	// Relationships
	sb.WriteString("    %% Relationships\n")
	sb.WriteString("    USER --> MAIN\n")
	sb.WriteString("    USER --> HTTP\n")
	if hasMain && hasFleet {
		sb.WriteString("    MAIN --> FLEET\n")
		sb.WriteString("    HTTP --> FLEET\n")
	}
	if hasMain && hasConfig {
		sb.WriteString("    MAIN --> CONFIG\n")
	}
	if hasFleet && hasClient {
		sb.WriteString("    FLEET --> CLIENT\n")
	}
	if hasFleet && hasState {
		sb.WriteString("    FLEET --> STATE\n")
	}
	if hasFleet && hasLB {
		sb.WriteString("    FLEET --> LB\n")
	}
	if hasFleet && hasMetrics {
		sb.WriteString("    FLEET --> METRICS\n")
	}
	if hasClient {
		sb.WriteString("    CLIENT --> OCI\n")
	}
	if hasLB {
		sb.WriteString("    LB --> OCI\n")
	}
	if hasMain && hasDiagram {
		sb.WriteString("    MAIN --> DIAGRAM\n")
	}

	// Styling
	sb.WriteString("\n    %% Styling\n")
	sb.WriteString("    classDef external fill:#f9f,stroke:#333,stroke-width:2px\n")
	sb.WriteString("    classDef cli fill:#bbf,stroke:#333,stroke-width:2px\n")
	sb.WriteString("    classDef core fill:#bfb,stroke:#333,stroke-width:2px\n")
	sb.WriteString("    classDef infra fill:#fbb,stroke:#333,stroke-width:2px\n")
	sb.WriteString("    classDef support fill:#ff9,stroke:#333,stroke-width:2px\n")
	sb.WriteString("    class OCI,USER external\n")
	if hasMain {
		sb.WriteString("    class MAIN,HTTP cli\n")
	}
	if hasFleet || hasConfig {
		parts := []string{}
		if hasFleet {
			parts = append(parts, "FLEET")
		}
		if hasConfig {
			parts = append(parts, "CONFIG")
		}
		sb.WriteString(fmt.Sprintf("    class %s core\n", strings.Join(parts, ",")))
	}
	if hasClient || hasLB {
		parts := []string{}
		if hasClient {
			parts = append(parts, "CLIENT")
		}
		if hasLB {
			parts = append(parts, "LB")
		}
		sb.WriteString(fmt.Sprintf("    class %s infra\n", strings.Join(parts, ",")))
	}
	if hasState || hasMetrics || hasDiagram {
		parts := []string{}
		if hasState {
			parts = append(parts, "STATE")
		}
		if hasMetrics {
			parts = append(parts, "METRICS")
		}
		if hasDiagram {
			parts = append(parts, "DIAGRAM")
		}
		sb.WriteString(fmt.Sprintf("    class %s support\n", strings.Join(parts, ",")))
	}

	sb.WriteString("```\n")
	return sb.String(), nil
}

// shortLabel returns a short label for a package path.
func shortLabel(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return path
}

// pkgNotes returns a brief description of package contents.
func pkgNotes(pkg *PackageInfo) string {
	var parts []string
	if len(pkg.Types) > 0 {
		if len(pkg.Types) <= 3 {
			parts = append(parts, strings.Join(pkg.Types, ", "))
		} else {
			parts = append(parts, fmt.Sprintf("%d types", len(pkg.Types)))
		}
	}
	if len(pkg.Functions) > 0 {
		if len(pkg.Functions) <= 3 {
			parts = append(parts, strings.Join(pkg.Functions, ", "))
		} else {
			parts = append(parts, fmt.Sprintf("%d funcs", len(pkg.Functions)))
		}
	}
	return strings.Join(parts, "<br/>")
}

// AvailableDiagramTypes returns a list of available diagram types.
func AvailableDiagramTypes() []DiagramType {
	return []DiagramType{PackageDeps, Architecture}
}
