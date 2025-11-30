// cmd/fleetctl/main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fleetctl/internal/client"
	"fleetctl/internal/config"
	"fleetctl/internal/diagram"
	"fleetctl/internal/fleet"
	"fleetctl/internal/metrics"
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
	flagReconcileEvery time.Duration
	flagDiagram        string
)

// controlStatus tracks the background control loop state for diagnostics.
type controlStatus struct {
	mu               sync.RWMutex
	Enabled          bool
	Interval         string
	LastTick         time.Time
	LastConfigReload *time.Time
	Desired          int
	Actual           int
	LastAction       string
	LastError        string
	LoopCount        int
}

func (c *controlStatus) set(update func(*controlStatus)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	update(c)
}

func (c *controlStatus) snapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var lcr string
	if c.LastConfigReload != nil {
		lcr = c.LastConfigReload.Format(time.RFC3339)
	}
	return map[string]any{
		"enabled":          c.Enabled,
		"interval":         c.Interval,
		"lastTick":         c.LastTick.Format(time.RFC3339),
		"lastConfigReload": lcr,
		"desired":          c.Desired,
		"actual":           c.Actual,
		"lastAction":       c.LastAction,
		"lastError":        c.LastError,
		"loopCount":        c.LoopCount,
	}
}

var ctrlStatus controlStatus

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
	flag.DurationVar(&flagReconcileEvery, "reconcile-every", 30*time.Second, "Background reconcile interval for --http mode (e.g., 30s, 1m)")
	flag.StringVar(&flagDiagram, "diagram", "", "Generate Mermaid diagram (packages, architecture)")

	// Custom usage printer
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "fleetctl %s\n\nUsage:\n  %s [flags]\n\nRequires: --config plus at least one additional flag, or --diagram, or --version\n\nFlags:\n", version, os.Args[0])
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()

	if flagVersion {
		fmt.Println(version)
		return
	}

	// Handle --diagram flag (standalone, doesn't require --config)
	if flagDiagram != "" {
		// Determine project root (directory containing go.mod)
		rootDir, err := findProjectRoot()
		if err != nil {
			log.Fatalf("find project root: %v", err)
		}

		gen, err := diagram.NewGenerator(rootDir)
		if err != nil {
			log.Fatalf("init diagram generator: %v", err)
		}

		var diagType diagram.DiagramType
		switch strings.ToLower(flagDiagram) {
		case "packages", "package", "pkg", "deps":
			diagType = diagram.PackageDeps
		case "architecture", "arch", "overview":
			diagType = diagram.Architecture
		default:
			fmt.Fprintf(os.Stderr, "Unknown diagram type: %s\n", flagDiagram)
			fmt.Fprintf(os.Stderr, "Available types: packages, architecture\n")
			os.Exit(1)
		}

		output, err := gen.Generate(diagType)
		if err != nil {
			log.Fatalf("generate diagram: %v", err)
		}
		fmt.Print(output)
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
		// Normalize address: allow bare port like "8080" by prefixing with ":"
		addr := flagHTTP
		if !strings.Contains(addr, ":") {
			addr = ":" + addr
		}
		log.Printf("Starting control loop every %s (config: %s)", flagReconcileEvery, flagConfig)
		startControlLoop(f, flagConfig, flagReconcileEvery)
		log.Printf("Starting HTTP server on %s", addr)
		if err := startHTTPServer(f, st, cfg, addr); err != nil {
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
		// use control loop snapshot to avoid concurrent SDK calls (prevents race)
		cs := ctrlStatus.snapshot()
		remoteActive := 0
		if v, ok := cs["actual"].(int); ok {
			remoteActive = v
		} else if df, ok := cs["actual"].(float64); ok {
			remoteActive = int(df)
		}
		var lbSnapshot any
		if lb, ok, _ := st.GetLBInfo(cfg.Metadata.Name); ok {
			lbSnapshot = map[string]any{
				"enabled":       lb.Enabled,
				"id":            lb.ID,
				"backendSet":    lb.BackendSet,
				"listener":      lb.Listener,
				"backends":      lb.Backends,
				"backendsCount": lb.BackendsCount,
				"updatedAt":     lb.UpdatedAt.Format(time.RFC3339),
			}
		}
		resp := map[string]any{
			"fleet":        cfg.Metadata.Name,
			"localActive":  localActive,
			"remoteActive": remoteActive,
			"timestamp":    time.Now().Format(time.RFC3339),
			"control":      ctrlStatus.snapshot(),
			"actions":      metrics.Snapshot(),
			"lb":           lbSnapshot,
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
		desired := body.Desired
		// Enqueue requested desired to show in Scale queue badge immediately.
		// Do not override current scaling badge; it should reflect the active operation.
		localActive, _ := st.CountActive(cfg.Metadata.Name)
		if desired != localActive {
			metrics.AppendScaleQueue(desired)
		}
		go func(d int) {
			if err := f.Scale(d); err != nil {
				log.Printf("scale failed (async): %v", err)
			}
		}(desired)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("scale accepted"))
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

	// Emit control loop status
	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ctrlStatus.snapshot())
	})

	// Server-Sent Events for live updates
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ctx := r.Context()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// initial ping to open the stream in some proxies/browsers
		fmt.Fprintf(w, ": ping\n\n")
		fl.Flush()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// control snapshot first (authoritative desired from control loop that reloads config)
				ctrl := ctrlStatus.snapshot()
				desired := 0
				if dv, ok := ctrl["desired"].(int); ok {
					desired = dv
				} else if df, ok := ctrl["desired"].(float64); ok {
					desired = int(df)
				}

				// compute local/remote
				localActive, _ := st.CountActive(cfg.Metadata.Name)
				remoteActive := 0
				if v, ok := ctrl["actual"].(int); ok {
					remoteActive = v
				} else if df, ok := ctrl["actual"].(float64); ok {
					remoteActive = int(df)
				}

				// status HTML (Desired vs Remote vs Local)
				statusHTML := fmt.Sprintf(
					"<div class='grid3'><div><div class='label'>Desired</div><div class='value'>%d</div></div><div><div class='label'>Remote</div><div class='value'>%d</div></div><div><div class='label'>Local</div><div class='value'>%d</div></div></div>",
					desired, remoteActive, localActive,
				)
				// fleet name header
				fleetNameHTML := fmt.Sprintf("<div class='fleet-name'>Fleet: <code>%s</code></div>", html.EscapeString(cfg.Metadata.Name))
				// minimums from config (sum and per-group)
				minTotal := 0
				var groupParts []string
				for _, g := range f.Config.Spec.Instances {
					minTotal += g.Count
					if g.Count > 0 {
						name := g.Name
						if name == "" {
							name = "group"
						}
						groupParts = append(groupParts, fmt.Sprintf("%s:%d", name, g.Count))
					}
				}
				minimumsHTML := fmt.Sprintf("<div class='minimums'><div class='label'>Config file Minimums</div><div class='value'>%d</div><div class='groups'>%s</div></div>", minTotal, html.EscapeString(strings.Join(groupParts, " ")))

				// metrics HTML
				act := metrics.Snapshot()
				actJSON, _ := json.MarshalIndent(act, "", "  ")
				metricsHTML := "<pre>" + string(actJSON) + "</pre>"

				// Build Scale Queue badge from actions metrics
				scaleQueue := []int{}
				if arr, ok := act["scaleQueue"].([]int); ok {
					scaleQueue = append(scaleQueue, arr...)
				} else if arrAny, ok := act["scaleQueue"].([]any); ok {
					for _, it := range arrAny {
						switch tv := it.(type) {
						case int:
							scaleQueue = append(scaleQueue, tv)
						case float64:
							scaleQueue = append(scaleQueue, int(tv))
						}
					}
				}
				vals := []string{}
				for _, x := range scaleQueue {
					vals = append(vals, fmt.Sprintf("%d", x))
				}
				valStr := "empty"
				if len(vals) > 0 {
					valStr = strings.Join(vals, ", ")
				}
				scaleQueueBadgeHTML := fmt.Sprintf("<div class='badge scalequeue-badge'>Scale queue: %s</div>", html.EscapeString(valStr))

				// LB badge HTML derived from metrics
				lbEnabled := false
				lbBackends := 0
				if v, ok := act["lbEnabled"].(bool); ok {
					lbEnabled = v
				}
				if v, ok := act["lbBackends"].(int); ok {
					lbBackends = v
				} else if df, ok := act["lbBackends"].(float64); ok {
					lbBackends = int(df)
				}
				lbBadgeHTML := ""
				if lbEnabled {
					lbBadgeHTML = fmt.Sprintf("<div class='badge lb lb-enabled'>LB %d backends</div>", lbBackends)
				} else {
					lbBadgeHTML = "<div class='badge lb lb-disabled'>LB disabled</div>"
				}
				// Scaling badge HTML derived from metrics.operation
				op := ""
				if v, ok := act["operation"].(string); ok {
					op = v
				}
				phase := ""
				if v, ok := act["phase"].(string); ok {
					phase = v
				}
				statusSuffix := ""
				if phase != "" && strings.ToLower(phase) != "done" {
					statusSuffix = " - " + html.EscapeString(phase)
				}
				// extract scale context and progress
				startTotal := 0
				if v, ok := act["startTotal"].(int); ok {
					startTotal = v
				} else if df, ok := act["startTotal"].(float64); ok {
					startTotal = int(df)
				}
				targetTotal := 0
				if v, ok := act["targetTotal"].(int); ok {
					targetTotal = v
				} else if df, ok := act["targetTotal"].(float64); ok {
					targetTotal = int(df)
				}
				launchSucceeded := 0
				if v, ok := act["launchSucceeded"].(int); ok {
					launchSucceeded = v
				} else if df, ok := act["launchSucceeded"].(float64); ok {
					launchSucceeded = int(df)
				}
				terminateSucceeded := 0
				if v, ok := act["terminateSucceeded"].(int); ok {
					terminateSucceeded = v
				} else if df, ok := act["terminateSucceeded"].(float64); ok {
					terminateSucceeded = int(df)
				}
				delta := 0
				if targetTotal >= startTotal {
					delta = targetTotal - startTotal
				} else {
					delta = startTotal - targetTotal
				}
				scaleBadgeHTML := ""
				switch strings.ToLower(op) {
				case "scale-up":
					x := launchSucceeded
					y := delta
					if y > 0 {
						scaleBadgeHTML = fmt.Sprintf("<div class='badge scale scale-up'>Scaling up to %d (%d of %d)%s</div>", targetTotal, x, y, statusSuffix)
					} else {
						scaleBadgeHTML = fmt.Sprintf("<div class='badge scale scale-up'>Scaling up to %d%s</div>", targetTotal, statusSuffix)
					}
				case "scale-down":
					x := terminateSucceeded
					y := delta
					if y > 0 {
						scaleBadgeHTML = fmt.Sprintf("<div class='badge scale scale-down'>Scaling down to %d (%d of %d)%s</div>", targetTotal, x, y, statusSuffix)
					} else {
						scaleBadgeHTML = fmt.Sprintf("<div class='badge scale scale-down'>Scaling down to %d%s</div>", targetTotal, statusSuffix)
					}
				default:
					scaleBadgeHTML = "<div class='badge scale scale-idle'>Scaling idle</div>"
				}

				// Rolling restart badge HTML derived from metrics during active operation
				rrBadgeHTML := ""
				if strings.ToLower(op) == "rolling-restart" {
					idx := 0
					total := 0
					if v, ok := act["rollingRestartIndex"].(int); ok {
						idx = v
					} else if df, ok := act["rollingRestartIndex"].(float64); ok {
						idx = int(df)
					}
					if v, ok := act["rollingRestartTotal"].(int); ok {
						total = v
					} else if df, ok := act["rollingRestartTotal"].(float64); ok {
						total = int(df)
					}
					if total > 0 {
						rrBadgeHTML = fmt.Sprintf("<div class='badge rr rr-in-progress'>Rolling restart %d/%d</div>", idx, total)
					} else {
						rrBadgeHTML = "<div class='badge rr rr-in-progress'>Rolling restart</div>"
					}
				}

				// Drift badge HTML comparing desired vs remote actual
				driftBadgeHTML := ""
				if remoteActive == desired {
					driftBadgeHTML = "<div class='badge drift drift-ok'>No drift</div>"
				} else {
					driftBadgeHTML = "<div class='badge drift drift-alert'>Drift detected</div>"
				}

				// control HTML
				ctrlJSON, _ := json.MarshalIndent(ctrl, "", "  ")
				controlHTML := "<pre>" + string(ctrlJSON) + "</pre>"

				// send SSE events for htmx-sse swapping (prefix each line with data:)
				writeEvent := func(name, data string) {
					fmt.Fprintf(w, "event: %s\n", name)
					for _, ln := range strings.Split(data, "\n") {
						fmt.Fprintf(w, "data: %s\n", ln)
					}
					fmt.Fprint(w, "\n")
				}
				writeEvent("status", fleetNameHTML+lbBadgeHTML+scaleBadgeHTML+rrBadgeHTML+driftBadgeHTML+scaleQueueBadgeHTML+minimumsHTML+statusHTML)
				writeEvent("metrics", metricsHTML)
				writeEvent("control", controlHTML)
				fl.Flush()
			}
		}
	})

	// Serve OpenAPI spec and basic UI
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAPISpecJSON()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(uiPageHTML()))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return server.ListenAndServe()
}

