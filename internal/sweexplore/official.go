package sweexplore

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const officialScoreProgram = `
import importlib.util
import json
import pathlib
import sys

official_root = pathlib.Path(sys.argv[1])
bench_path = pathlib.Path(sys.argv[2])
result_path = pathlib.Path(sys.argv[3])
repo_root = pathlib.Path(sys.argv[4])

spec = importlib.util.spec_from_file_location("pinned_swe_explore_eval", official_root / "eval.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

result = json.loads(result_path.read_text(encoding="utf-8"))
instance_id = result["instance_id"]
evaluator = module.ExploreEvaluator(bench_path)
ground_truth = evaluator.bench_data_dict[instance_id]["ground_truth"]

paths = set()
for region in ground_truth.get("read_core_regions") or []:
    paths.add(region["path"])
for regions in (ground_truth.get("read_optional_regions_map") or {}).values():
    for region in regions:
        paths.add(region["path"])
for region in result["regions"]:
    paths.add(region["path"])

line_counts = {}
for relative in paths:
    path = repo_root / relative
    if path.is_file():
        line_counts[relative] = len(path.read_text(errors="ignore").splitlines())

evaluator._current_instance_id = instance_id
evaluator._current_file_line_counts = line_counts
predictions = [(region["path"], region["start"], region["end"]) for region in result["regions"]]
metric_names = [
    "precision", "recall", "f1_score", "hit_file_rate", "noise_file_rate",
    "hit_region_rate", "noise_region_rate", "weighted_core_coverage",
    "context_efficiency", "optional_coverage", "ndcg_at_100", "ndcg_at_300",
    "ndcg_at_500", "recall_at_100", "recall_at_300", "recall_at_500",
    "first_useful_hit",
]
metrics = {name: getattr(evaluator, "evaluate_" + name)(predictions, ground_truth) for name in metric_names}
print(json.dumps({
    "instance_id": instance_id,
    "explorer": result["arm_id"],
    "regions": result["regions"],
    "metrics": metrics,
    "num_regions": len(result["regions"]),
}, ensure_ascii=False))
`

// OfficialScore invokes the commit-pinned upstream evaluator on an already
// saved result. The agent is never rerun, so scoring can be audited or repeated
// without additional model cost.
func OfficialScore(ctx context.Context, python string, config Config, resultPath string) (string, error) {
	absoluteResult, err := filepath.Abs(resultPath)
	if err != nil {
		return "", fmt.Errorf("resolve result path: %w", err)
	}
	commit, err := commandOutput(ctx, config.Benchmark.OfficialRoot, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect official evaluator commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(commit), config.Benchmark.OfficialCommit) {
		return "", fmt.Errorf("official evaluator is at %s, want %s", strings.TrimSpace(commit), config.Benchmark.OfficialCommit)
	}
	command := exec.CommandContext(ctx, python, "-c", officialScoreProgram,
		config.Benchmark.OfficialRoot, config.Benchmark.ExploreDatasetPath, absoluteResult, config.Workspace.Root)
	command.Dir = config.Workspace.Root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run official SWE-Explore evaluator: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return string(output), nil
}
