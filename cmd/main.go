package main

import (
	"encoding/json"
	_ "embed"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boettiger-lab/carbon-api/internal/scraper"
)

//go:embed static/dashboard.html
var dashboardHTML []byte

//go:embed static/methodology.html
var methodologyHTML []byte

var s *scraper.Scraper

func main() {
	promURL := getenv("PROMETHEUS_URL", "https://prometheus.nrp-nautilus.io")
	interval := getenvDuration("SCRAPE_INTERVAL", 30*time.Second)
	addr := getenv("LISTEN_ADDR", ":8080")

	s = scraper.New(promURL, interval)
	go s.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard)
	mux.HandleFunc("/methodology", handleMethodology)
	mux.HandleFunc("/api/v1/carbon", handleModels)
	mux.HandleFunc("/api/v1/carbon/timeseries", handleTimeSeries)
	mux.HandleFunc("/api/v1/carbon/", handleSeries) // /api/v1/carbon/{ns}/{container}/{metric}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	log.Printf("carbon-api listening on %s (prometheus=%s, interval=%s)", addr, promURL, interval)
	// Timeouts so slow clients cannot hold connections open indefinitely.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// Ranges the timeseries endpoint serves, with the step each one uses. Each
// request runs two range queries against NRP's shared Prometheus, so the
// endpoint takes only the ranges the dashboard offers, and each answer is
// cached for one step: a new point cannot exist before then.
var timeSeriesRanges = map[string]struct{ rng, step time.Duration }{
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, time.Hour},
	"30d": {30 * 24 * time.Hour, 6 * time.Hour},
}

type tsCacheEntry struct {
	at   time.Time
	body []byte
}

var (
	tsMu    sync.Mutex // held across the fetch, so concurrent misses make one query
	tsCache = map[string]tsCacheEntry{}
)

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

func handleMethodology(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(methodologyHTML)
}

// GET /api/v1/carbon/timeseries?range=24h|7d|30d
// Returns aggregated cluster-wide CO2 and power time series from Prometheus history.
func handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "24h"
	}
	spec, ok := timeSeriesRanges[key]
	if !ok {
		http.Error(w, "range must be one of 24h, 7d, 30d", http.StatusBadRequest)
		return
	}

	tsMu.Lock()
	entry, hit := tsCache[key]
	if !hit || time.Since(entry.at) >= spec.step {
		pts, err := s.ClusterTimeSeries(spec.rng, spec.step)
		errMsg := ""
		if err != nil {
			// Log the detail; callers get a generic message.
			log.Printf("timeseries %s: %v", key, err)
			errMsg = "upstream query failed"
		}
		body, _ := json.Marshal(map[string]interface{}{
			"range":  spec.rng.String(),
			"step":   spec.step.String(),
			"points": pts,
			"error":  errMsg,
		})
		entry = tsCacheEntry{at: time.Now(), body: body}
		if err == nil {
			tsCache[key] = entry
		}
	}
	tsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(entry.body)
	w.Write([]byte("\n"))
}

// GET /api/v1/carbon
// Returns current metrics for all models, sorted by CO2/hr descending.
func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	models := s.Models()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models":     models,
		"updated_at": time.Now(),
	})
}

// GET /api/v1/carbon/{namespace}/{container}/{metric}?range=1h|6h|24h|48h
// metric: power_watts | co2_grams_per_hour | co2_mg_per_token
func handleSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// strip prefix /api/v1/carbon/
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/carbon/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 {
		http.Error(w, "usage: /api/v1/carbon/{namespace}/{container}/{metric}", http.StatusBadRequest)
		return
	}
	ns, container, metric := parts[0], parts[1], parts[2]

	rangeStr := r.URL.Query().Get("range")
	rangeDur := parseDuration(rangeStr, 24*time.Hour)
	// Served from memory, which holds at most 7 days; a longer range adds nothing.
	if rangeDur <= 0 || rangeDur > 7*24*time.Hour {
		rangeDur = 7 * 24 * time.Hour
	}

	series := s.Series(ns, container, metric, rangeDur)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"namespace": ns,
		"container": container,
		"metric":    metric,
		"range":     rangeDur.String(),
		"series":    series,
	})
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// accept bare seconds integer or Go duration string
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	// time.ParseDuration only handles up to "h"; handle "d" manually.
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
