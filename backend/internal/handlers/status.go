package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/telemetry"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// streamReplayCount is how many recent events/log lines a fresh SSE client
// receives before the live stream starts.
const streamReplayCount = 100

// maxLogRequestLimit bounds the ?limit= on the logs/events endpoints.
const maxLogRequestLimit = 500

// StatusHandler serves the admin status page: runtime overview, traffic
// counters and the live log stream. All endpoints are admin-authenticated and
// read-only.
type StatusHandler struct {
	cfg      *config.Config
	db       *gorm.DB
	cache    cache.CacheInterface
	recorder *telemetry.Recorder
}

// NewStatusHandler creates the status handler.
func NewStatusHandler(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface, recorder *telemetry.Recorder) *StatusHandler {
	if recorder == nil {
		recorder = telemetry.Default()
	}
	return &StatusHandler{cfg: cfg, db: db, cache: cacheClient, recorder: recorder}
}

type configStatus struct {
	ID          int32  `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Type        string `json:"type"`
	Active      bool   `json:"active"`
	Keys        int    `json:"keys"`
	ActiveKeys  int    `json:"active_keys"`
	CoolingKeys int    `json:"cooling_keys"`
}

type overviewResponse struct {
	Status        string         `json:"status"`
	Time          time.Time      `json:"time"`
	StartedAt     time.Time      `json:"started_at"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Version       string         `json:"version"`
	GoVersion     string         `json:"go_version"`
	CacheType     string         `json:"cache_type"`
	DBType        string         `json:"db_type"`
	EgressEnabled bool           `json:"egress_enabled"`
	Configs       int            `json:"configs"`
	ActiveConfigs int            `json:"active_configs"`
	Keys          int            `json:"keys"`
	ActiveKeys    int            `json:"active_keys"`
	CoolingKeys   int            `json:"cooling_keys"`
	TotalRequests int64          `json:"total_requests"`
	PerConfig     []configStatus `json:"per_config"`
}

// Overview returns process/runtime status plus per-config key health.
func (h *StatusHandler) Overview(c *gin.Context) {
	resp := overviewResponse{
		Status:        "ok",
		Time:          time.Now(),
		StartedAt:     h.recorder.StartedAt(),
		UptimeSeconds: int64(h.recorder.Uptime().Seconds()),
		Version:       appVersion(),
		GoVersion:     runtime.Version(),
		CacheType:     h.cfg.CacheType,
		DBType:        h.cfg.DBType,
		EgressEnabled: h.cfg.EgressEnabled,
		TotalRequests: h.recorder.TotalRequests(),
		PerConfig:     []configStatus{},
	}

	var configs []models.ProxyConfig
	if h.db != nil {
		if err := h.db.Preload("APIKeys").Order("id").Find(&configs).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load configs", "detail": err.Error()})
			return
		}
	}

	rotator := services.NewKeyRotator(h.cfg, h.cache, "status overview")

	for _, cfgRow := range configs {
		row := configStatus{
			ID:     cfgRow.ID,
			Name:   cfgRow.Name,
			Slug:   cfgRow.Slug,
			Type:   cfgRow.ConfigType,
			Active: cfgRow.IsActive,
		}
		for _, key := range cfgRow.APIKeys {
			row.Keys++
			resp.Keys++
			if !key.IsActive {
				continue
			}
			row.ActiveKeys++
			resp.ActiveKeys++
			if rotator.RemainingCooldown(cfgRow.ID, key.KeyValue) > 0 {
				row.CoolingKeys++
				resp.CoolingKeys++
			}
		}
		resp.Configs++
		if cfgRow.IsActive {
			resp.ActiveConfigs++
		}
		resp.PerConfig = append(resp.PerConfig, row)
	}

	c.JSON(http.StatusOK, resp)
}

type slugTraffic struct {
	Slug         string `json:"slug"`
	Requests     int64  `json:"requests"`
	RateLimited  int64  `json:"rate_limited"`
	ClientErrors int64  `json:"client_errors"`
	ServerErrors int64  `json:"server_errors"`
	AvgLatencyMS int64  `json:"avg_latency_ms"`
}

type trafficResponse struct {
	Since             time.Time        `json:"since"`
	TotalRequests     int64            `json:"total_requests"`
	AvgLatencyMS      int64            `json:"avg_latency_ms"`
	StatusClasses     map[string]int64 `json:"status_classes"`
	StatusCounts      map[string]int64 `json:"status_counts"`
	RequestsPerMinute []int64          `json:"requests_per_minute"`
	PerSlug           []slugTraffic    `json:"per_slug"`
}

