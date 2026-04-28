package engine

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
)

const semanticVectorDim = 256

func buildHashEmbedding(text string, dim int) []float32 {
	if dim <= 0 {
		dim = semanticVectorDim
	}
	vec := make([]float32, dim)
	if strings.TrimSpace(text) == "" {
		return vec
	}

	normalized := strings.ToLower(text)
	tokens := splitTokens(normalized)
	if len(tokens) == 0 {
		tokens = []string{normalized}
	}

	for _, tok := range tokens {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum32()
		idx := int(sum % uint32(dim))
		sign := float32(1)
		if (sum>>31)&1 == 1 {
			sign = -1
		}
		vec[idx] += sign
	}

	// L2 normalize for cosine-style matching stability.
	var norm float64
	for _, v := range vec {
		norm += float64(v * v)
	}
	if norm <= 0 {
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

func splitTokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\n', '\t', '\r', ',', '.', ';', ':', '!', '?', '，', '。', '；', '：', '！', '？', '、', '(', ')', '[', ']', '{', '}', '<', '>', '"', '\'', '`', '/', '\\':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (s *Service) buildSemanticMemoryContext(ctx context.Context, agentID, userContent string) string {
	hits, err := s.store.SearchVectorMemory(ctx, buildHashEmbedding(userContent, semanticVectorDim), 3, agentID)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	for i, hit := range hits {
		b.WriteString(fmt.Sprintf("%d. [%s] (distance=%.4f) %s\n", i+1, hit.CreatedAt.Format("2006-01-02 15:04"), hit.Distance, strings.TrimSpace(hit.Content)))
	}
	return strings.TrimSpace(b.String())
}