func startControlLoop(f *fleet.Fleet, cfgPath string, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		var lastMod time.Time

		ctrlStatus.set(func(c *controlStatus) {
			c.Enabled = true
			c.Interval = every.String()
			c.LastError = ""
		})

		for {
			ctrlStatus.set(func(c *controlStatus) {
				c.LastTick = time.Now()
				c.LoopCount++
			})
			// 1) Reload config if modified
			if fi, err := os.Stat(cfgPath); err == nil {
				if fi.ModTime().After(lastMod) {
					if newCfg, err := config.ParseFile(cfgPath); err == nil {
						f.Config = *newCfg
						lastMod = fi.ModTime()
						t := fi.ModTime()
						tCopy := t
						ctrlStatus.set(func(c *controlStatus) {
							c.LastConfigReload = &tCopy
							c.LastError = ""
						})
						log.Printf("control: reloaded config (modified %s)", fi.ModTime().Format(time.RFC3339))
					} else {
						log.Printf("control: parse config error: %v", err)
					}
				}
			} else {
				ctrlStatus.set(func(c *controlStatus) { c.LastError = err.Error() })
				log.Printf("control: stat config error: %v", err)
			}

			// 2) Determine desired total from config, then apply lower-bound from local state (only scale up)
			desired := 0
			for _, g := range f.Config.Spec.Instances {
				desired += g.Count
			}
			// baseline from local state: do not go below what's tracked locally
			target := desired
			if f.Store != nil {
				if la, err := f.Store.CountActive(f.Config.Metadata.Name); err == nil && la > target {
					target = la
				}
			}
			ctrlStatus.set(func(c *controlStatus) { c.Desired = target })

			// 3) Compare actual vs desired and reconcile if needed
			if f.Client != nil {
				inst, err := f.Client.ListInstancesByFleet(context.Background(), f.Config.Spec.CompartmentID, f.Config.Metadata.Name)
				if err != nil {
					ctrlStatus.set(func(c *controlStatus) { c.LastError = err.Error() })
					log.Printf("control: list instances error: %v", err)
				} else {
					actual := len(inst)
					ctrlStatus.set(func(c *controlStatus) {
						c.Actual = actual
						c.LastError = ""
					})
					if actual < target {
						ctrlStatus.set(func(c *controlStatus) { c.LastAction = fmt.Sprintf("scale up to %d", target) })
						log.Printf("control: scaling up to meet target; target=%d actual=%d", target, actual)
						if err := f.Scale(target); err != nil {
							ctrlStatus.set(func(c *controlStatus) { c.LastError = err.Error() })
							log.Printf("control: scale up to %d failed: %v", target, err)
						}
					} else {
						ctrlStatus.set(func(c *controlStatus) { c.LastAction = "noop" })
						log.Printf("control: actual (%d) meets or exceeds target (%d); no downscale", actual, target)
					}
				}
			}

			// 4) Load balancer reconcile every tick
			if f.Client != nil {
				ctrlStatus.set(func(c *controlStatus) { c.LastAction = "lb-reconcile" })
				if err := f.ReconcileLoadBalancer(context.Background()); err != nil {
					ctrlStatus.set(func(c *controlStatus) { c.LastError = err.Error() })
					log.Printf("control: lb reconcile error: %v", err)
				} else {
					ctrlStatus.set(func(c *controlStatus) { c.LastError = "" })
				}
			}

			<-ticker.C
		}
	}()
}

