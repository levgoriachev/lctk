package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lev-goryachev/lctk/images/code-intel/internal/symbols"
)

const (
	// The source-content target preserves useful structural regions, while the
	// complete input cap is the fail-closed contract with the pinned llama.cpp
	// runtime. A tokenizer cannot emit more non-special tokens than the UTF-8
	// bytes it consumes, and the 64-byte margin covers its special tokens.
	defaultChunkBytes      = 3 << 10
	maxEmbeddingInputBytes = 1984
	defaultOverlapLines    = 8
)

// Outliner is the structural capability the chunker needs. Keeping the narrow
// interface makes chunking independently testable while reusing Stage 4 byte
// extents in production.
type Outliner interface {
	Outline(ctx context.Context, name string, content []byte, digest string) (symbols.Outline, error)
}

// Chunk is one durable retrieval unit. StableID describes its structural place;
// ContentDigest decides whether its embedding may be reused.
type Chunk struct {
	StableID      string
	Path          string
	Language      string
	Precision     string
	Anchor        string
	Ordinal       int
	StartLine     int
	EndLine       int
	Content       string
	EmbeddingText string
	ContentDigest string
}

// Chunker turns one already-authorized file into bounded retrieval units.
type Chunker struct {
	Outliner     Outliner
	MaxBytes     int
	OverlapLines int
}

// Chunks prefers top-level syntax declarations and uses an explicit text
// fallback for unsupported files. Parse failures are not hidden as text chunks:
// a supported file that cannot be parsed must remain diagnosable.
func (c Chunker) Chunks(ctx context.Context, path string, content []byte, digest string) ([]Chunk, error) {
	maximum := c.MaxBytes
	if maximum <= 0 {
		maximum = defaultChunkBytes
	}
	overlap := c.OverlapLines
	if overlap < 0 {
		overlap = 0
	}
	if overlap == 0 {
		overlap = defaultOverlapLines
	}
	if c.Outliner != nil {
		outline, err := c.Outliner.Outline(ctx, path, content, digest)
		if err == nil {
			chunks, err := structuralChunks(path, content, outline, maximum)
			if err != nil {
				return nil, err
			}
			if len(chunks) > 0 {
				return chunks, nil
			}
			return textChunks(path, content, outline.Language, maximum, overlap)
		}
		var typed *symbols.Error
		if !errors.As(err, &typed) || typed.Code != symbols.CodeUnsupportedLanguage {
			return nil, err
		}
	}
	return textChunks(path, content, "text", maximum, overlap)
}

func structuralChunks(path string, content []byte, outline symbols.Outline, maximum int) ([]Chunk, error) {
	var chunks []Chunk
	type group struct {
		startByte    int
		endByte      int
		startLine    int
		endLine      int
		anchor       string
		stableAnchor string
		maximum      int
	}
	var current group
	flush := func() {
		if current.endByte <= current.startByte {
			return
		}
		chunks = append(chunks, makeChunk(path, outline.Language, "syntax", current.anchor, current.stableAnchor,
			0, current.startLine, current.endLine, content[current.startByte:current.endByte]))
		current = group{}
	}
	// A language may legally declare the same method name on different receivers
	// or overload the same function name. The visible anchor remains concise, but
	// the persistent identity includes the complete syntax declaration plus its
	// deterministic occurrence. This is the structural identity contract: two
	// distinct declarations in one file must never reach the UNIQUE database key
	// with the same value.
	occurrences := map[string]int{}
	for _, symbol := range outline.Symbols {
		if symbol.Depth != 0 || symbol.StartByte < 0 || symbol.EndByte > len(content) || symbol.EndByte <= symbol.StartByte {
			continue
		}
		anchor := string(symbol.Kind) + ":" + symbol.Name
		structural := anchor + "\x00" + symbol.Signature
		occurrence := occurrences[structural]
		occurrences[structural] = occurrence + 1
		stableAnchor := fmt.Sprintf("%s\x00%d", structural, occurrence)
		contentLimit, err := embeddingContentLimit(path, anchor, maximum)
		if err != nil {
			return nil, err
		}
		if symbol.EndByte-symbol.StartByte <= contentLimit {
			if current.endByte == 0 {
				current = group{startByte: symbol.StartByte, endByte: symbol.EndByte,
					startLine: symbol.StartLine, endLine: symbol.EndLine, anchor: anchor, stableAnchor: stableAnchor, maximum: contentLimit}
				continue
			}
			if symbol.EndByte-current.startByte <= current.maximum {
				current.endByte = symbol.EndByte
				current.endLine = symbol.EndLine
				continue
			}
			flush()
			current = group{startByte: symbol.StartByte, endByte: symbol.EndByte,
				startLine: symbol.StartLine, endLine: symbol.EndLine, anchor: anchor, stableAnchor: stableAnchor, maximum: contentLimit}
			continue
		}
		flush()
		pieces := splitLines(content[symbol.StartByte:symbol.EndByte], contentLimit, 0)
		line := symbol.StartLine
		for ordinal, piece := range pieces {
			end := line + lineCount(piece) - 1
			chunks = append(chunks, makeChunk(path, outline.Language, "syntax", anchor, stableAnchor,
				ordinal, line, end, piece))
			line = end + 1
		}
	}
	flush()
	return chunks, nil
}

