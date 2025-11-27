// cmd/fleetctl/main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	flagHTTP           string
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
	flag.StringVar(&flagHTTP, "http", "", "Listen address for HTTP API (e.g., :8080). Serves /healthz, /status, /metrics and command endpoints.")

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
	case flagHTTP != "":
		// Initialize OCI client for remote operations
		if f.Client == nil {
			cli, err := client.New(cfg.Spec.Auth)
			if err != nil {
				log.Fatalf("init OCI client: %v", err)
			}
			f.Client = cli
		}
		log.Printf("Starting HTTP server on %s", flagHTTP)
		if err := startHTTPServer(f, st, cfg, flagHTTP); err != nil {
			log.Fatalf("http server error: %v", err)
		}
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

// startHTTPServer serves health, metrics, status and control endpoints.
func startHTTPServer(f *fleet.Fleet, st *state.Store, cfg *config.FleetConfig, addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		out, err := f.StatusCompare()
		if err != nil {
			http.Error(w, fmt.Sprintf("status error: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(out))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Simple JSON metrics for now
		localActive, _ := st.CountActive(cfg.Metadata.Name)
		remoteActive := 0
		if f.Client != nil {
			if inst, err := f.Client.ListInstancesByFleet(r.Context(), cfg.Spec.CompartmentID, cfg.Metadata.Name); err == nil {
				remoteActive = len(inst)
			}
		}
		resp := map[string]any{
			"fleet":        cfg.Metadata.Name,
			"localActive":  localActive,
			"remoteActive": remoteActive,
			"timestamp":    time.Now().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/scale", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Desired int `json:"desired"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.Desired < 0 {
			http.Error(w, "desired must be >= 0", http.StatusBadRequest)
			return
		}
		if err := f.Scale(body.Desired); err != nil {
			http.Error(w, fmt.Sprintf("scale failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("scale OK"))
	})

	mux.HandleFunc("/rolling-restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := f.RollingRestart(); err != nil {
			http.Error(w, fmt.Sprintf("rolling restart failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("rolling-restart OK"))
	})

	mux.HandleFunc("/sync-state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := f.SyncState(); err != nil {
			http.Error(w, fmt.Sprintf("sync-state failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("sync-state OK"))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return server.ListenAndServe()
}
