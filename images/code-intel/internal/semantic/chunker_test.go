package semantic

import (
	"context"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/images/code-intel/internal/symbols"
)

type outlineStub struct {
	outline symbols.Outline
	err     error
}

func (s outlineStub) Outline(context.Context, string, []byte, string) (symbols.Outline, error) {
	return s.outline, s.err
}

func TestStructuralChunksUseTopLevelDeclarationExtents(t *testing.T) {
	content := []byte("package sample\n\nfunc Alpha() {\n\tBeta()\n}\n")
	chunker := Chunker{Outliner: outlineStub{outline: symbols.Outline{
		Language: symbols.LanguageGo,
		Symbols: []symbols.Symbol{
			{Name: "Alpha", Kind: symbols.KindFunction, StartLine: 3, EndLine: 5, StartByte: 16, EndByte: len(content), Depth: 0},
			{Name: "nested", Kind: symbols.KindVariable, StartLine: 4, EndLine: 4, StartByte: 32, EndByte: 36, Depth: 1},
		},
	}}}
	chunks, err := chunker.Chunks(context.Background(), "sample.go", content, "digest")
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want one top-level declaration", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Precision != "syntax" || chunk.Anchor != "function:Alpha" || chunk.StartLine != 3 || chunk.EndLine != 5 {
		t.Fatalf("chunk = %+v, want the declaration extent", chunk)
	}
	if chunk.Content != "func Alpha() {\n\tBeta()\n}" {
		t.Fatalf("content = %q, want the declaration only", chunk.Content)
	}
}

func TestUnsupportedSourceUsesBoundedTextChunks(t *testing.T) {
	chunker := Chunker{
		Outliner: outlineStub{err: &symbols.Error{Code: symbols.CodeUnsupportedLanguage}},
		MaxBytes: 18, OverlapLines: 1,
	}
	content := []byte("first line\nsecond line\nthird line\n")
	chunks, err := chunker.Chunks(context.Background(), "README.md", content, "digest")
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want bounded chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.Precision != "text" || chunk.Language != "text" {
			t.Fatalf("chunk = %+v, want explicit text precision", chunk)
		}
		if len(chunk.Content) > 18 {
			t.Fatalf("chunk is %d bytes, want at most 18", len(chunk.Content))
		}
	}
}

func TestStableIdentityAndContentDigestHaveSeparateJobs(t *testing.T) {
	first := makeChunk("a.go", "go", "syntax", "function:A", "func A()", 0, 1, 1, []byte("func A() {}"))
	second := makeChunk("a.go", "go", "syntax", "function:A", "func A()", 0, 2, 2, []byte("func A() { work() }"))
	if first.StableID != second.StableID {
		t.Fatal("moving or editing a declaration changed its stable identity")
	}
	if first.ContentDigest == second.ContentDigest {
		t.Fatal("editing a declaration did not change its content digest")
	}
}

// Methods with the same name on different receivers are ordinary Go. Their
// concise search anchors may match, but their persistent identities must not:
// semantic_chunks.stable_id is globally unique and publication is atomic.
func TestSameNamedMethodsOnDifferentReceiversHaveDistinctStableIDs(t *testing.T) {
	first := "func (s Snapshot) Mark() {}\n"
	content := []byte(first + "func (j *Journal) Mark() {}\n")
	chunker := Chunker{Outliner: outlineStub{outline: symbols.Outline{
		Language: symbols.LanguageGo,
		Symbols: []symbols.Symbol{
			{Name: "Mark", Kind: symbols.KindMethod, Signature: "func (s Snapshot) Mark()", StartLine: 1, EndLine: 1, StartByte: 0, EndByte: len(first)},
			{Name: "Mark", Kind: symbols.KindMethod, Signature: "func (j *Journal) Mark()", StartLine: 2, EndLine: 2, StartByte: len(first), EndByte: len(content)},
		},
	}}, MaxBytes: len(first)}

	chunks, err := chunker.Chunks(context.Background(), "journal.go", content, "digest")
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want two methods", len(chunks))
	}
	if chunks[0].Anchor != chunks[1].Anchor || chunks[0].StableID == chunks[1].StableID {
		t.Fatalf("chunks = %+v, want one visible anchor and distinct persistent identities", chunks)
	}
}

func TestEveryChunkFitsTheCompleteEmbeddingInputBudget(t *testing.T) {
	path := strings.Repeat("deep/", 40) + "token_dense.py"
	content := []byte(strings.Repeat("!", 10_000))
	chunker := Chunker{Outliner: outlineStub{outline: symbols.Outline{
		Language: symbols.LanguagePython,
		Symbols: []symbols.Symbol{{
			Name: "Dense", Kind: symbols.KindFunction, Signature: "def Dense()",
			StartLine: 1, EndLine: 1, StartByte: 0, EndByte: len(content),
		}},
	}}}

	chunks, err := chunker.Chunks(context.Background(), path, content, "digest")
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want the oversized declaration split", len(chunks))
	}
	for _, chunk := range chunks {
		input := embeddingInput(EmbeddingDocument, chunk.EmbeddingText)
		if len(input) > maxEmbeddingInputBytes {
			t.Fatalf("embedding input is %d bytes, want at most %d", len(input), maxEmbeddingInputBytes)
		}
	}
}

func TestChunkingRejectsMetadataThatCannotFitTheEmbeddingInput(t *testing.T) {
	path := strings.Repeat("p", maxEmbeddingInputBytes)
	chunker := Chunker{Outliner: outlineStub{err: &symbols.Error{Code: symbols.CodeUnsupportedLanguage}}}
	if _, err := chunker.Chunks(context.Background(), path, []byte("content"), "digest"); err == nil {
		t.Fatal("oversized embedding metadata was accepted")
	}
}
