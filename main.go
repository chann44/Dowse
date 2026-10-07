// Command dowse is a local semantic code search engine for Claude Code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/chann44/dowse/internal/config"
	"github.com/chann44/dowse/internal/embed"
	"github.com/chann44/dowse/internal/index"
	"github.com/chann44/dowse/internal/mcpserver"
	"github.com/chann44/dowse/internal/store"
)

var version = "dev"

const usage = `dowse - local semantic code search

Usage:
  dowse "<question>"          search the current repo (indexes changed files first)
  dowse index [path] [--force] build or update the index
  dowse mcp                    run the MCP server (stdio) for Claude Code
  dowse hook install           re-index after every git commit
  dowse status                 show index stats

Search flags:
  -n int      number of results (default 10)
  -code       print the code of each result

Set up Claude Code with:  claude mcp add dowse -- dowse mcp
`

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dowse:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "index":
		return cmdIndex(ctx, args[1:])
	case "mcp":
		return cmdMCP(ctx)
	case "hook":
		return cmdHook(args[1:])
	case "status":
		return cmdStatus()
	}
	return cmdSearch(ctx, args)
}

func open(dir string, force bool) (*index.Indexer, error) {
	root := index.FindRoot(dir)
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(index.DBPath(root), cfg.Model, cfg.Dimensions, force)
	if err != nil {
		return nil, err
	}
	ensureGitignored(root)
	return &index.Indexer{
		Root:  root,
		Cfg:   cfg,
		Store: st,
		Embed: embed.New(cfg.OllamaURL, cfg.Model, cfg.Dimensions),
	}, nil
}

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	force := fs.Bool("force", false, "discard the existing index and rebuild")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	ix, err := open(dir, *force)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	if err := ix.Embed.Ping(ctx); err != nil {
		return err
	}
	tty := isTerminal(os.Stderr)
	ix.Progress = func(done, total int) {
		if tty && (done%25 == 0 || done == total) {
			fmt.Fprintf(os.Stderr, "\rscanning %d/%d files", done, total)
		}
	}
	st, err := ix.Run(ctx)
	if tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "indexed %s: %s\n", ix.Root, st)
	return nil
}

func cmdSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	n := fs.Int("n", 10, "number of results")
	code := fs.Bool("code", false, "print code")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	q := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(q) == "" {
		return errors.New("empty query")
	}
	ix, err := open(".", false)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	if st, err := ix.Run(ctx); err != nil {
		return err
	} else if st.Added+st.Updated+st.Removed > 0 {
		fmt.Fprintf(os.Stderr, "updated index: %s\n", st)
	}
	emb, err := ix.Embed.Query(ctx, q)
	if err != nil {
		return err
	}
	res, err := ix.Store.Search(store.Query{Text: q, Embedding: emb, Limit: *n})
	if err != nil {
		return err
	}
	if *code {
		fmt.Print(mcpserver.Format(res, true))
		return nil
	}
	if len(res) == 0 {
		fmt.Println("no matches")
	}
	w := 0
	for _, r := range res {
		w = max(w, len(fmt.Sprintf("%s:%d", r.Path, r.StartLine)))
	}
	for _, r := range res {
		loc := fmt.Sprintf("%s:%d", r.Path, r.StartLine)
		fmt.Printf("%-*s  %s\n", w, loc, r.Signature)
	}
	return nil
}

func cmdMCP(ctx context.Context) error {
	// stdout is the MCP transport; logs must go to stderr.
	log.SetOutput(os.Stderr)
	wd, _ := os.Getwd()
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		wd = d
	}
	ix, err := open(wd, false)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	return mcpserver.New(ix).Run(ctx, version)
}

func cmdStatus() error {
	ix, err := open(".", false)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	files, chunks, err := ix.Store.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("root:   %s\nindex:  %s\nmodel:  %s (%d dims)\nfiles:  %d\nchunks: %d\n",
		ix.Root, index.DBPath(ix.Root), ix.Cfg.Model, ix.Cfg.Dimensions, files, chunks)
	return nil
}

const hookScript = `#!/bin/sh
# dowse: re-index changed files in the background after each commit
command -v dowse >/dev/null 2>&1 && (dowse index . >/dev/null 2>&1 &)
`

func cmdHook(args []string) error {
	if len(args) != 1 || args[0] != "install" {
		return errors.New("usage: dowse hook install")
	}
	root := index.FindRoot(".")
	hooks := filepath.Join(root, ".git", "hooks")
	if _, err := os.Stat(hooks); err != nil {
		return fmt.Errorf("%s is not a git repo", root)
	}
	p := filepath.Join(hooks, "post-commit")
	existing, err := os.ReadFile(p)
	switch {
	case err == nil && strings.Contains(string(existing), "dowse"):
		fmt.Println("hook already installed:", p)
		return nil
	case err == nil:
		// Append to an existing hook rather than clobbering it.
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.WriteString(f, "\n"+strings.SplitN(hookScript, "\n", 2)[1])
		if err != nil {
			return err
		}
	default:
		if err := os.WriteFile(p, []byte(hookScript), 0o755); err != nil {
			return err
		}
	}
	fmt.Println("installed", p)
	return nil
}

// ensureGitignored keeps the index out of git by giving .dowse its own .gitignore.
func ensureGitignored(root string) {
	p := filepath.Join(root, index.DirName, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.WriteFile(p, []byte("*\n"), 0o644)
}

// reorder moves flags before positional args so `dowse index . --force` works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if (a == "-n" || a == "--n") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
