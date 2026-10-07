// Package store keeps chunks, their embeddings and a keyword index in one
// SQLite file. Vectors live in a sqlite-vec vec0 table (exact KNN, cosine);
// keywords live in a plain postings table scored with BM25 in Go, so no
// SQLite build tags (FTS5) are needed and `go install` just works.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/chann44/dowse/internal/chunk"
)

// SchemaVersion is bumped whenever the on-disk format changes.
const SchemaVersion = "1"

var ErrIncompatible = errors.New("index was built with a different model, dimensions or format; run `dowse index --force`")

func init() { vec.Auto() }

type Store struct {
	DB   *sql.DB
	Dims int
}

type FileState struct {
	Size    int64
	MtimeNs int64
	Hash    string
}

// Open opens (creating if needed) the index at path. If force is set the
// existing index is discarded.
func Open(path, model string, dims int, force bool) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if force {
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suf)
		}
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Dims: dims}
	if err := s.init(model); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) init(model string) error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	want := map[string]string{"schema": SchemaVersion, "model": model, "dims": strconv.Itoa(s.Dims)}
	rows, err := s.DB.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		have[k] = v
	}
	rows.Close()
	if len(have) > 0 {
		for k, v := range want {
			if have[k] != v {
				return ErrIncompatible
			}
		}
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, hash TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY, path TEXT NOT NULL, name TEXT NOT NULL, kind TEXT NOT NULL,
			signature TEXT NOT NULL, start_line INTEGER NOT NULL, end_line INTEGER NOT NULL,
			content TEXT NOT NULL, ntokens INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS chunks_path ON chunks(path)`,
		`CREATE TABLE IF NOT EXISTS postings (
			term TEXT NOT NULL, chunk_id INTEGER NOT NULL, tf INTEGER NOT NULL,
			PRIMARY KEY (term, chunk_id)) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS postings_chunk ON postings(chunk_id)`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(embedding float[%d] distance_metric=cosine)`, s.Dims),
	}
	for _, q := range stmts {
		if _, err := s.DB.Exec(q); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	for k, v := range want {
		if _, err := s.DB.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

// Files returns the recorded state of every indexed file.
func (s *Store) Files() (map[string]FileState, error) {
	rows, err := s.DB.Query(`SELECT path, size, mtime_ns, hash FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileState{}
	for rows.Next() {
		var p string
		var f FileState
		if err := rows.Scan(&p, &f.Size, &f.MtimeNs, &f.Hash); err != nil {
			return nil, err
		}
		out[p] = f
	}
	return out, rows.Err()
}

// TouchFile updates size/mtime for a file whose content hash is unchanged.
func (s *Store) TouchFile(path string, st FileState) error {
	_, err := s.DB.Exec(`UPDATE files SET size = ?, mtime_ns = ? WHERE path = ?`, st.Size, st.MtimeNs, path)
	return err
}

// RemoveFile deletes a file and all of its chunks.
func (s *Store) RemoveFile(path string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteChunks(tx, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceFile atomically swaps a file's chunks for new ones.
// embs[i] is the embedding of chunks[i].
func (s *Store) ReplaceFile(path string, st FileState, chunks []chunk.Chunk, embs [][]float32) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteChunks(tx, path); err != nil {
		return err
	}
	insChunk, err := tx.Prepare(`INSERT INTO chunks(path, name, kind, signature, start_line, end_line, content, ntokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insChunk.Close()
	insVec, err := tx.Prepare(`INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer insVec.Close()
	insPost, err := tx.Prepare(`INSERT INTO postings(term, chunk_id, tf) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insPost.Close()

	for i, c := range chunks {
		tf, n := termFreqs(path, c)
		res, err := insChunk.Exec(path, c.Name, c.Kind, c.Signature, c.StartLine, c.EndLine, c.Content, n)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		blob, err := vec.SerializeFloat32(embs[i])
		if err != nil {
			return err
		}
		if _, err := insVec.Exec(id, blob); err != nil {
			return err
		}
		for term, f := range tf {
			if _, err := insPost.Exec(term, id, f); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO files(path, size, mtime_ns, hash) VALUES (?, ?, ?, ?)`,
		path, st.Size, st.MtimeNs, st.Hash); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteChunks(tx *sql.Tx, path string) error {
	for _, q := range []string{
		`DELETE FROM vec_chunks WHERE rowid IN (SELECT id FROM chunks WHERE path = ?)`,
		`DELETE FROM postings WHERE chunk_id IN (SELECT id FROM chunks WHERE path = ?)`,
		`DELETE FROM chunks WHERE path = ?`,
	} {
		if _, err := tx.Exec(q, path); err != nil {
			return err
		}
	}
	return nil
}

// termFreqs tokenizes a chunk for keyword search. The name, signature and
// path are weighted above the body so identifier hits rank first.
func termFreqs(path string, c chunk.Chunk) (map[string]int, int) {
	tf := map[string]int{}
	n := 0
	add := func(s string, w int) {
		for _, t := range Tokenize(s) {
			tf[t] += w
			n++
		}
	}
	add(c.Name, 4)
	add(c.Signature, 2)
	add(filepath.ToSlash(path), 1)
	add(c.Content, 1)
	return tf, n
}

// Stats summarises the index.
func (s *Store) Stats() (files, chunks int, err error) {
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		return
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks)
	return
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