// Traffic returns cumulative counters (since process start) plus the last 60
// one-minute buckets.
func (h *StatusHandler) Traffic(c *gin.Context) {
	counts := h.recorder.StatusCounts()

	resp := trafficResponse{
		Since:             h.recorder.StartedAt(),
		TotalRequests:     h.recorder.TotalRequests(),
		StatusClasses:     map[string]int64{"1xx": 0, "2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0, "other": 0},
		StatusCounts:      make(map[string]int64, len(counts)),
		RequestsPerMinute: h.recorder.RequestsPerMinute(),
		PerSlug:           []slugTraffic{},
	}

	var latencyTotal int64
	for code, count := range counts {
		resp.StatusCounts[strconv.Itoa(code)] = count
		switch code / 100 {
		case 1:
			resp.StatusClasses["1xx"] += count
		case 2:
			resp.StatusClasses["2xx"] += count
		case 3:
			resp.StatusClasses["3xx"] += count
		case 4:
			resp.StatusClasses["4xx"] += count
		case 5:
			resp.StatusClasses["5xx"] += count
		default:
			resp.StatusClasses["other"] += count
		}
	}

	for slug, stat := range h.recorder.SlugStats() {
		latencyTotal += stat.TotalLatencyMS
		resp.PerSlug = append(resp.PerSlug, slugTraffic{
			Slug:         slug,
			Requests:     stat.Requests,
			RateLimited:  stat.RateLimited,
			ClientErrors: stat.ClientErrors,
			ServerErrors: stat.ServerErrors,
			AvgLatencyMS: stat.AvgLatencyMS(),
		})
	}
	if resp.TotalRequests > 0 {
		resp.AvgLatencyMS = latencyTotal / resp.TotalRequests
	}
	sort.Slice(resp.PerSlug, func(i, j int) bool {
		if resp.PerSlug[i].Requests == resp.PerSlug[j].Requests {
			return resp.PerSlug[i].Slug < resp.PerSlug[j].Slug
		}
		return resp.PerSlug[i].Requests > resp.PerSlug[j].Requests
	})

	c.JSON(http.StatusOK, resp)
}

// Logs returns the most recent application log lines (redacted).
func (h *StatusHandler) Logs(c *gin.Context) {
	lines := h.recorder.RecentLogs(requestLimit(c))
	out := make([]telemetry.LogLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, h.redactLog(line))
	}
	c.JSON(http.StatusOK, gin.H{"logs": out})
}

// Events returns the most recent structured events (redacted).
func (h *StatusHandler) Events(c *gin.Context) {
	events := h.recorder.RecentEvents(requestLimit(c))
	out := make([]telemetry.Event, 0, len(events))
	for _, ev := range events {
		out = append(out, h.redactEvent(ev))
	}
	c.JSON(http.StatusOK, gin.H{"events": out})
}

// Stream is a Server-Sent Events endpoint. It replays recent events/log lines
// and then pushes new ones live. Clients consume it with fetch (the native
// EventSource cannot send the Authorization header this API requires).
func (h *StatusHandler) Stream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	// Ask nginx (and similar proxies) not to buffer this one response.
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()

	for _, ev := range h.recorder.RecentEvents(streamReplayCount) {
		raw, err := json.Marshal(h.redactEvent(ev))
		if err != nil {
			continue
		}
		if err := writeSSEFrame(c.Writer, ev.ID, "event", raw); err != nil {
			return
		}
	}
	for _, line := range h.recorder.RecentLogs(streamReplayCount) {
		raw, err := json.Marshal(h.redactLog(line))
		if err != nil {
			continue
		}
		if err := writeSSEFrame(c.Writer, line.ID, "log", raw); err != nil {
			return
		}
	}
	c.Writer.Flush()

	frames, cancel := h.recorder.Subscribe()
	defer cancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	c.Stream(func(w io.Writer) bool {
		select {
		case frame, ok := <-frames:
			if !ok {
				return false
			}
			switch data := frame.Data.(type) {
			case telemetry.Event:
				raw, err := json.Marshal(h.redactEvent(data))
				if err != nil {
					return true
				}
				return writeSSEFrame(w, data.ID, "event", raw) == nil
			case telemetry.LogLine:
				raw, err := json.Marshal(h.redactLog(data))
				if err != nil {
					return true
				}
				return writeSSEFrame(w, data.ID, "log", raw) == nil
			default:
				return true
			}
		case <-heartbeat.C:
			_, err := io.WriteString(w, ": heartbeat\n\n")
			return err == nil
		case <-c.Request.Context().Done():
			return false
		}
	})
}

// writeSSEFrame writes one SSE record. JSON payloads never contain raw
// newlines (the encoder escapes them), so a single data: line is correct.
func writeSSEFrame(w io.Writer, id int64, name string, data []byte) error {
	if id > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", id); err != nil {
			return err
		}
	}
	if name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", name); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func (h *StatusHandler) redact(s string) string {
	return utils.RedactSecrets(s, h.cfg.GetGlobalProxyKeys())
}

func (h *StatusHandler) redactEvent(ev telemetry.Event) telemetry.Event {
	ev.Message = h.redact(ev.Message)
	ev.Path = h.redact(ev.Path)
	return ev
}

func (h *StatusHandler) redactLog(line telemetry.LogLine) telemetry.LogLine {
	line.Message = h.redact(line.Message)
	return line
}

func requestLimit(c *gin.Context) int {
	limit := 200
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxLogRequestLimit {
		limit = maxLogRequestLimit
	}
	return limit
}

func appVersion() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return "dev"
}
