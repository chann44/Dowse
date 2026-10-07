package store

import (
	"fmt"
	"math"
	"sort"
	"strings"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

type Result struct {
	Path      string  `json:"path"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Signature string  `json:"signature"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Content   string  `json:"content,omitempty"`
	Score     float64 `json:"score"`
	// Similarity is cosine similarity to the query (0 if only a keyword hit).
	Similarity float64 `json:"similarity"`
}

type Query struct {
	Text       string
	Embedding  []float32
	Limit      int
	PathPrefix string // optional: only return results under this path
}

const (
	candidates = 50 // per ranker, before fusion
	// vectorWeight is the share of the final score from semantic similarity;
	// the rest comes from BM25. Semantic dominates because concept queries
	// are dowse's job; keyword still lifts exact identifier matches.
	vectorWeight = 0.7
	bm25K1       = 1.2
	bm25B        = 0.75
)

// Search runs vector and keyword search over the union of both candidate
// sets and fuses them as a weighted sum of min-max normalized scores.
//
// Rank fusion (RRF) was tried first, but it rewards chunks that show up in
// both lists even at mediocre ranks, so a top vector hit with no keyword
// overlap ("is the embedding server alive" -> Ping) lost to keyword noise.
// Score fusion keeps how confident each ranker actually is.
func (s *Store) Search(q Query) ([]Result, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	k := candidates
	if q.PathPrefix != "" {
		k = candidates * 8 // filter happens after KNN
	}
	vecIDs, sims, err := s.vectorSearch(q.Embedding, k)
	if err != nil {
		return nil, err
	}
	kwIDs, bm25, err := s.keywordSearch(q.Text, k)
	if err != nil {
		return nil, err
	}

	ids := vecIDs
	var missing []int64
	for _, id := range kwIDs {
		if _, ok := sims[id]; !ok {
			ids = append(ids, id)
			missing = append(missing, id)
		}
	}
	if err := s.similarities(q.Embedding, missing, sims); err != nil {
		return nil, err
	}

	vmin, vmax := math.Inf(1), math.Inf(-1)
	kmax := 0.0
	for _, id := range ids {
		vmin, vmax = math.Min(vmin, sims[id]), math.Max(vmax, sims[id])
		kmax = math.Max(kmax, bm25[id])
	}
	score := make(map[int64]float64, len(ids))
	for _, id := range ids {
		v := 1.0
		if vmax > vmin {
			v = (sims[id] - vmin) / (vmax - vmin)
		}
		kw := 0.0
		if kmax > 0 {
			kw = bm25[id] / kmax
		}
		score[id] = vectorWeight*v + (1-vectorWeight)*kw
	}
	sort.SliceStable(ids, func(a, b int) bool { return score[ids[a]] > score[ids[b]] })

	byID, err := s.load(ids)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, id := range ids {
		r, ok := byID[id]
		if !ok || (q.PathPrefix != "" && !strings.HasPrefix(r.Path, q.PathPrefix)) {
			continue
		}
		r.Score = score[id]
		r.Similarity = sims[id]
		out = append(out, r)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// similarities fills in cosine similarity for chunks found only by keyword.
func (s *Store) similarities(emb []float32, ids []int64, into map[int64]float64) error {
	if len(ids) == 0 {
		return nil
	}
	blob, err := vec.SerializeFloat32(emb)
	if err != nil {
		return err
	}
	args := []any{blob}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.Query(`SELECT rowid, vec_distance_cosine(embedding, ?) FROM vec_chunks
		WHERE rowid IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var d float64
		if err := rows.Scan(&id, &d); err != nil {
			return err
		}
		into[id] = 1 - d
	}
	return rows.Err()
}

func (s *Store) vectorSearch(emb []float32, k int) ([]int64, map[int64]float64, error) {
	blob, err := vec.SerializeFloat32(emb)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.DB.Query(`SELECT rowid, distance FROM vec_chunks WHERE embedding MATCH ? AND k = ? ORDER BY distance`, blob, k)
	if err != nil {
		return nil, nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()
	var ids []int64
	sims := map[int64]float64{}
	for rows.Next() {
		var id int64
		var d float64
		if err := rows.Scan(&id, &d); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		sims[id] = 1 - d
	}
	return ids, sims, rows.Err()
}

func (s *Store) keywordSearch(text string, k int) ([]int64, map[int64]float64, error) {
	terms := dedupe(Tokenize(text))
	if len(terms) == 0 {
		return nil, nil, nil
	}
	var n int
	var avgdl float64
	if err := s.DB.QueryRow(`SELECT COUNT(*), COALESCE(AVG(ntokens), 0) FROM chunks`).Scan(&n, &avgdl); err != nil {
		return nil, nil, err
	}
	if n == 0 {
		return nil, nil, nil
	}
	args := make([]any, len(terms))
	for i, t := range terms {
		args[i] = t
	}
	df := map[string]int{}
	rows, err := s.DB.Query(`SELECT term, COUNT(*) FROM postings WHERE term IN (`+placeholders(len(terms))+`) GROUP BY term`, args...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return nil, nil, err
		}
		df[t] = c
	}
	rows.Close()

	rows, err = s.DB.Query(`SELECT p.term, p.chunk_id, p.tf, c.ntokens FROM postings p JOIN chunks c ON c.id = p.chunk_id
		WHERE p.term IN (`+placeholders(len(terms))+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	score := map[int64]float64{}
	for rows.Next() {
		var t string
		var id int64
		var tf, dl int
		if err := rows.Scan(&t, &id, &tf, &dl); err != nil {
			return nil, nil, err
		}
		idf := math.Log(1 + (float64(n)-float64(df[t])+0.5)/(float64(df[t])+0.5))
		f := float64(tf)
		score[id] += idf * f * (bm25K1 + 1) / (f + bm25K1*(1-bm25B+bm25B*float64(dl)/avgdl))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return score[ids[a]] > score[ids[b]] })
	if len(ids) > k {
		ids = ids[:k]
	}
	return ids, score, nil
}

func (s *Store) load(ids []int64) (map[int64]Result, error) {
	out := map[int64]Result{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.Query(`SELECT id, path, name, kind, signature, start_line, end_line, content FROM chunks
		WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r Result
		if err := rows.Scan(&id, &r.Path, &r.Name, &r.Kind, &r.Signature, &r.StartLine, &r.EndLine, &r.Content); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
