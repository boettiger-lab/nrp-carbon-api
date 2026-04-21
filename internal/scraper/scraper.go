package scraper

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/boettiger-lab/carbon-api/internal/carbon"
	"github.com/boettiger-lab/carbon-api/internal/prom"
)

// ModelMetrics holds the latest carbon and performance metrics for one LLM model.
type ModelMetrics struct {
	// Identity
	ModelName  string `json:"model_name"`
	Namespace  string `json:"namespace"`
	Container  string `json:"container"`
	GPUHardware string `json:"gpu_hardware"` // e.g. "A100-SXM4-80GB"
	Node       string `json:"node"`

	// Raw
	GPUCount               int     `json:"gpu_count"`
	PowerWatts             float64 `json:"power_watts"`
	PromptTokensPerSec     float64 `json:"prompt_tokens_per_sec"`     // input (prefill) token rate
	GenerationTokensPerSec float64 `json:"generation_tokens_per_sec"` // output (decode) token rate
	TokensPerSec           float64 `json:"tokens_per_sec"`            // total = prompt + generation

	// Carbon
	CarbonIntensity      float64 `json:"carbon_intensity_kg_per_kwh"`        // grid intensity used
	CO2GramsPerHour      float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken        float64 `json:"co2_mg_per_token,omitempty"`          // 0 when idle (5-min window, ≥5 tok/s)
	CO2MgPerTokenAvg24h  float64 `json:"co2_mg_per_token_avg_24h,omitempty"`  // token-weighted 24h mean, active periods only
	CO2MgPerTokenAvg7d   float64 `json:"co2_mg_per_token_avg_7d,omitempty"`   // token-weighted 7-day mean, active periods only

	// Time-weighted 24h means (all samples, active + idle). Used for apples-to-apples
	// "vs commercial frontier" comparison on the card over the same window as the bar chart.
	PowerWattsAvg24h             float64 `json:"power_watts_avg_24h,omitempty"`
	PromptTokensPerSecAvg24h     float64 `json:"prompt_tokens_per_sec_avg_24h,omitempty"`
	GenerationTokensPerSecAvg24h float64 `json:"generation_tokens_per_sec_avg_24h,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// History is a fixed-size ring buffer of (time, value) pairs per metric.
type dataPoint struct {
	T time.Time
	V float64
}

// avgBucket holds one hour of aggregates: a token-weighted CO₂/token mean
// (active samples only) plus time-weighted means for power, prompt tok/s,
// and generation tok/s (every reporting sample, active or idle).
type avgBucket struct {
	Hour         int64   // Unix timestamp truncated to hour
	WeightedSum  float64 // Σ(co2_per_token_i × tokens_per_sec_i), active samples
	TokenSum     float64 // Σ(tokens_per_sec_i), active samples
	PowerSum     float64 // Σ power_watts_i, all reporting samples
	PromptTokSum float64 // Σ prompt_tokens_per_sec_i, all reporting samples
	GenTokSum    float64 // Σ generation_tokens_per_sec_i, all reporting samples
	SampleCount  int     // number of reporting samples contributing to the *Sum fields
}

const maxBuckets = 168  // 7 days of hourly buckets
const maxHistory = 20160 // 7 days at 30s scrape intervals (for Series endpoint ring buffers)

type modelHistory struct {
	PowerWatts      []dataPoint // ring buffer for Series endpoint
	CO2GramsPerHour []dataPoint // ring buffer for Series endpoint
	CO2MgPerToken   []dataPoint // ring buffer for Series endpoint
	AvgBuckets      []avgBucket // hourly aggregates for token-weighted averaging
}

func (h *modelHistory) append(now time.Time, m *ModelMetrics) {
	push := func(buf *[]dataPoint, v float64) {
		*buf = append(*buf, dataPoint{T: now, V: v})
		if len(*buf) > maxHistory {
			*buf = (*buf)[len(*buf)-maxHistory:]
		}
	}
	push(&h.PowerWatts, m.PowerWatts)
	push(&h.CO2GramsPerHour, m.CO2GramsPerHour)
	if m.CO2MgPerToken > 0 {
		push(&h.CO2MgPerToken, m.CO2MgPerToken)
	}
	h.addSample(now, m.PowerWatts, m.PromptTokensPerSec, m.GenerationTokensPerSec, m.CarbonIntensity)
}

// addSample accumulates one scrape sample into the hourly aggregate bucket for time t.
// Every sample with power > 0 contributes to the time-weighted power/tok-rate sums.
// Active samples (prompt+gen > 5 tok/s) additionally contribute to the token-weighted
// CO₂/token sums.
func (h *modelHistory) addSample(t time.Time, power, promptTok, genTok, intensity float64) {
	if power <= 0 {
		return
	}
	totalTok := promptTok + genTok
	var co2Weight, co2Tokens float64
	if totalTok > 5.0 {
		co2PerToken := carbon.MgPerToken(power, intensity, totalTok)
		co2Weight = co2PerToken * totalTok
		co2Tokens = totalTok
	}
	hourKey := t.Truncate(time.Hour).Unix()
	for i := len(h.AvgBuckets) - 1; i >= 0; i-- {
		if h.AvgBuckets[i].Hour == hourKey {
			b := &h.AvgBuckets[i]
			b.WeightedSum += co2Weight
			b.TokenSum += co2Tokens
			b.PowerSum += power
			b.PromptTokSum += promptTok
			b.GenTokSum += genTok
			b.SampleCount++
			return
		}
	}
	h.AvgBuckets = append(h.AvgBuckets, avgBucket{
		Hour:         hourKey,
		WeightedSum:  co2Weight,
		TokenSum:     co2Tokens,
		PowerSum:     power,
		PromptTokSum: promptTok,
		GenTokSum:    genTok,
		SampleCount:  1,
	})
	if len(h.AvgBuckets) > maxBuckets {
		h.AvgBuckets = h.AvgBuckets[len(h.AvgBuckets)-maxBuckets:]
	}
}

// Scraper polls Prometheus and maintains in-memory state.
type Scraper struct {
	client   *prom.Client
	interval time.Duration

	mu      sync.RWMutex
	models  map[string]*ModelMetrics // key: namespace/container
	history map[string]*modelHistory
}

func New(promURL string, interval time.Duration) *Scraper {
	return &Scraper{
		client:   prom.NewClient(promURL, 30*time.Second),
		interval: interval,
		models:   make(map[string]*ModelMetrics),
		history:  make(map[string]*modelHistory),
	}
}

// Run starts the background scrape loop. Call in a goroutine.
func (s *Scraper) Run() {
	s.scrape()   // first scrape populates models with current intensities
	s.backfill() // seed hourly buckets from Prometheus history
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for range t.C {
		s.scrape()
	}
}

// backfill queries Prometheus for 7 days of historical power and token data
// and seeds the hourly average buckets so that 24h/7d averages are immediately
// correct after a restart, rather than starting from zero.
func (s *Scraper) backfill() {
	log.Println("scraper: backfilling 7-day averages from Prometheus...")
	end := time.Now()
	start := end.Add(-7 * 24 * time.Hour)
	step := 5 * time.Minute

	powerSeries, err := s.client.RangeQuery(
		`sum by (namespace, container) (DCGM_FI_DEV_POWER_USAGE{namespace=~"nrp-llm|sdsc-llm"})`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill power query failed: %v", err)
		return
	}
	promptSeries, err := s.client.RangeQuery(
		`sum by (namespace, container) (rate(vllm:prompt_tokens_total[5m]))`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill prompt token query failed: %v", err)
		return
	}
	genSeries, err := s.client.RangeQuery(
		`sum by (namespace, container) (rate(vllm:generation_tokens_total[5m]))`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill generation token query failed: %v", err)
		return
	}

	// Join power and token data by (key, timestamp).
	type sample struct{ power, promptTok, genTok float64 }
	byKeyTime := make(map[string]map[int64]*sample)

	for _, sr := range powerSeries {
		key := sr.Metric["namespace"] + "/" + sr.Metric["container"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].power += pt.Value
		}
	}
	for _, sr := range promptSeries {
		key := sr.Metric["namespace"] + "/" + sr.Metric["container"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].promptTok += pt.Value
		}
	}
	for _, sr := range genSeries {
		key := sr.Metric["namespace"] + "/" + sr.Metric["container"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].genTok += pt.Value
		}
	}

	// Use current carbon intensities from the first live scrape.
	s.mu.RLock()
	intensityByKey := make(map[string]float64, len(s.models))
	for key, m := range s.models {
		intensityByKey[key] = m.CarbonIntensity
	}
	s.mu.RUnlock()

	// Populate hourly average buckets.
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, timestamps := range byKeyTime {
		intensity := intensityByKey[key]
		if intensity == 0 {
			intensity = carbon.NRPDefault
		}
		if s.history[key] == nil {
			s.history[key] = &modelHistory{}
		}
		h := s.history[key]
		for ts, samp := range timestamps {
			if samp.power <= 0 {
				continue
			}
			h.addSample(time.Unix(ts, 0), samp.power, samp.promptTok, samp.genTok, intensity)
		}
	}

	log.Printf("scraper: backfilled %d model(s) from Prometheus", len(byKeyTime))
}

// Models returns a snapshot of all current model metrics.
func (s *Scraper) Models() []*ModelMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ModelMetrics, 0, len(s.models))
	for _, m := range s.models {
		cp := *m
		out = append(out, &cp)
	}
	return out
}

// Series returns the history for a model/metric combination.
// metric is one of "power_watts", "co2_grams_per_hour", "co2_mg_per_token".
func (s *Scraper) Series(namespace, container, metric string, since time.Duration) [][2]interface{} {
	key := namespace + "/" + container
	s.mu.RLock()
	h, ok := s.history[key]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	cutoff := time.Now().Add(-since)
	var buf []dataPoint
	switch metric {
	case "power_watts":
		buf = h.PowerWatts
	case "co2_grams_per_hour":
		buf = h.CO2GramsPerHour
	case "co2_mg_per_token":
		buf = h.CO2MgPerToken
	default:
		return nil
	}

	var out [][2]interface{}
	for _, p := range buf {
		if p.T.After(cutoff) {
			out = append(out, [2]interface{}{p.T.Unix(), p.V})
		}
	}
	return out
}

// ---- internal ----

func (s *Scraper) scrape() {
	// 1. GPU power per pod (namespace + container)
	powerByKey, nodeByKey, err := s.queryPower()
	if err != nil {
		log.Printf("scraper: power query failed: %v", err)
	}

	// 2. GPU count + hardware model per pod
	gpuByKey, hardwareByKey, err := s.queryGPUInfo()
	if err != nil {
		log.Printf("scraper: gpu info query failed: %v", err)
	}

	// 3. Token generation and prompt rates per pod
	genTokensByKey, promptTokensByKey, modelNameByKey, err := s.queryTokens()
	if err != nil {
		log.Printf("scraper: token query failed: %v", err)
	}

	// Union of all known model keys
	keys := make(map[string]struct{})
	for k := range powerByKey {
		keys[k] = struct{}{}
	}
	for k := range genTokensByKey {
		keys[k] = struct{}{}
	}
	for k := range promptTokensByKey {
		keys[k] = struct{}{}
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range keys {
		ns, container := splitKey(key)
		power := powerByKey[key]
		node := nodeByKey[key]
		intensity := carbon.IntensityForNode(node, ns)

		genTok    := genTokensByKey[key]
		promptTok := promptTokensByKey[key]
		totalTok  := genTok + promptTok
		modelName := modelNameByKey[key]
		gpuCount  := gpuByKey[key]
		hw        := hardwareByKey[key]

		co2PerHour := carbon.GramsPerHour(power, intensity)
		co2PerToken := 0.0
		if totalTok > 5.0 {
			// CO₂/token uses total tokens (prompt + generation) as denominator:
			// both prefill and decode phases consume GPU energy, and agentic
			// workloads are dominated by large prompt (input) token counts.
			co2PerToken = carbon.MgPerToken(power, intensity, totalTok)
		}

		m := &ModelMetrics{
			ModelName:              modelName,
			Namespace:              ns,
			Container:              container,
			GPUHardware:            hw,
			Node:                   node,
			GPUCount:               gpuCount,
			PowerWatts:             math.Round(power*10) / 10,
			PromptTokensPerSec:     math.Round(promptTok*10) / 10,
			GenerationTokensPerSec: math.Round(genTok*10) / 10,
			TokensPerSec:           math.Round(totalTok*10) / 10,
			CarbonIntensity:        intensity,
			CO2GramsPerHour:        math.Round(co2PerHour*10) / 10,
			UpdatedAt:              now,
		}
		if co2PerToken > 0 {
			m.CO2MgPerToken = math.Round(co2PerToken*1000) / 1000
		}

		s.models[key] = m

		if s.history[key] == nil {
			s.history[key] = &modelHistory{}
		}
		h := s.history[key]
		h.append(now, m)

		// Token-weighted CO₂/token averages (active samples) and time-weighted
		// power / prompt-tok/s / gen-tok/s averages (all samples) from hourly buckets.
		var wSum24, tSum24, wSum7d, tSum7d float64
		var powSum24, promptSum24, genSum24 float64
		var nSum24 int
		cutoff24h := now.Add(-24 * time.Hour).Truncate(time.Hour).Unix()
		cutoff7d := now.Add(-7 * 24 * time.Hour).Truncate(time.Hour).Unix()
		for _, b := range h.AvgBuckets {
			if b.Hour >= cutoff7d {
				wSum7d += b.WeightedSum
				tSum7d += b.TokenSum
			}
			if b.Hour >= cutoff24h {
				wSum24 += b.WeightedSum
				tSum24 += b.TokenSum
				powSum24 += b.PowerSum
				promptSum24 += b.PromptTokSum
				genSum24 += b.GenTokSum
				nSum24 += b.SampleCount
			}
		}
		if tSum24 > 0 {
			m.CO2MgPerTokenAvg24h = math.Round(wSum24/tSum24*1000) / 1000
		}
		if tSum7d > 0 {
			m.CO2MgPerTokenAvg7d = math.Round(wSum7d/tSum7d*1000) / 1000
		}
		if nSum24 > 0 {
			n := float64(nSum24)
			m.PowerWattsAvg24h = math.Round(powSum24/n*10) / 10
			m.PromptTokensPerSecAvg24h = math.Round(promptSum24/n*10) / 10
			m.GenerationTokensPerSecAvg24h = math.Round(genSum24/n*10) / 10
		}
	}
}

// queryPower returns total GPU power (W) keyed by "namespace/container".
// Also returns the node hostname (Hostname label) for carbon intensity lookup.
func (s *Scraper) queryPower() (map[string]float64, map[string]string, error) {
	// Include Hostname in the aggregation — all GPUs in a pod share the same node.
	results, err := s.client.Query(
		`sum by (namespace, container, Hostname) (avg_over_time(DCGM_FI_DEV_POWER_USAGE{namespace=~"nrp-llm|sdsc-llm"}[5m]))`,
	)
	if err != nil {
		return nil, nil, err
	}

	power := make(map[string]float64)
	nodes := make(map[string]string)
	for _, r := range results {
		key := r.Metric["namespace"] + "/" + r.Metric["container"]
		power[key] += r.Value
		if nodes[key] == "" {
			nodes[key] = r.Metric["Hostname"]
		}
	}
	return power, nodes, nil
}

// queryGPUInfo returns GPU count and hardware model keyed by "namespace/container".
func (s *Scraper) queryGPUInfo() (map[string]int, map[string]string, error) {
	results, err := s.client.Query(
		`count by (namespace, container, modelName) (DCGM_FI_DEV_GPU_UTIL{namespace=~"nrp-llm|sdsc-llm"})`,
	)
	if err != nil {
		return nil, nil, err
	}

	counts := make(map[string]int)
	hardware := make(map[string]string)
	for _, r := range results {
		key := r.Metric["namespace"] + "/" + r.Metric["container"]
		counts[key] += int(r.Value)
		if hardware[key] == "" {
			hardware[key] = r.Metric["modelName"]
		}
	}
	return counts, hardware, nil
}

// queryTokens returns 5-minute prompt and generation token rates keyed by "namespace/container".
// Also returns the LLM model_name label.
// Prompt tokens (prefill/input) and generation tokens (decode/output) are returned separately
// so callers can track agentic workloads where input tokens dominate.
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	genResults, err := s.client.Query(
		`sum by (namespace, container, model_name) (rate(vllm:generation_tokens_total[5m]))`,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(
		`sum by (namespace, container, model_name) (rate(vllm:prompt_tokens_total[5m]))`,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	genTokens = make(map[string]float64)
	promptTokens = make(map[string]float64)
	names = make(map[string]string)
	for _, r := range genResults {
		key := r.Metric["namespace"] + "/" + r.Metric["container"]
		genTokens[key] += r.Value
		if names[key] == "" {
			names[key] = r.Metric["model_name"]
		}
	}
	for _, r := range promptResults {
		key := r.Metric["namespace"] + "/" + r.Metric["container"]
		promptTokens[key] += r.Value
		if names[key] == "" {
			names[key] = r.Metric["model_name"]
		}
	}
	return genTokens, promptTokens, names, nil
}

// ClusterTimePoint is one time-step of aggregated cluster-wide carbon data.
type ClusterTimePoint struct {
	Timestamp       int64   `json:"t"`
	PowerWatts      float64 `json:"power_watts"`
	CO2GramsPerHour float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken   float64 `json:"co2_mg_per_token,omitempty"` // 0 when no active generation
}

// ClusterTimeSeries queries Prometheus for historical power + token data, applies
// per-model carbon intensities, and returns aggregated cluster totals per time step.
func (s *Scraper) ClusterTimeSeries(rangeBack, step time.Duration) ([]ClusterTimePoint, error) {
	end := time.Now()
	start := end.Add(-rangeBack)

	// Fetch power and token rate time series in parallel.
	powerSeries, err := s.client.RangeQuery(
		`sum by (namespace, container) (DCGM_FI_DEV_POWER_USAGE{namespace=~"nrp-llm|sdsc-llm"})`,
		start, end, step,
	)
	if err != nil {
		return nil, err
	}
	// Use total tokens (prompt + generation) as denominator for CO₂/token.
	// Agentic workloads have large prompt token counts that dominate compute.
	tokenSeries, err := s.client.RangeQuery(
		`sum by (namespace, container) (rate(vllm:generation_tokens_total[5m]) + rate(vllm:prompt_tokens_total[5m]))`,
		start, end, step,
	)
	if err != nil {
		return nil, err
	}

	// Build intensity lookup from current model state.
	s.mu.RLock()
	intensityByKey := make(map[string]float64, len(s.models))
	for key, m := range s.models {
		intensityByKey[key] = m.CarbonIntensity
	}
	s.mu.RUnlock()

	type agg struct{ power, co2, tokens float64 }
	byTime := make(map[int64]*agg)

	for _, sr := range powerSeries {
		key := sr.Metric["namespace"] + "/" + sr.Metric["container"]
		intensity, ok := intensityByKey[key]
		if !ok {
			intensity = carbon.USAverage
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byTime[ts] == nil {
				byTime[ts] = &agg{}
			}
			byTime[ts].power += pt.Value
			byTime[ts].co2 += carbon.GramsPerHour(pt.Value, intensity)
		}
	}
	for _, sr := range tokenSeries {
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byTime[ts] == nil {
				byTime[ts] = &agg{}
			}
			byTime[ts].tokens += pt.Value
		}
	}

	// Sort by timestamp and return.
	out := make([]ClusterTimePoint, 0, len(byTime))
	for ts, a := range byTime {
		pt := ClusterTimePoint{
			Timestamp:       ts,
			PowerWatts:      math.Round(a.power*10) / 10,
			CO2GramsPerHour: math.Round(a.co2*10) / 10,
		}
		// CO2/token = total_co2_grams_per_hour / (tokens_per_sec × 3600) × 1000 mg/g
		if a.tokens > 0.1 {
			pt.CO2MgPerToken = math.Round(a.co2/a.tokens/3.6*1000) / 1000
		}
		out = append(out, pt)
	}
	// Simple insertion sort — range queries are usually small.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Timestamp < out[j-1].Timestamp; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func splitKey(key string) (namespace, container string) {
	for i, c := range key {
		if c == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
