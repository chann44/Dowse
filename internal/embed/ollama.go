// Package embed computes EmbeddingGemma vectors through a local Ollama server.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// EmbeddingGemma is trained with task prefixes; using them noticeably improves
// retrieval quality over raw text.
const (
	queryPrefix = "task: code retrieval | query: "
	docPrefix   = "title: %s | text: %s"
)

type Client struct {
	URL   string
	Model string
	Dims  int // Matryoshka truncation: 768, 512, 256 or 128
	http  *http.Client
}

func New(url, model string, dims int) *Client {
	return &Client{URL: url, Model: model, Dims: dims, http: &http.Client{Timeout: 5 * time.Minute}}
}

// Query embeds a search query.
func (c *Client) Query(ctx context.Context, q string) ([]float32, error) {
	vs, err := c.embed(ctx, []string{queryPrefix + q})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// Documents embeds code chunks. titles[i] names texts[i] (e.g. the function name).
func (c *Client) Documents(ctx context.Context, titles, texts []string) ([][]float32, error) {
	in := make([]string, len(texts))
	for i := range texts {
		t := titles[i]
		if t == "" {
			t = "none"
		}
		in[i] = fmt.Sprintf(docPrefix, t, texts[i])
	}
	return c.embed(ctx, in)
}

// Ping checks that Ollama is reachable and the model is pulled.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.embed(ctx, []string{"ping"})
	return err
}

type embedReq struct {
	Model    string         `json:"model"`
	Input    []string       `json:"input"`
	Truncate bool           `json:"truncate"`
	Options  map[string]any `json:"options,omitempty"`
}

type embedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

func (c *Client) embed(ctx context.Context, input []string) ([][]float32, error) {
	body, _ := json.Marshal(embedReq{
		Model:    c.Model,
		Input:    input,
		Truncate: true,
		Options:  map[string]any{"num_ctx": 2048},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama not reachable at %s (is it running? `ollama serve`): %w", c.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out embedResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ollama: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	if out.Error != "" || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: %s (try `ollama pull %s`)", out.Error, c.Model)
	}
	if len(out.Embeddings) != len(input) {
		return nil, fmt.Errorf("ollama returned %d embeddings for %d inputs", len(out.Embeddings), len(input))
	}
	for i, v := range out.Embeddings {
		if len(v) < c.Dims {
			return nil, fmt.Errorf("model %s returns %d dims, config wants %d", c.Model, len(v), c.Dims)
		}
		out.Embeddings[i] = normalize(v[:c.Dims])
	}
	return out.Embeddings, nil
}

// normalize L2-normalizes v in place. Required after Matryoshka truncation.
func normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}
