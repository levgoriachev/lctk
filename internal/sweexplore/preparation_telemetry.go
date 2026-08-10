package sweexplore

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/inference"
)

// PreparationSample is one durable observation of actual index advancement and
// the CUDA device doing the embedding work. It deliberately keeps liveness,
// progress, and compute utilization as separate facts.
type PreparationSample struct {
	ObservedAt        string                    `json:"observed_at"`
	Phase             string                    `json:"phase"`
	NotReadyReason    string                    `json:"not_ready_reason,omitempty"`
	Exact             PreparationExactStatus    `json:"exact"`
	Semantic          PreparationSemanticStatus `json:"semantic"`
	Graph             PreparationGraphStatus    `json:"graph"`
	Inference         inference.Status          `json:"inference"`
	CompletionPercent float64                   `json:"completion_percent"`
	ChunksPerSecond   float64                   `json:"chunks_per_second"`
	ETASeconds        int64                     `json:"eta_seconds,omitempty"`
}

// PreparationExactStatus is the immutable-code index portion needed to prove
// that the selected checkout was actually observed.
type PreparationExactStatus struct {
	Ready      bool   `json:"ready"`
	Indexing   bool   `json:"indexing"`
	Generation uint64 `json:"generation"`
	FileCount  int    `json:"file_count"`
}

// PreparationSemanticStatus carries the counters whose increase proves real
// embedding progress rather than a merely healthy service process.
type PreparationSemanticStatus struct {
	Ready          bool   `json:"ready"`
	Indexing       bool   `json:"indexing"`
	Generation     uint64 `json:"generation"`
	FileCount      int    `json:"file_count"`
	ChunkCount     int    `json:"chunk_count"`
	ChunksTotal    int    `json:"chunks_total"`
	ChunksEmbedded int    `json:"chunks_embedded"`
	ChunksReused   int    `json:"chunks_reused"`
	StartedAt      string `json:"started_at,omitempty"`
	ProgressAt     string `json:"progress_at,omitempty"`
	Stalled        bool   `json:"stalled"`
	StallSeconds   int64  `json:"stall_seconds"`
	LastError      string `json:"last_error,omitempty"`
}

// PreparationGraphStatus proves that graph freshness converges to the same
// checkout generation before any measured agent arm starts.
type PreparationGraphStatus struct {
	Ready      bool   `json:"ready"`
	Generation uint64 `json:"generation"`
	FileCount  int    `json:"file_count"`
	Freshness  string `json:"freshness"`
}

// PreparationTelemetrySummary is stored in the immutable prepare receipt so a
// reviewer can evaluate the GPU gate without trusting a transient console.
type PreparationTelemetrySummary struct {
	Samples              int `json:"samples"`
	ProgressSamples      int `json:"progress_samples"`
	FirstChunksCompleted int `json:"first_chunks_completed"`
	LastChunksCompleted  int `json:"last_chunks_completed"`
	PeakGPUUtilization   int `json:"peak_gpu_utilization_percent"`
	GPUActiveSamples     int `json:"gpu_active_samples"`
}

// preparationTelemetryWriter appends and fsyncs each sample immediately. A
// crash therefore preserves the last confirmed counter and GPU measurement.
type preparationTelemetryWriter struct {
	path       string
	previous   map[string]PreparationSample
	summary    PreparationTelemetrySummary
	hasSummary bool
}

func newPreparationTelemetryWriter(path string) *preparationTelemetryWriter {
	return &preparationTelemetryWriter{path: path, previous: map[string]PreparationSample{}}
}

// Observe derives rate and ETA from counter deltas, appends one JSONL record,
// and synchronizes it before the next potentially long indexing wait.
func (writer *preparationTelemetryWriter) Observe(sample PreparationSample) error {
	completed := completedSemanticChunks(sample.Semantic)
	if sample.Semantic.ChunksTotal > 0 {
		sample.CompletionPercent = math.Min(100, 100*float64(completed)/float64(sample.Semantic.ChunksTotal))
	}
	if previous, ok := writer.previous[sample.Phase]; ok {
		previousAt, timeErr := time.Parse(time.RFC3339Nano, previous.ObservedAt)
		currentAt, currentErr := time.Parse(time.RFC3339Nano, sample.ObservedAt)
		delta := completed - previous.Semantic.ChunksEmbedded - previous.Semantic.ChunksReused
		seconds := currentAt.Sub(previousAt).Seconds()
		if timeErr == nil && currentErr == nil && delta > 0 && seconds > 0 {
			sample.ChunksPerSecond = float64(delta) / seconds
			remaining := sample.Semantic.ChunksTotal - completed
			if remaining > 0 {
				sample.ETASeconds = int64(math.Ceil(float64(remaining) / sample.ChunksPerSecond))
			}
			writer.summary.ProgressSamples++
		}
	}
	writer.previous[sample.Phase] = sample
	writer.summary.Samples++
	if !writer.hasSummary {
		writer.summary.FirstChunksCompleted = completed
		writer.hasSummary = true
	}
	writer.summary.LastChunksCompleted = completed
	if sample.Inference.GPUTelemetry != nil {
		utilization := sample.Inference.GPUTelemetry.UtilizationPercent
		writer.summary.PeakGPUUtilization = max(writer.summary.PeakGPUUtilization, utilization)
		if utilization >= 80 {
			writer.summary.GPUActiveSamples++
		}
	}
	body, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("encode preparation sample: %w", err)
	}
	file, err := os.OpenFile(writer.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open preparation telemetry: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("append preparation telemetry: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("synchronize preparation telemetry: %w", err)
	}
	return nil
}

// completedSemanticChunks preserves the live in-flight counters while an
// index is advancing, then uses the committed chunk count after publication.
// The status API intentionally clears transient embedded/reused counters when
// indexing closes, so treating that reset as zero would erase proven progress
// from the immutable preparation summary.
func completedSemanticChunks(status PreparationSemanticStatus) int {
	completed := status.ChunksEmbedded + status.ChunksReused
	if status.Ready && !status.Indexing && status.ChunksTotal == 0 && status.ChunkCount > completed {
		return status.ChunkCount
	}
	return completed
}

func (writer *preparationTelemetryWriter) Summary() PreparationTelemetrySummary {
	return writer.summary
}

func newPreparationSample(phase string, status codeintel.Status, inferenceStatus inference.Status, reason string, observedAt time.Time) PreparationSample {
	sample := PreparationSample{ObservedAt: observedAt.Format(time.RFC3339Nano), Phase: phase, NotReadyReason: reason,
		Exact:     PreparationExactStatus{Ready: status.Ready, Indexing: status.Indexing, Generation: status.Generation, FileCount: status.FileCount},
		Inference: inferenceStatus}
	if status.Semantic != nil {
		semantic := status.Semantic
		sample.Semantic = PreparationSemanticStatus{Ready: semantic.Ready, Indexing: semantic.Indexing, Generation: semantic.Generation,
			FileCount: semantic.FileCount, ChunkCount: semantic.ChunkCount, ChunksTotal: semantic.ChunksTotal,
			ChunksEmbedded: semantic.ChunksEmbedded, ChunksReused: semantic.ChunksReused, StartedAt: semantic.StartedAt,
			ProgressAt: semantic.ProgressAt, Stalled: semantic.Stalled, StallSeconds: semantic.StallSeconds, LastError: semantic.LastError}
	}
	if status.Graph != nil {
		graph := status.Graph
		sample.Graph = PreparationGraphStatus{Ready: graph.Ready, Generation: graph.Generation, FileCount: graph.FileCount, Freshness: graph.Freshness}
	}
	return sample
}