// openAPISpecJSON returns the OpenAPI 3.0 definition for the HTTP API.
func openAPISpecJSON() string {
	return `{
  "openapi": "3.0.0",
  "info": {
    "title": "fleetctl API",
    "version": "0.1.0",
    "description": "HTTP API for fleetctl daemon: health, status, metrics, and control operations"
  },
  "paths": {
    "/healthz": {
      "get": {
        "summary": "Liveness probe",
        "responses": {
          "200": { "description": "OK", "content": { "text/plain": { } } }
        }
      }
    },
    "/status": {
      "get": {
        "summary": "Local vs Remote (OCI) status",
        "responses": {
          "200": { "description": "Status text", "content": { "text/plain": { } } },
          "500": { "description": "Error", "content": { "text/plain": { } } }
        }
      }
    },
    "/metrics": {
      "get": {
        "summary": "Metrics JSON",
        "responses": {
          "200": {
            "description": "Metrics",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "fleet": { "type": "string" },
                    "localActive": { "type": "integer" },
                    "remoteActive": { "type": "integer" },
                    "timestamp": { "type": "string", "format": "date-time" },
                    "control": { "type": "object" },
                    "actions": { "type": "object" }
                  },
                  "required": ["fleet","localActive","remoteActive","timestamp"]
                }
              }
            }
          },
          "500": { "description": "Error", "content": { "application/json": { } } }
        }
      }
    },
    "/scale": {
      "post": {
        "summary": "Scale fleet to desired total",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": { "desired": { "type": "integer", "minimum": 0 } },
                "required": ["desired"]
              }
            }
          }
        },
        "responses": {
          "202": { "description": "Accepted - scaling in background", "content": { "text/plain": { } } },
          "400": { "description": "Bad request", "content": { "text/plain": { } } },
          "500": { "description": "Error", "content": { "text/plain": { } } }
        }
      }
    },
    "/rolling-restart": {
      "post": {
        "summary": "Serial rolling restart",
        "responses": {
          "200": { "description": "OK", "content": { "text/plain": { } } },
          "500": { "description": "Error", "content": { "text/plain": { } } }
        }
      }
    },
    "/sync-state": {
      "post": {
        "summary": "Rebuild local state from OCI",
        "responses": {
          "200": { "description": "OK", "content": { "text/plain": { } } },
          "500": { "description": "Error", "content": { "text/plain": { } } }
        }
      }
    },
    "/openapi.json": {
      "get": {
        "summary": "OpenAPI specification",
        "responses": {
          "200": { "description": "OpenAPI JSON", "content": { "application/json": { } } }
        }
      }
    },
    "/control": {
      "get": {
        "summary": "Control loop status",
        "responses": {
          "200": { "description": "JSON status", "content": { "application/json": { } } }
        }
      }
    }
  }
}`
}

