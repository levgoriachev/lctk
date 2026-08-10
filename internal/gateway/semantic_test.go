package gateway

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

// semanticFixture wires the extended surface, where the conceptual search lives
// until a retrieval-quality measurement earns it a default seat (ADR-0031).
func semanticFixture(t *testing.T) (*fixture, *fakeService, *mcp.ClientSession) {
	t.Helper()
	f := newConfiguredFixture(t, fixtureConfig{requireRunning: true, extendedTools: true},
		"alpha-aaaaaaaa", "beta-bbbbbbbb")
	service := newFakeService(t)
	service.semanticReady = true
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true}
	return f, service, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
}

func callSemanticSearch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	return callText(t, session, "code_search_semantic", arguments)
}

// TestSemanticSearchAnswersRankedRangesWithItsCaveat pins the text shape: ranged
// grep lines plus the one line that keeps a ranked lead from being read as
// proof.
func TestSemanticSearchAnswersRankedRangesWithItsCaveat(t *testing.T) {
	_, service, session := semanticFixture(t)

	text, result := callSemanticSearch(t, session, map[string]any{
		"query": "retry a failed request with backoff", "limit": 7,
		"project_id": "beta-bbbbbbbb", "repository_root": "C:\\other", "path": "../other",
	})
	if result.IsError {
		t.Fatalf("semantic search failed: %s", resultText(result))
	}
	want := "internal/retry.go:7-12: func Retry() { backoff() }\n" +
		"[ranked by meaning, not proof — read the ranges with read_span before relying on them]"
	if text != want {
		t.Errorf("answer = %q, want %q", text, want)
	}
	if len(service.semanticRequests) != 1 || service.semanticRequests[0]["query"] != "retry a failed request with backoff" {
		t.Fatalf("service requests = %+v", service.semanticRequests)
	}
	// The scope-like arguments are dropped, not forwarded: the backend has no
	// business seeing a project name it could act on.
	for _, forbidden := range []string{"project_id", "repository_root", "path"} {
		if _, crossed := service.semanticRequests[0][forbidden]; crossed {
			t.Fatalf("ignored scope field %q crossed the adapter boundary", forbidden)
		}
	}
	if service.semanticRequests[0]["limit"] != float64(7) {
		t.Fatalf("the limit was altered in transit: %+v", service.semanticRequests[0])
	}

	// The extended surface advertises the capability through project_info.
	capabilities := capabilitiesLine(t, callProjectInfo(t, session, nil))
	if !strings.Contains(capabilities, "code_search_semantic") {
		t.Fatalf("capabilities = %q, want code_search_semantic", capabilities)
	}
}

func TestSemanticSearchWithNoMatchesPointsAtExactSearch(t *testing.T) {
	_, service, session := semanticFixture(t)
	service.semanticResponse = `{"matches":[],"total":0,"truncated":false,` +
		`"generation":4,"exact_generation":4,"freshness":"fresh","model":"nomic-test","dimensions":768}`

	text, result := callSemanticSearch(t, session, map[string]any{"query": "quantum sorting"})
	if result.IsError {
		t.Fatalf("semantic search failed: %s", resultText(result))
	}
	if text != "no conceptual matches — try exact_search with a distinctive string" {
		t.Errorf("empty answer = %q", text)
	}
}

func TestSemanticFailureKeepsTypedRemediation(t *testing.T) {
	_, service, session := semanticFixture(t)
	service.semanticStatus = 503
	service.semanticResponse = `{"error":{"code":"EMBEDDING_UNAVAILABLE",` +
		`"message":"The local embedding service could not be reached.","retryable":true}}`

	_, result := callSemanticSearch(t, session, map[string]any{"query": "retry"})
	message := resultText(result)
	if !result.IsError || !strings.Contains(message, codeintel.CodeEmbeddingUnavailable) ||
		!strings.Contains(message, "lctk doctor") {
		t.Fatalf("typed error = %q", message)
	}
}