func textChunks(path string, content []byte, language string, maximum, overlap int) ([]Chunk, error) {
	contentLimit, err := embeddingContentLimit(path, "file", maximum)
	if err != nil {
		return nil, err
	}
	pieces := splitLines(content, contentLimit, overlap)
	chunks := make([]Chunk, 0, len(pieces))
	line := 1
	for ordinal, piece := range pieces {
		end := line + lineCount(piece) - 1
		chunks = append(chunks, makeChunk(path, language, "text", "file", "file", ordinal, line, end, piece))
		advance := lineCount(piece) - overlap
		if advance < 1 {
			advance = 1
		}
		line += advance
	}
	return chunks, nil
}

// embeddingContentLimit converts a requested source-content size into the
// remaining complete document-input budget after the task prefix, path, anchor,
// and separators are counted. Metadata is never silently truncated because it
// is part of retrieval meaning and provenance.
func embeddingContentLimit(path, anchor string, requested int) (int, error) {
	overhead := len(embeddingInput(EmbeddingDocument, path+"\n"+anchor+"\n"))
	available := maxEmbeddingInputBytes - overhead
	if available <= 0 {
		return 0, fail(CodeInternalError,
			"Semantic chunk metadata exceeds the embedding input budget.", false, nil)
	}
	if requested < available {
		return requested, nil
	}
	return available, nil
}

func makeChunk(path, language, precision, anchor, stableAnchor string, ordinal, startLine, endLine int, content []byte) Chunk {
	stable := digestString(fmt.Sprintf("%s\x00%s\x00%d", path, stableAnchor, ordinal))
	text := strings.TrimSpace(string(content))
	return Chunk{
		StableID: stable, Path: path, Language: language, Precision: precision,
		Anchor: anchor, Ordinal: ordinal, StartLine: startLine, EndLine: endLine,
		Content: text, EmbeddingText: path + "\n" + anchor + "\n" + text,
		ContentDigest: digestString(text),
	}
}

// splitLines keeps UTF-8 source on line boundaries. A single overlong line is
// split on rune boundaries so every chunk remains bounded and valid UTF-8.
func splitLines(content []byte, maximum, overlap int) [][]byte {
	if len(content) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(content), "\n")
	var pieces [][]byte
	for start := 0; start < len(lines); {
		end := start
		size := 0
		for end < len(lines) && (size+len(lines[end]) <= maximum || end == start) {
			if len(lines[end]) > maximum && end == start {
				for _, part := range splitUTF8([]byte(lines[end]), maximum) {
					pieces = append(pieces, part)
				}
				end++
				size = 0
				break
			}
			size += len(lines[end])
			end++
		}
		if size > 0 {
			pieces = append(pieces, []byte(strings.Join(lines[start:end], "")))
		}
		if end >= len(lines) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return pieces
}

func splitUTF8(content []byte, maximum int) [][]byte {
	var parts [][]byte
	for len(content) > maximum {
		cut := maximum
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maximum
		}
		parts = append(parts, append([]byte(nil), content[:cut]...))
		content = content[cut:]
	}
	if len(content) > 0 {
		parts = append(parts, append([]byte(nil), content...))
	}
	return parts
}

func lineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := strings.Count(string(content), "\n")
	if content[len(content)-1] != '\n' {
		count++
	}
	return count
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
