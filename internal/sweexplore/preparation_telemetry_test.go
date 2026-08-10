package sweexplore

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/inference"
)

func TestPreparationTelemetrySeparatesProgressFromGPULiveness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	writer := newPreparationTelemetryWriter(path)
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	samples := []PreparationSample{
		{ObservedAt: started.Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{ChunksTotal: 100, ChunksEmbedded: 20}, Inference: inference.Status{Backend: "cuda", GPUTelemetry: &inference.GPUTelemetry{UtilizationPercent: 95}}},
		{ObservedAt: started.Add(2 * time.Second).Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{ChunksTotal: 100, ChunksEmbedded: 20}, Inference: inference.Status{Backend: "cuda", GPUTelemetry: &inference.GPUTelemetry{UtilizationPercent: 96}}},
		{ObservedAt: started.Add(4 * time.Second).Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{ChunksTotal: 100, ChunksEmbedded: 40}, Inference: inference.Status{Backend: "cuda", GPUTelemetry: &inference.GPUTelemetry{UtilizationPercent: 91}}},
	}
	for _, sample := range samples {
		if err := writer.Observe(sample); err != nil {
			t.Fatal(err)
		}
	}
	summary := writer.Summary()
	if summary.Samples != 3 || summary.ProgressSamples != 1 || summary.FirstChunksCompleted != 20 || summary.LastChunksCompleted != 40 || summary.PeakGPUUtilization != 96 || summary.GPUActiveSamples != 3 {
		t.Fatalf("unexpected telemetry summary: %+v", summary)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if err := scanner.Err(); err != nil || lines != 3 {
		t.Fatalf("durable rows=%d err=%v", lines, err)
	}
}

func TestPreparationTelemetryRetainsCommittedProgressAfterTransientCountersReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	writer := newPreparationTelemetryWriter(path)
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	samples := []PreparationSample{
		{ObservedAt: started.Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{Indexing: true, ChunksTotal: 20622, ChunksEmbedded: 20608}},
		{ObservedAt: started.Add(2 * time.Second).Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{Ready: true, ChunkCount: 20622}},
	}
	for _, sample := range samples {
		if err := writer.Observe(sample); err != nil {
			t.Fatal(err)
		}
	}
	summary := writer.Summary()
	if summary.LastChunksCompleted != 20622 || summary.ProgressSamples != 1 {
		t.Fatalf("committed progress was lost after counter reset: %+v", summary)
	}
}
