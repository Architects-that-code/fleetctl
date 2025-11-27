// cmd/fleetctl/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"fleetctl/internal/client"
	"fleetctl/internal/config"
	"fleetctl/internal/fleet"
	"fleetctl/internal/state"
)

const version = "0.1.0"

var (
	flagConfig         string
	flagScale          int
	flagRollingRestart bool
	flagVersion        bool
	flagStatus         bool
	flagState          string
	flagAuthValidate   bool
	flagSyncState      bool
)

func init() {
	flag.StringVar(&flagConfig, "config", "fleet.yaml", "Path to fleet configuration file")
	flag.IntVar(&flagScale, "scale", -1, "Scale fleet to desired total number of instances")
	flag.BoolVar(&flagRollingRestart, "rolling-restart", false, "Perform a rolling restart of the fleet")
	flag.BoolVar(&flagVersion, "version", false, "Print version and exit")
	flag.BoolVar(&flagStatus, "status", false, "Print tracked fleet state from local store")
	flag.StringVar(&flagState, "state", ".fleetctl/state.json", "Path to local state JSON for tracking launched instances")
	flag.BoolVar(&flagAuthValidate, "auth-validate", false, "Validate OCI authentication by performing a lightweight API call")
	flag.BoolVar(&flagSyncState, "sync-state", false, "Rebuild local state by querying OCI for instances tagged to this fleet")

	// Custom usage printer
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "fleetctl %s\n\nUsage:\n  %s [flags]\n\nRequires: --config plus at least one additional flag\n\nFlags:\n", version, os.Args[0])
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()

	if flagVersion {
		fmt.Println(version)
		return
	}

	// Require at least two flags to be provided, and one must be --config.
	// This enforces usage like: --config <file> plus one action flag (e.g., --auth-validate, --status, --scale, --rolling-restart).
	var visitedCount int
	hasConfig := false
	flag.Visit(func(f *flag.Flag) {
		visitedCount++
		if f.Name == "config" {
			hasConfig = true
		}
	})
	if !(hasConfig && visitedCount >= 2) {
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := config.ParseFile(flagConfig)
	if err != nil {
		log.Fatalf("failed to load configuration from %s: %v", flagConfig, err)
	}

	// Resolve default state path to be alongside the config file and reflect the fleet name, unless overridden.
	statePath := flagState
	cfgDir := filepath.Dir(flagConfig)
	if statePath == ".fleetctl/state.json" {
		base := fmt.Sprintf(".%s.state.json", cfg.Metadata.Name)
		statePath = filepath.Join(cfgDir, base)
	}
	st := state.New(statePath)
	f := fleet.New(*cfg, nil, st)

	switch {
	case flagSyncState:
		stubClient, err := client.New(cfg.Spec.Auth)
		if err != nil {
			log.Fatalf("init OCI client: %v", err)
		}
		f.Client = stubClient
		if err := f.SyncState(); err != nil {
			log.Fatalf("sync-state failed: %v", err)
		}
		summary, err := st.Summary(cfg.Metadata.Name)
		if err != nil {
			log.Fatalf("status after sync failed: %v", err)
		}
		fmt.Println(summary)
	case flagAuthValidate:
		cli, err := client.New(cfg.Spec.Auth)
		if err != nil {
			log.Fatalf("init OCI client: %v", err)
		}
		info, err := cli.ValidateInfo(context.Background())
		if err != nil {
			log.Fatalf("auth validation failed: %v", err)
		}
		fmt.Printf("Auth validation succeeded\n")
		fmt.Printf("  region: %s\n", info.Region)
		if info.TenancyOCID != "" {
			fmt.Printf("  tenancy: %s\n", info.TenancyOCID)
		}
		if info.UserOCID != "" {
			fmt.Printf("  user: %s\n", info.UserOCID)
		} else {
			fmt.Printf("  user: (instance principal)\n")
		}
		fmt.Printf("  regions_available: %d\n", info.RegionsCount)
		if len(info.SubscribedRegions) > 0 {
			fmt.Printf("  subscriptions: %s\n", strings.Join(info.SubscribedRegions, ","))
		}
	case flagScale >= 0:
		if f.Client == nil {
			cli, err := client.New(cfg.Spec.Auth)
			if err != nil {
				log.Fatalf("init OCI client: %v", err)
			}
			f.Client = cli
		}
		if err := f.Scale(flagScale); err != nil {
			log.Fatalf("scale failed: %v", err)
		}
	case flagRollingRestart:
		if f.Client == nil {
			cli, err := client.New(cfg.Spec.Auth)
			if err != nil {
				log.Fatalf("init OCI client: %v", err)
			}
			f.Client = cli
		}
		if err := f.RollingRestart(); err != nil {
			log.Fatalf("rolling restart failed: %v", err)
		}
	case flagStatus:
		// Ensure OCI client available for remote status
		if f.Client == nil {
			cli, err := client.New(cfg.Spec.Auth)
			if err != nil {
				log.Fatalf("init OCI client: %v", err)
			}
			f.Client = cli
		}
		// Use Fleet.StatusCompare to print clearly labeled local vs remote sections
		out, err := f.StatusCompare()
		if err != nil {
			log.Fatalf("status failed: %v", err)
		}
		fmt.Println(out)
	default:
		// If only --config (or other non-action flags) are provided, print a summary by default.
		fmt.Println(f.Summary())
	}
}
