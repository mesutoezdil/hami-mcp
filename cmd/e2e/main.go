// e2e is a small client that proves the full chain:
//
//	HAMi /metrics → hami-mcp-server (stdio JSON-RPC) → local LLM (OpenAI API).
//
// It launches the server as a subprocess, calls get_cluster_summary, and asks
// the LLM to interpret the result.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type mcpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

func startServer(ctx context.Context, bin string) (*mcpClient, error) {
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), nextID: 1}
	if err := c.initialize(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *mcpClient) Close() {
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
}

func (c *mcpClient) send(req rpcReq) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

func (c *mcpClient) recv() (*rpcResp, error) {
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var r rpcResp
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("decode response %q: %w", line, err)
	}
	return &r, nil
}

func (c *mcpClient) initialize() error {
	id := c.nextID
	c.nextID++
	if err := c.send(rpcReq{
		JSONRPC: "2.0", ID: id, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e", "version": "0.1.0"},
		},
	}); err != nil {
		return err
	}
	resp, err := c.recv()
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize: %s", resp.Error.Message)
	}
	return c.send(rpcReq{JSONRPC: "2.0", Method: "notifications/initialized"})
}

func (c *mcpClient) callTool(name string, args map[string]any) (string, error) {
	id := c.nextID
	c.nextID++
	if err := c.send(rpcReq{
		JSONRPC: "2.0", ID: id, Method: "tools/call",
		Params: map[string]any{"name": name, "arguments": args},
	}); err != nil {
		return "", err
	}
	resp, err := c.recv()
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("%s: %s", name, resp.Error.Message)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("%s returned no content", name)
	}
	return result.Content[0].Text, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatResp struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func askLLM(ctx context.Context, baseURL, model, system, user string) (string, error) {
	body, _ := json.Marshal(chatReq{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.2,
		MaxTokens:   400,
		Stream:      false,
	})
	url := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out chatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices: %s", string(raw))
	}
	return out.Choices[0].Message.Content, nil
}

func main() {
	bin := flag.String("server", "./hami-mcp-server", "path to hami-mcp-server binary")
	llmURL := flag.String("llm-url", "http://localhost:11434", "OpenAI-compatible base URL (Ollama default)")
	model := flag.String("model", "llama3.2:3b", "model to query")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("==> Starting hami-mcp-server")
	c, err := startServer(ctx, *bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start server: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	fmt.Println("==> tools/call get_cluster_summary")
	summary, err := c.callTool("get_cluster_summary", map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "call: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(summary)

	fmt.Println("\n==> tools/call get_vgpu_allocation")
	allocs, err := c.callTool("get_vgpu_allocation", map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "call: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(allocs)

	prompt := fmt.Sprintf(`HAMi cluster summary (JSON):
%s

Per-pod vGPU allocations (JSON):
%s

Based on these GPU metrics, what is the current state of this cluster and are there any concerns? Keep your answer to 4-6 sentences.`, summary, allocs)

	fmt.Printf("\n==> Asking %s at %s\n", *model, *llmURL)
	answer, err := askLLM(ctx, *llmURL, *model,
		"You are a Kubernetes GPU operator. Be concise and specific.",
		prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n==> Model answer")
	fmt.Println(answer)
}
