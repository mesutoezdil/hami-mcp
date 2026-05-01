package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func init() {
	// prometheus/common reads this global during text parsing; in v0.67 it defaults
	// to the zero value ("unset") in some build paths and panics. Pin it explicitly.
	model.NameValidationScheme = model.UTF8Validation
}

const defaultMetricsURL = "http://localhost:31993/metrics"

func metricsURL() string {
	if v := os.Getenv("HAMI_METRICS_URL"); v != "" {
		return v
	}
	return defaultMetricsURL
}

type sample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

func scrape(ctx context.Context) ([]sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", metricsURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("metrics endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	var out []sample
	for name, mf := range families {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			val, ok := metricValue(m, mf.GetType())
			if !ok {
				continue
			}
			out = append(out, sample{Name: name, Labels: labels, Value: val})
		}
	}
	return out, nil
}

func metricValue(m *dto.Metric, t dto.MetricType) (float64, bool) {
	switch t {
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue(), true
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue(), true
	case dto.MetricType_UNTYPED:
		return m.GetUntyped().GetValue(), true
	}
	return 0, false
}

func filter(samples []sample, match map[string]string) []sample {
	var out []sample
	for _, s := range samples {
		ok := true
		for k, v := range match {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func textResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

// get_gpu_metrics: per-device memory + core utilization, optionally filtered by node.
func handleGetGPUMetrics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	node := req.GetString("node_name", "")
	samples, err := scrape(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	type deviceState struct {
		Node              string  `json:"node"`
		DeviceIndex       string  `json:"device_index"`
		DeviceUUID        string  `json:"device_uuid"`
		DeviceType        string  `json:"device_type,omitempty"`
		MemoryAllocatedMB float64 `json:"memory_allocated_mb"`
		MemoryLimitMB     float64 `json:"memory_limit_mb"`
		MemoryPercent     float64 `json:"memory_percent"`
		CoreAllocated     float64 `json:"core_allocated"`
		CoreLimit         float64 `json:"core_limit"`
		SharedContainers  float64 `json:"shared_containers"`
	}
	devices := map[string]*deviceState{}
	get := func(uuid, nodeID string) *deviceState {
		key := nodeID + "|" + uuid
		if d, ok := devices[key]; ok {
			return d
		}
		d := &deviceState{Node: nodeID, DeviceUUID: uuid}
		devices[key] = d
		return d
	}

	// Only consider node-scoped device metrics. Per-pod metrics like
	// vGPUMemoryAllocated use a different label set (`nodename`, `podname`) and
	// are surfaced by get_vgpu_allocation.
	deviceMetrics := map[string]bool{
		"GPUDeviceMemoryAllocated": true,
		"GPUDeviceMemoryLimit":     true,
		"GPUDeviceCoreAllocated":   true,
		"GPUDeviceCoreLimit":       true,
		"GPUDeviceSharedNum":       true,
		"nodeGPUMemoryPercentage":  true,
		"nodeGPUOverview":          true,
	}
	for _, s := range samples {
		if !deviceMetrics[s.Name] {
			continue
		}
		nodeID := s.Labels["nodeid"]
		uuid := s.Labels["deviceuuid"]
		if uuid == "" || nodeID == "" {
			continue
		}
		if node != "" && nodeID != node {
			continue
		}
		d := get(uuid, nodeID)
		d.DeviceIndex = s.Labels["deviceidx"]
		switch s.Name {
		case "GPUDeviceMemoryAllocated":
			d.MemoryAllocatedMB = s.Value / (1024 * 1024)
		case "GPUDeviceMemoryLimit":
			d.MemoryLimitMB = s.Value / (1024 * 1024)
		case "GPUDeviceCoreAllocated":
			d.CoreAllocated = s.Value
		case "GPUDeviceCoreLimit":
			d.CoreLimit = s.Value
		case "GPUDeviceSharedNum":
			d.SharedContainers = s.Value
		case "nodeGPUMemoryPercentage":
			d.MemoryPercent = s.Value
		case "nodeGPUOverview":
			d.DeviceType = s.Labels["devicetype"]
		}
	}

	out := make([]*deviceState, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].DeviceIndex < out[j].DeviceIndex
	})
	return textResult(out)
}

// get_vgpu_allocation: HAMi exposes per-pod metrics (`vGPUMemoryAllocated`,
// `vGPUCoreAllocated`) labeled with `podname`/`podnamespace`/`deviceuuid`/
// `containeridx`. Filter by namespace+pod_name when given, return all otherwise.
func handleGetVGPUAllocation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	namespace := req.GetString("namespace", "")
	podName := req.GetString("pod_name", "")
	samples, err := scrape(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	type allocKey struct {
		ns, pod, uuid, containerIdx string
	}
	type podAlloc struct {
		Namespace         string  `json:"namespace"`
		PodName           string  `json:"pod_name"`
		Node              string  `json:"node"`
		DeviceUUID        string  `json:"device_uuid"`
		ContainerIndex    string  `json:"container_index"`
		MemoryAllocatedMB float64 `json:"memory_allocated_mb"`
		CoreAllocated     float64 `json:"core_allocated"`
	}
	allocs := map[allocKey]*podAlloc{}
	for _, s := range samples {
		if s.Name != "vGPUMemoryAllocated" && s.Name != "vGPUCoreAllocated" {
			continue
		}
		ns := s.Labels["podnamespace"]
		pod := s.Labels["podname"]
		uuid := s.Labels["deviceuuid"]
		containerIdx := s.Labels["containeridx"]
		if uuid == "" || pod == "" {
			continue
		}
		if namespace != "" && ns != namespace {
			continue
		}
		if podName != "" && pod != podName {
			continue
		}
		k := allocKey{ns, pod, uuid, containerIdx}
		a, ok := allocs[k]
		if !ok {
			a = &podAlloc{
				Namespace:      ns,
				PodName:        pod,
				Node:           s.Labels["nodename"],
				DeviceUUID:     uuid,
				ContainerIndex: containerIdx,
			}
			allocs[k] = a
		}
		switch s.Name {
		case "vGPUMemoryAllocated":
			a.MemoryAllocatedMB = s.Value / (1024 * 1024)
		case "vGPUCoreAllocated":
			a.CoreAllocated = s.Value
		}
	}
	out := make([]*podAlloc, 0, len(allocs))
	for _, a := range allocs {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].PodName < out[j].PodName
	})

	resp := map[string]any{
		"namespace_filter": namespace,
		"pod_filter":       podName,
		"match_count":      len(out),
		"allocations":      out,
	}
	if len(out) == 0 {
		resp["note"] = "No matching pod has an active vGPU allocation. HAMi only emits per-pod " +
			"vGPUMemoryAllocated/vGPUCoreAllocated when a pod has reserved a vGPU through the scheduler."
	}
	return textResult(resp)
}

