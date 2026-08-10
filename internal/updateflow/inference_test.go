package updateflow

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type inferenceTransactionStub struct {
	distribution   inference.Distribution
	imageAvailable bool
	modelAvailable bool
	pulled         bool
	modelInstalled bool
	ensured        int
	selfTested     int
	ensureErr      error
	selfTestErr    error
}

func (s *inferenceTransactionStub) ImageAvailable(context.Context) bool { return s.imageAvailable }
func (s *inferenceTransactionStub) ModelAvailable() bool                { return s.modelAvailable }
func (s *inferenceTransactionStub) PullImage(context.Context) error {
	s.pulled = true
	s.imageAvailable = true
	return nil
}
func (s *inferenceTransactionStub) InstallModel(context.Context, *http.Client) error {
	s.modelInstalled = true
	s.modelAvailable = true
	return nil
}
func (s *inferenceTransactionStub) Ensure(context.Context, time.Duration) (inference.Status, error) {
	s.ensured++
	return inference.Status{Ready: s.ensureErr == nil, Distribution: s.distribution}, s.ensureErr
}
func (s *inferenceTransactionStub) SelfTest(context.Context) error {
	s.selfTested++
	return s.selfTestErr
}

type nvidiaTransactionStub struct {
	ensured int
	err     error
}

func (s *nvidiaTransactionStub) Inspect(context.Context, releasebundle.Manifest) (nvidiainstall.Plan, error) {
	return nvidiainstall.Plan{}, nil
}
func (s *nvidiaTransactionStub) Ensure(context.Context, releasebundle.Manifest) (nvidiainstall.Status, error) {
	s.ensured++
	return nvidiainstall.Status{Ready: s.err == nil}, s.err
}

func TestActivateInferenceCommitsGPUAndRestoresCPU(t *testing.T) {
	cpu := &inferenceTransactionStub{distribution: inference.DistributionCPU, imageAvailable: true, modelAvailable: true}
	gpu := &inferenceTransactionStub{distribution: inference.DistributionNVIDIAGPU}
	nvidia := &nvidiaTransactionStub{}
	stored := inference.DefaultSelection
	manager := Manager{
		NVIDIA: nvidia,
		NewInference: func(distribution inference.Distribution) (Inference, error) {
			if distribution == inference.DistributionCPU {
				return cpu, nil
			}
			return gpu, nil
		},
		LoadSelection: func() (inference.Selection, error) { return stored, nil },
		SaveSelection: func(selection inference.Selection) error {
			stored = selection
			return nil
		},
	}
	restore, err := manager.activateInference(t.Context(), releasebundle.Manifest{}, inference.DistributionNVIDIAGPU)
	if err != nil {
		t.Fatal(err)
	}
	if nvidia.ensured != 1 || !gpu.pulled || !gpu.modelInstalled || gpu.ensured != 1 || gpu.selfTested != 1 {
		t.Fatalf("GPU activation was incomplete: nvidia=%d gpu=%+v", nvidia.ensured, gpu)
	}
	if stored.Distribution != inference.DistributionNVIDIAGPU {
		t.Fatalf("stored distribution = %q, want NVIDIA GPU", stored.Distribution)
	}
	if err := restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cpu.ensured != 1 || cpu.selfTested != 1 || stored.Distribution != inference.DistributionCPU {
		t.Fatalf("CPU rollback was incomplete: cpu=%+v stored=%q", cpu, stored.Distribution)
	}
}

func TestActivateInferenceRestoresRuntimeWhenSelectionCommitFails(t *testing.T) {
	cpu := &inferenceTransactionStub{distribution: inference.DistributionCPU, imageAvailable: true, modelAvailable: true}
	gpu := &inferenceTransactionStub{distribution: inference.DistributionNVIDIAGPU, imageAvailable: true, modelAvailable: true}
	manager := Manager{
		NVIDIA: &nvidiaTransactionStub{},
		NewInference: func(distribution inference.Distribution) (Inference, error) {
			if distribution == inference.DistributionCPU {
				return cpu, nil
			}
			return gpu, nil
		},
		LoadSelection: func() (inference.Selection, error) { return inference.DefaultSelection, nil },
		SaveSelection: func(selection inference.Selection) error {
			if selection.Distribution == inference.DistributionNVIDIAGPU {
				return errors.New("selection write failed")
			}
			return nil
		},
	}
	_, err := manager.activateInference(t.Context(), releasebundle.Manifest{}, inference.DistributionNVIDIAGPU)
	if err == nil || cpu.ensured != 1 || cpu.selfTested != 1 {
		t.Fatalf("selection failure did not restore CPU: err=%v cpu=%+v", err, cpu)
	}
}

func TestPre012RollbackUsesCPUAndCanRestoreSelectedGPU(t *testing.T) {
	cpu := &inferenceTransactionStub{distribution: inference.DistributionCPU, imageAvailable: true, modelAvailable: true}
	gpu := &inferenceTransactionStub{distribution: inference.DistributionNVIDIAGPU, imageAvailable: true, modelAvailable: true}
	manager := Manager{
		NewInference: func(distribution inference.Distribution) (Inference, error) {
			if distribution == inference.DistributionCPU {
				return cpu, nil
			}
			return gpu, nil
		},
		LoadSelection: func() (inference.Selection, error) {
			return inference.Selection{SchemaVersion: inference.SelectionSchemaVersion, Distribution: inference.DistributionNVIDIAGPU}, nil
		},
	}
	distribution, restore, err := manager.activateRollbackInference(t.Context(), "0.1.11")
	if err != nil {
		t.Fatal(err)
	}
	if distribution != inference.DistributionCPU {
		t.Fatalf("rollback distribution = %q, want CPU", distribution)
	}
	if cpu.ensured != 1 || cpu.selfTested != 1 {
		t.Fatalf("pre-0.1.12 rollback did not activate CPU: %+v", cpu)
	}
	if err := restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gpu.ensured != 1 || gpu.selfTested != 1 {
		t.Fatalf("failed rollback recovery did not reactivate selected GPU: %+v", gpu)
	}
}