// uiPageHTML returns a minimal interactive UI for status and control.
func uiPageHTML() string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>fleetctl UI</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<script src="https://unpkg.com/htmx.org@1.9.12"></script>
<script src="https://unpkg.com/htmx.org/dist/ext/sse.js"></script>
<style>
:root { --border:#e5e7eb; --muted:#666; --bg:#fff; --chip:#f3f4f6;}
body { font-family: system-ui, -apple-system, Segoe UI, Roboto, Arial, sans-serif; margin: 20px; color: #111; background: var(--bg); }
h1 { margin: 0 0 12px 0; }
h2 { margin: 0 0 8px 0; font-size: 1.1rem; }
section { margin-bottom: 20px; padding: 12px; border: 1px solid var(--border); border-radius: 8px; }
.grid3 { display: grid; grid-template-columns: repeat(3, 1fr); gap: 12px; }
.label { font-size: 0.8rem; color: var(--muted); }
.value { font-size: 1.6rem; font-weight: 600; }
.controls { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
input[type=number] { width: 120px; padding: 6px; }
button, .btn { padding: 6px 12px; border: 1px solid var(--border); background:#fff; border-radius:6px; cursor:pointer;}
button:hover, .btn:hover { background: var(--chip); }
pre { background: #f7f7f7; padding: 12px; overflow: auto; border-radius: 6px; }
.kv { display: grid; grid-template-columns: 160px 1fr; gap: 6px; }
.ts { font-size: 0.8rem; color: var(--muted); }
.fleet-name { margin-bottom:8px; font-size:0.95rem; }
.badge { display:inline-block; padding:4px 8px; border-radius:9999px; font-size:0.8rem; margin-top:8px; border:1px solid var(--border); }
.badge.lb.lb-enabled { background:#e6ffed; color:#055e11; border-color:#a7f3d0; }
.badge.lb.lb-disabled { background:#fff7ed; color:#9a3412; border-color:#fed7aa; }
.badge.scale.scale-up { background:#e6ffed; color:#065f46; border-color:#a7f3d0; }
.badge.scale.scale-down { background:#fef3c7; color:#92400e; border-color:#fcd34d; }
.badge.scale.scale-idle { background:#f3f4f6; color:#374151; border-color:#e5e7eb; }
.badge.rr.rr-in-progress { background:#dbeafe; color:#1e40af; border-color:#bfdbfe; }
.badge.drift.drift-ok { background:#e6ffed; color:#065f46; border-color:#a7f3d0; }
.badge.drift.drift-alert { background:#fee2e2; color:#991b1b; border-color:#fecaca; }
.minimums { margin-top:8px; padding:8px; background: var(--chip); border:1px solid var(--border); border-radius:6px; }
.minimums .groups { font-size:0.8rem; color: var(--muted); margin-top:4px; }
.badge.scalequeue-badge { background:#f3f4f6; color:#374151; border-color:#e5e7eb; margin-left:8px; }
</style>
</head>
<body hx-ext="sse" sse-connect="/events">
<h1>fleetctl UI</h1>

<section>
  <h2>Fleet Status</h2>
  <div id="status-panel" sse-swap="status"></div>
  <div class="ts">Live via SSE</div>
</section>

<section>
  <h2>Operation Metrics</h2>
  <div id="metrics-panel" sse-swap="metrics"></div>
</section>

<section>
  <h2>Control Loop</h2>
  <div id="control-panel" sse-swap="control"></div>
</section>

<section>
  <h2>Controls</h2>
  <div class="controls">
    <label for="desired">Desired total:</label>
    <input id="desired" type="number" min="0" value="0">
    <a class="btn" href="#" onclick="scale()">Scale</a>
    <a class="btn" href="#" onclick="rollingRestart()">Rolling Restart</a>
    <a class="btn" href="#" onclick="syncState()">Sync State</a>
    <a class="btn" href="/openapi.json" target="_blank">OpenAPI JSON</a>
  </div>
</section>

<script>
async function scale() {
  const d = parseInt(document.getElementById('desired').value, 10) || 0;
  const res = await fetch('/scale', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ desired: d })
  });
  alert(await res.text());
}
async function rollingRestart() {
  const res = await fetch('/rolling-restart', { method: 'POST' });
  alert(await res.text());
}
async function syncState() {
  const res = await fetch('/sync-state', { method: 'POST' });
  alert(await res.text());
}
</script>
</body>
</html>`
}

// findProjectRoot walks up from the current working directory to find go.mod.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	for {
		modPath := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(modPath); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent directory")
		}
		dir = parent
	}
}
