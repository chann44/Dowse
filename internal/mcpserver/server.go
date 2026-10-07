// Package mcpserver exposes dowse as an MCP server with one search_code tool.
package mcpserver

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chann44/dowse/internal/index"
	"github.com/chann44/dowse/internal/store"
)

const toolDescription = `Semantic code search over this repository. Finds functions, methods and types by what they DO, not what they are named.

Use it for concept questions where you don't know the identifier, e.g. "where is retry backoff handled?", "how are webhook signatures validated?", "what writes the user session cookie?". Results are whole functions with file path and line range, ranked by fused vector + keyword relevance.

Prefer grep/glob when you already know an exact name or string.`

// instructions are shown to the agent up front. Claude Code defers MCP tool
// schemas, so without this the agent only sees the tool name and falls back
// to grep for everything.
const instructions = `dowse is a local semantic index of this repository. When you need to find where something happens or how a behaviour is implemented and you don't already know the exact identifier or string, call search_code FIRST with a plain-English description, before Grep/Glob. One search_code call usually returns the right function with its code, replacing several rounds of grep and file reads. Use Grep when you know the exact name or text.`

type SearchInput struct {
	Query       string `json:"query" jsonschema:"natural-language description of the code you are looking for"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max results (default 8, max 30)"`
	Path        string `json:"path,omitempty" jsonschema:"optional repo-relative path prefix to restrict results, e.g. internal/http/"`
	IncludeCode *bool  `json:"include_code,omitempty" jsonschema:"include each result's source (default true); false returns only locations and signatures"`
}

type Server struct {
	ix *index.Indexer

	mu       sync.Mutex
	running  chan struct{} // closed when the current refresh finishes; nil if idle
	progress [2]int
	lastErr  error
}

func New(ix *index.Indexer) *Server {
	s := &Server{ix: ix}
	ix.Progress = func(done, total int) {
		s.mu.Lock()
		s.progress = [2]int{done, total}
		s.mu.Unlock()
	}
	return s
}

func (s *Server) Run(ctx context.Context, version string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "dowse", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	mcp.AddTool(srv, &mcp.Tool{Name: "search_code", Description: toolDescription}, s.searchCode)
	s.refresh(ctx) // start indexing right away so the first search is fast
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// refresh starts an incremental index run if none is in flight and returns
// a channel closed when it finishes.
func (s *Server) refresh(ctx context.Context) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != nil {
		return s.running
	}
	done := make(chan struct{})
	s.running = done
	go func() {
		st, err := s.ix.Run(ctx)
		if err != nil {
			log.Printf("dowse: index: %v", err)
		} else if st.Added+st.Updated+st.Removed > 0 {
			log.Printf("dowse: %s", st)
		}
		s.mu.Lock()
		s.lastErr = err
		s.running = nil
		s.mu.Unlock()
		close(done)
	}()
	return done
}

func (s *Server) searchCode(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, nil, fmt.Errorf("query is required")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 8
	}
	limit = min(limit, 30)

	var note string
	select {
	case <-s.refresh(context.WithoutCancel(ctx)):
		s.mu.Lock()
		if s.lastErr != nil {
			note = fmt.Sprintf("note: index refresh failed (%v); results may be stale\n\n", s.lastErr)
		}
		s.mu.Unlock()
	case <-time.After(5 * time.Second):
		s.mu.Lock()
		note = fmt.Sprintf("note: index still building (%d/%d files scanned); results may be incomplete\n\n", s.progress[0], s.progress[1])
		s.mu.Unlock()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	emb, err := s.ix.Embed.Query(ctx, in.Query)
	if err != nil {
		return nil, nil, err
	}
	res, err := s.ix.Store.Search(store.Query{Text: in.Query, Embedding: emb, Limit: limit, PathPrefix: in.Path})
	if err != nil {
		return nil, nil, err
	}
	withCode := in.IncludeCode == nil || *in.IncludeCode
	text := note + Format(res, withCode)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

const maxCodeLines = 80

// Format renders results compactly for an LLM.
func Format(res []store.Result, withCode bool) string {
	if len(res) == 0 {
		return "No matches. The index may be empty for this path, or try rephrasing."
	}
	var b strings.Builder
	for i, r := range res {
		fmt.Fprintf(&b, "%d. %s:%d-%d  %s  (similarity %.2f)\n", i+1, r.Path, r.StartLine, r.EndLine, r.Signature, r.Similarity)
		if withCode {
			lines := strings.Split(r.Content, "\n")
			if len(lines) > maxCodeLines {
				lines = append(lines[:maxCodeLines], fmt.Sprintf("... (%d more lines)", len(lines)-maxCodeLines))
			}
			b.WriteString("```\n" + strings.Join(lines, "\n") + "\n```\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
