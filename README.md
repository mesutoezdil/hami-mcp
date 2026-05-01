# hami-mcp

A small MCP server in Go that reads HAMi vGPU metrics from a Kubernetes cluster and exposes them as tools any LLM can call.

If you run HAMi on Kubernetes, you already get a Prometheus endpoint full of useful numbers about your GPUs. This server scrapes that endpoint and turns it into four tool calls. Plug it into any MCP client (Claude Desktop, an editor extension, your own CLI) and ask questions like "is the L40S on tarantula full?" or "which pod is sitting on that 8 GiB?"

## What is in this repo

```
.
├── main.go                 the MCP server, four tools, stdio transport
├── cmd/e2e/main.go         a tiny client that drives the server end to end
├── k8s-vgpu-workload.yaml  a demo pod that holds an 8 GiB vGPU reservation
├── test_stdio.sh           shell helper for hand-testing one tool call
├── go.mod / go.sum         Go module and locked deps
└── README.md               this file
```

The four tools are:

- `get_cluster_summary` returns total vGPU memory, used and free, plus a per device list ranked by utilization.
- `get_gpu_metrics` returns per device numbers, optionally filtered to one node.
- `get_vgpu_allocation` returns per pod attribution. This is the one most people are looking for.
- `run_promql` accepts a metric name and label matchers like `nodeGPUMemoryPercentage{nodeid="my-node"}`. HAMi exposes a /metrics endpoint, not a real PromQL server, so range queries and functions are not supported. The tool description says so.

## Requirements

- Go 1.25 or newer. Older versions will fail because `mcp-go` v0.50 needs the new toolchain.
- A Kubernetes cluster with HAMi installed and the device plugin reachable on `:31993/metrics`.
- For the optional end to end test, an OpenAI compatible LLM endpoint. Ollama works fine.

## Build

```
git clone git@github.com:mesutoezdil/GPUClusterHAMiMCPserverinGo.git hami-mcp
cd hami-mcp
go build -o hami-mcp-server .
go build -o e2e ./cmd/e2e/
```

You should now have two binaries in the project root, `hami-mcp-server` (the MCP server) and `e2e` (the test client).

## Confirm HAMi is reachable

```
curl -s http://localhost:31993/metrics | head -10
```

You should see lines that start with `GPUDeviceCoreAllocated` and `GPUDeviceMemoryAllocated`. If you do not, the device plugin pod is either not running or its NodePort is not 31993. Adjust with the env var `HAMI_METRICS_URL` when you start the server.

## Hand test one tool call

```
./test_stdio.sh ./hami-mcp-server get_cluster_summary '{}'
```

This sends an MCP initialize handshake, the `notifications/initialized` message, and one `tools/call` for `get_cluster_summary`. The script prints the raw JSON-RPC responses. Look for a `result.content[0].text` field that contains a JSON object describing your cluster.

## Make the metrics interesting

A clean cluster reports zeros for everything. To see the tools do real work, deploy a pod that asks for a vGPU slice.

```
kubectl apply -f k8s-vgpu-workload.yaml
kubectl get pod hami-demo-workload
```

The manifest requests `nvidia.com/gpu: 1`, `nvidia.com/gpumem: 8192` (MiB), and `nvidia.com/gpucores: 30` (percent). Once the pod is Running, the next scrape of `:31993/metrics` will include `vGPUMemoryAllocated` and `vGPUCoreAllocated` series with `podname="hami-demo-workload"`.

To remove it later:

```
kubectl delete -f k8s-vgpu-workload.yaml
```

## Run the end to end test

The `e2e` binary spawns the MCP server, calls two tools, and asks an LLM to interpret the JSON it gets back.

```
./e2e --server ./hami-mcp-server --llm-url http://localhost:11434 --model llama3.2:3b
```

Defaults match a local Ollama install. Override with flags if you are pointing at vLLM, OpenRouter, or anything else that speaks the OpenAI chat completions shape.

## Use it from Claude Desktop

Add an entry like this to your Claude Desktop MCP config (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):

```json
{
  "mcpServers": {
    "hami": {
      "command": "/absolute/path/to/hami-mcp-server",
      "env": {
        "HAMI_METRICS_URL": "http://localhost:31993/metrics"
      }
    }
  }
}
```

Restart Claude Desktop and the four tools show up under the hammer icon.

## Configuration

The server reads one environment variable.

| Variable           | Default                          | Purpose                                           |
| ------------------ | -------------------------------- | ------------------------------------------------- |
| `HAMI_METRICS_URL` | `http://localhost:31993/metrics` | Where to scrape HAMi from. Set this for remote clusters. |

If you want the server to reach a remote cluster, run a `kubectl port-forward` against the device plugin pod and point `HAMI_METRICS_URL` at the local port.

## Known sharp edges

- Two label families. HAMi uses `nodeid` on device level metrics and `nodename` on per pod metrics. Do not write a single dedup key that mixes them, you will see phantom devices.
- The `prometheus/common` text parser keeps a private validation scheme. Setting the global `model.NameValidationScheme` does not help. Construct the parser with `expfmt.NewTextParser(model.UTF8Validation)` instead, this repo does that in `scrape()`.
- If the `vGPUmonitor` container in the HAMi device plugin pod crashloops with `failed to watch lock file: too many open files`, raise the inotify limits on the host: `sudo sysctl -w fs.inotify.max_user_instances=8192`.

## License

MIT.