// run_promql: HAMi exposes Prometheus-format metrics, not a PromQL endpoint. We
// implement metric-name + label-matcher filtering — the most common PromQL shape
// (`metric{label="value"}`). Anything beyond that needs a real Prometheus.
func handleRunPromQL(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}
	name, matchers, err := parseSimpleSelector(query)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	samples, err := scrape(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var out []sample
	for _, s := range samples {
		if name != "" && s.Name != name {
			continue
		}
		ok := true
		for k, v := range matchers {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	resp := map[string]any{
		"query":   query,
		"matched": len(out),
		"results": out,
	}
	return textResult(resp)
}

// parseSimpleSelector parses `metric_name{label="v",label2="v2"}` style queries.
// Returns the metric name (may be empty) and a map of label matchers.
func parseSimpleSelector(q string) (string, map[string]string, error) {
	q = strings.TrimSpace(q)
	matchers := map[string]string{}
	brace := strings.Index(q, "{")
	var name string
	if brace < 0 {
		return q, matchers, nil
	}
	name = strings.TrimSpace(q[:brace])
	end := strings.LastIndex(q, "}")
	if end < 0 || end < brace {
		return "", nil, fmt.Errorf("unterminated label selector: %q", q)
	}
	body := q[brace+1 : end]
	if body == "" {
		return name, matchers, nil
	}
	for _, pair := range splitTopLevel(body, ',') {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.Index(pair, "=")
		if eq < 0 {
			return "", nil, fmt.Errorf("malformed matcher %q (only `=` is supported)", pair)
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		v = strings.Trim(v, `"`)
		matchers[k] = v
	}
	return name, matchers, nil
}

func splitTopLevel(s string, sep rune) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		if r == '"' {
			inQuote = !inQuote
		}
		if r == sep && !inQuote {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// get_cluster_summary: aggregate health across all nodes/devices.
func handleGetClusterSummary(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	samples, err := scrape(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	type devKey struct {
		node, uuid string
	}
	type devTotals struct {
		deviceType        string
		memAllocated      float64
		memLimit          float64
		coreAllocated     float64
		coreLimit         float64
		sharedContainers  float64
		memoryPercent     float64
		hasOverview       bool
	}
	devs := map[devKey]*devTotals{}
	var hamiVersion, hamiBuildDate string
	deviceMetrics := map[string]bool{
		"GPUDeviceMemoryAllocated": true,
		"GPUDeviceMemoryLimit":     true,
		"GPUDeviceCoreAllocated":   true,
		"GPUDeviceCoreLimit":       true,
		"GPUDeviceSharedNum":       true,
		"nodeGPUMemoryPercentage":  true,
		"nodeGPUOverview":          true,
	}

	for _, s := range samples {
		if s.Name == "hami_build_info" {
			hamiVersion = s.Labels["version"]
			hamiBuildDate = s.Labels["build_date"]
			continue
		}
		if !deviceMetrics[s.Name] {
			continue
		}
		nodeID := s.Labels["nodeid"]
		uuid := s.Labels["deviceuuid"]
		if uuid == "" || nodeID == "" {
			continue
		}
		k := devKey{node: nodeID, uuid: uuid}
		d, ok := devs[k]
		if !ok {
			d = &devTotals{}
			devs[k] = d
		}
		switch s.Name {
		case "GPUDeviceMemoryAllocated":
			d.memAllocated = s.Value
		case "GPUDeviceMemoryLimit":
			d.memLimit = s.Value
		case "GPUDeviceCoreAllocated":
			d.coreAllocated = s.Value
		case "GPUDeviceCoreLimit":
			d.coreLimit = s.Value
		case "GPUDeviceSharedNum":
			d.sharedContainers = s.Value
		case "nodeGPUMemoryPercentage":
			d.memoryPercent = s.Value
		case "nodeGPUOverview":
			d.deviceType = s.Labels["devicetype"]
			d.hasOverview = true
		}
	}

	type deviceLine struct {
		Node             string  `json:"node"`
		DeviceUUID       string  `json:"device_uuid"`
		DeviceType       string  `json:"device_type,omitempty"`
		MemAllocatedMB   float64 `json:"memory_allocated_mb"`
		MemLimitMB       float64 `json:"memory_limit_mb"`
		MemoryPercent    float64 `json:"memory_percent"`
		CoreAllocated    float64 `json:"core_allocated"`
		CoreLimit        float64 `json:"core_limit"`
		SharedContainers float64 `json:"shared_containers"`
	}
	var lines []deviceLine
	var totalMemAlloc, totalMemLimit, totalCoreAlloc, totalCoreLimit float64
	totalContainers := 0.0
	nodes := map[string]struct{}{}
	for k, d := range devs {
		nodes[k.node] = struct{}{}
		totalMemAlloc += d.memAllocated
		totalMemLimit += d.memLimit
		totalCoreAlloc += d.coreAllocated
		totalCoreLimit += d.coreLimit
		totalContainers += d.sharedContainers
		lines = append(lines, deviceLine{
			Node:             k.node,
			DeviceUUID:       k.uuid,
			DeviceType:       d.deviceType,
			MemAllocatedMB:   d.memAllocated / (1024 * 1024),
			MemLimitMB:       d.memLimit / (1024 * 1024),
			MemoryPercent:    d.memoryPercent,
			CoreAllocated:    d.coreAllocated,
			CoreLimit:        d.coreLimit,
			SharedContainers: d.sharedContainers,
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].MemoryPercent != lines[j].MemoryPercent {
			return lines[i].MemoryPercent > lines[j].MemoryPercent
		}
		return lines[i].DeviceUUID < lines[j].DeviceUUID
	})

	memPct := 0.0
	if totalMemLimit > 0 {
		memPct = 100 * totalMemAlloc / totalMemLimit
	}
	corePct := 0.0
	if totalCoreLimit > 0 {
		corePct = 100 * totalCoreAlloc / totalCoreLimit
	}

	summary := map[string]any{
		"hami_version":              hamiVersion,
		"hami_build_date":           hamiBuildDate,
		"node_count":                len(nodes),
		"device_count":              len(devs),
		"total_memory_allocated_mb": totalMemAlloc / (1024 * 1024),
		"total_memory_limit_mb":     totalMemLimit / (1024 * 1024),
		"total_memory_free_mb":      (totalMemLimit - totalMemAlloc) / (1024 * 1024),
		"memory_utilization_pct":    memPct,
		"core_utilization_pct":      corePct,
		"total_shared_containers":   totalContainers,
		"devices":                   lines,
	}
	return textResult(summary)
}

func main() {
	s := server.NewMCPServer(
		"hami-mcp",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	s.AddTool(mcp.NewTool("get_gpu_metrics",
		mcp.WithDescription("Return per-GPU vGPU memory and core utilization scraped from HAMi's Prometheus endpoint. Optionally filter to one node."),
		mcp.WithString("node_name",
			mcp.Description("Kubernetes node name (e.g. 'nebius-tarantula'). Empty to return all nodes."),
		),
	), handleGetGPUMetrics)

	s.AddTool(mcp.NewTool("get_vgpu_allocation",
		mcp.WithDescription("Return device-level vGPU allocation (memory MB, cores, shared container count). Note: HAMi's /metrics endpoint does not break down by pod; per-pod attribution requires the Kubernetes API."),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace (currently informational — see note)."),
		),
		mcp.WithString("pod_name",
			mcp.Description("Pod name (currently informational — see note)."),
		),
	), handleGetVGPUAllocation)

	s.AddTool(mcp.NewTool("run_promql",
		mcp.WithDescription("Filter HAMi metrics using a PromQL-style selector. Supports `metric_name{label=\"value\"}` syntax. HAMi exposes /metrics, not a PromQL server, so range queries and functions are not supported."),
		mcp.WithString("query",
			mcp.Description("PromQL-style selector (e.g. `nodeGPUMemoryPercentage{nodeid=\"nebius-tarantula\"}`)."),
			mcp.Required(),
		),
	), handleRunPromQL)

	s.AddTool(mcp.NewTool("get_cluster_summary",
		mcp.WithDescription("Return overall cluster GPU health: HAMi version, node/device counts, total/used/free vGPU memory, top consumers ranked by memory percent."),
	), handleGetClusterSummary)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
