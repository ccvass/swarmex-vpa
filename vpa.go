package vpa

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

const (
	labelEnabled   = "swarmex.vpa.enabled"
	labelMinCPU    = "swarmex.vpa.min-cpu"
	labelMaxCPU    = "swarmex.vpa.max-cpu"
	labelMinMemory = "swarmex.vpa.min-memory"
	labelMaxMemory = "swarmex.vpa.max-memory"
)

type serviceState struct {
	name     string
	minCPU   float64
	maxCPU   float64
	minMem   int64
	maxMem   int64
}

type Controller struct {
	docker     *client.Client
	promURL    string
	logger     *slog.Logger
	services   map[string]*serviceState
	mu         sync.Mutex
	httpClient *http.Client
}

func New(cli *client.Client, promURL string, logger *slog.Logger) *Controller {
	return &Controller{docker: cli, promURL: promURL, logger: logger, services: make(map[string]*serviceState), httpClient: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Controller) HandleEvent(ctx context.Context, event events.Message) {
	if event.Type != events.ServiceEventType {
		return
	}
	if event.Action == events.ActionRemove {
		c.mu.Lock()
		delete(c.services, event.Actor.ID)
		c.mu.Unlock()
		return
	}
	c.reconcile(ctx, event.Actor.ID)
}

func (c *Controller) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evaluateAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) reconcile(ctx context.Context, serviceID string) {
	svc, _, err := c.docker.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return
	}
	if svc.Spec.Labels[labelEnabled] != "true" {
		c.mu.Lock()
		delete(c.services, serviceID)
		c.mu.Unlock()
		return
	}
	state := &serviceState{name: svc.Spec.Name}
	state.minCPU, _ = strconv.ParseFloat(svc.Spec.Labels[labelMinCPU], 64)
	state.maxCPU, _ = strconv.ParseFloat(svc.Spec.Labels[labelMaxCPU], 64)
	state.minMem = parseMemory(svc.Spec.Labels[labelMinMemory])
	state.maxMem = parseMemory(svc.Spec.Labels[labelMaxMemory])
	if state.minCPU == 0 { state.minCPU = 0.1 }
	if state.maxCPU == 0 { state.maxCPU = 2.0 }
	if state.minMem == 0 { state.minMem = 64 * 1024 * 1024 }
	if state.maxMem == 0 { state.maxMem = 1024 * 1024 * 1024 }

	c.mu.Lock()
	c.services[serviceID] = state
	c.mu.Unlock()
	c.logger.Info("vpa watching", "service", state.name, "cpu", fmt.Sprintf("%.1f-%.1f", state.minCPU, state.maxCPU), "mem", fmt.Sprintf("%dM-%dM", state.minMem/1024/1024, state.maxMem/1024/1024))
}

func (c *Controller) evaluateAll(ctx context.Context) {
	c.mu.Lock()
	snapshot := make(map[string]*serviceState)
	for k, v := range c.services { snapshot[k] = v }
	c.mu.Unlock()

	for id, state := range snapshot {
		c.evaluate(ctx, id, state)
	}
}

func (c *Controller) evaluate(ctx context.Context, serviceID string, state *serviceState) {
	svc, _, err := c.docker.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return
	}

	cpuUsage := c.queryMetric(ctx, fmt.Sprintf(`avg(rate(container_cpu_usage_seconds_total{container_label_com_docker_swarm_service_name="%s"}[5m]))`, state.name))
	memUsage := c.queryMetric(ctx, fmt.Sprintf(`avg(container_memory_usage_bytes{container_label_com_docker_swarm_service_name="%s"})`, state.name))

	if math.IsNaN(cpuUsage) || math.IsNaN(memUsage) { return }

	// Target: 20% headroom above actual usage
	targetCPU := math.Max(state.minCPU, math.Min(state.maxCPU, cpuUsage*1.2))
	targetMem := int64(math.Max(float64(state.minMem), math.Min(float64(state.maxMem), memUsage*1.2)))

	if svc.Spec.TaskTemplate.Resources == nil {
		svc.Spec.TaskTemplate.Resources = &swarm.ResourceRequirements{}
	}
	if svc.Spec.TaskTemplate.Resources.Limits == nil {
		svc.Spec.TaskTemplate.Resources.Limits = &swarm.Limit{}
	}

	currentCPU := float64(svc.Spec.TaskTemplate.Resources.Limits.NanoCPUs) / 1e9
	currentMem := svc.Spec.TaskTemplate.Resources.Limits.MemoryBytes

	// Only update if change > 10%
	cpuDiff := math.Abs(targetCPU-currentCPU) / math.Max(currentCPU, 0.01)
	memDiff := math.Abs(float64(targetMem-currentMem)) / math.Max(float64(currentMem), 1)

	if cpuDiff < 0.1 && memDiff < 0.1 { return }

	svc.Spec.TaskTemplate.Resources.Limits.NanoCPUs = int64(targetCPU * 1e9)
	svc.Spec.TaskTemplate.Resources.Limits.MemoryBytes = targetMem

	_, err = c.docker.ServiceUpdate(ctx, serviceID, svc.Version, svc.Spec, types.ServiceUpdateOptions{})
	if err != nil {
		c.logger.Error("vpa update failed", "service", state.name, "error", err)
		return
	}
	c.logger.Info("vpa adjusted", "service", state.name, "cpu", fmt.Sprintf("%.2f→%.2f", currentCPU, targetCPU), "mem", fmt.Sprintf("%dM→%dM", currentMem/1024/1024, targetMem/1024/1024))
}

func (c *Controller) queryMetric(ctx context.Context, query string) float64 {
	u := fmt.Sprintf("%s/api/v1/query?query=%s", c.promURL, url.QueryEscape(query))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil { return math.NaN() }
	defer resp.Body.Close()
	var result struct { Data struct { Result []struct { Value []json.RawMessage `json:"value"` } `json:"result"` } `json:"data"` }
	if json.NewDecoder(resp.Body).Decode(&result) != nil || len(result.Data.Result) == 0 { return math.NaN() }
	if len(result.Data.Result[0].Value) < 2 { return math.NaN() }
	var s string
	json.Unmarshal(result.Data.Result[0].Value[1], &s)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseMemory(s string) int64 {
	if s == "" { return 0 }
	s2 := s
	mul := int64(1)
	if s[len(s)-1] == 'M' { mul = 1024 * 1024; s2 = s[:len(s)-1] }
	if s[len(s)-1] == 'G' { mul = 1024 * 1024 * 1024; s2 = s[:len(s)-1] }
	v, _ := strconv.ParseInt(s2, 10, 64)
	return v * mul
}
