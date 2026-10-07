// Package index walks a repo and incrementally (re)embeds changed files.
package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/chann44/dowse/internal/chunk"
	"github.com/chann44/dowse/internal/config"
	"github.com/chann44/dowse/internal/embed"
	"github.com/chann44/dowse/internal/store"
)

const (
	DirName      = ".dowse"
	DBName       = "index.db"
	maxFileBytes = 1 << 20
	batchSize    = 32
	// EmbeddingGemma's context is 2048 tokens; ~4 chars/token for code.
	maxEmbedChars = 6000
)

var skipDirs = map[string]bool{
	".git": true, DirName: true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, ".venv": true, "venv": true, ".next": true, "coverage": true,
}

// FindRoot walks up from dir to the nearest directory containing .dowse or
// .git, falling back to dir itself.
func FindRoot(dir string) string {
	dir, _ = filepath.Abs(dir)
	for d := dir; ; d = filepath.Dir(d) {
		for _, m := range []string{DirName, ".git"} {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

func DBPath(root string) string { return filepath.Join(root, DirName, DBName) }

type Stats struct {
	Files, Added, Updated, Removed, Unchanged, Chunks int
}

func (s Stats) String() string {
	return fmt.Sprintf("%d files: %d added, %d updated, %d removed, %d unchanged (%d chunks embedded)",
		s.Files, s.Added, s.Updated, s.Removed, s.Unchanged, s.Chunks)
}

type Indexer struct {
	Root     string
	Cfg      config.Config
	Store    *store.Store
	Embed    *embed.Client
	Progress func(done, total int) // optional
}

type pending struct {
	path   string
	state  store.FileState
	chunks []chunk.Chunk
	isNew  bool
}

// Run brings the index in line with the working tree.
func (ix *Indexer) Run(ctx context.Context) (Stats, error) {
	var st Stats
	paths, err := ix.listFiles()
	if err != nil {
		return st, err
	}
	known, err := ix.Store.Files()
	if err != nil {
		return st, err
	}
	st.Files = len(paths)

	seen := map[string]bool{}
	var queue []pending
	queued := 0
	flush := func() error {
		if len(queue) == 0 {
			return nil
		}
		if err := ix.embedAndStore(ctx, queue); err != nil {
			return err
		}
		for _, p := range queue {
			st.Chunks += len(p.chunks)
		}
		queue, queued = queue[:0], 0
		return nil
	}

	for i, rel := range paths {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		seen[rel] = true
		if ix.Progress != nil {
			ix.Progress(i+1, len(paths))
		}
		abs := filepath.Join(ix.Root, rel)
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			seen[rel] = false
			continue
		}
		cur := store.FileState{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		old, had := known[rel]
		if had && old.Size == cur.Size && old.MtimeNs == cur.MtimeNs {
			st.Unchanged++
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			seen[rel] = false
			continue
		}
		sum := sha256.Sum256(src)
		cur.Hash = hex.EncodeToString(sum[:])
		if had && old.Hash == cur.Hash {
			st.Unchanged++
			if err := ix.Store.TouchFile(rel, cur); err != nil {
				return st, err
			}
			continue
		}
		if looksGenerated(src) {
			seen[rel] = false
			continue
		}
		chunks, err := chunk.File(ctx, rel, src)
		if err != nil {
			return st, fmt.Errorf("%s: %w", rel, err)
		}
		if had {
			st.Updated++
		} else {
			st.Added++
		}
		queue = append(queue, pending{path: rel, state: cur, chunks: chunks, isNew: !had})
		queued += len(chunks)
		if queued >= batchSize {
			if err := flush(); err != nil {
				return st, err
			}
		}
	}
	if err := flush(); err != nil {
		return st, err
	}
	for p := range known {
		if !seen[p] {
			if err := ix.Store.RemoveFile(p); err != nil {
				return st, err
			}
			st.Removed++
		}
	}
	return st, nil
}

func (ix *Indexer) embedAndStore(ctx context.Context, queue []pending) error {
	var titles, texts []string
	for _, p := range queue {
		for _, c := range p.chunks {
			titles = append(titles, c.Name)
			text := p.path + "\n" + c.Content
			if len(text) > maxEmbedChars {
				text = text[:maxEmbedChars]
			}
			texts = append(texts, text)
		}
	}
	var embs [][]float32
	for i := 0; i < len(texts); i += batchSize {
		j := min(i+batchSize, len(texts))
		e, err := ix.Embed.Documents(ctx, titles[i:j], texts[i:j])
		if err != nil {
			return err
		}
		embs = append(embs, e...)
	}
	off := 0
	for _, p := range queue {
		n := len(p.chunks)
		if err := ix.Store.ReplaceFile(p.path, p.state, p.chunks, embs[off:off+n]); err != nil {
			return fmt.Errorf("%s: %w", p.path, err)
		}
		off += n
	}
	return nil
}

func (ix *Indexer) listFiles() ([]string, error) {
	var paths []string
	if out, err := gitLsFiles(ix.Root); err == nil {
		paths = out
	} else {
		err := filepath.WalkDir(ix.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != ix.Root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(ix.Root, p)
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var out []string
	for _, p := range paths {
		if !chunk.Supported(p) || ix.ignored(p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (ix *Indexer) ignored(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if skipDirs[seg] {
			return true
		}
	}
	for _, pat := range ix.Cfg.Ignore {
		if strings.HasSuffix(pat, "/") {
			if strings.HasPrefix(p, pat) || strings.Contains(p, "/"+pat) {
				return true
			}
			continue
		}
		if ok, _ := doublestar.Match(pat, p); ok {
			return true
		}
	}
	return false
}

// gitLsFiles lists tracked and untracked-but-not-ignored files.
func gitLsFiles(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out.Bytes(), []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no files")
	}
	return files, nil
}

// looksGenerated skips minified bundles and generated code.
func looksGenerated(src []byte) bool {
	head := src[:min(len(src), 2048)]
	if bytes.Contains(head, []byte("Code generated")) && bytes.Contains(head, []byte("DO NOT EDIT")) {
		return true
	}
	lines := bytes.Count(src, []byte{'\n'}) + 1
	return len(src)/lines > 500
}
