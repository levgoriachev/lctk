package sweexplore

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Metrics mirrors the pinned official SWE-Explore metric names and formulas.
// Publication still uses the upstream evaluator; this scorer gives fast local
// parity evidence before paid mass runs.
type Metrics struct {
	Precision            float64 `json:"precision"`
	Recall               float64 `json:"recall"`
	F1Score              float64 `json:"f1_score"`
	HitFileRate          float64 `json:"hit_file_rate"`
	NoiseFileRate        float64 `json:"noise_file_rate"`
	HitRegionRate        float64 `json:"hit_region_rate"`
	NoiseRegionRate      float64 `json:"noise_region_rate"`
	WeightedCoreCoverage float64 `json:"weighted_core_coverage"`
	ContextEfficiency    float64 `json:"context_efficiency"`
	OptionalCoverage     float64 `json:"optional_coverage"`
	NDCGAt100            float64 `json:"ndcg_at_100"`
	NDCGAt300            float64 `json:"ndcg_at_300"`
	NDCGAt500            float64 `json:"ndcg_at_500"`
	RecallAt100          float64 `json:"recall_at_100"`
	RecallAt300          float64 `json:"recall_at_300"`
	RecallAt500          float64 `json:"recall_at_500"`
	FirstUsefulHit       float64 `json:"first_useful_hit"`
}

type lineKey struct {
	path string
	line int
}

type gainRegion struct {
	gain  float64
	lines int
}

// Score computes every official metric for one ordered prediction list.
func Score(root string, predictions []Region, truth GroundTruth) (Metrics, error) {
	counts, err := lineCounts(root, predictions, truth)
	if err != nil {
		return Metrics{}, err
	}
	predLines := regionsToLines(predictions, counts)
	coreLines := regionsToLines(truth.ReadCoreRegions, counts)
	optionalRegions := flattenRegions(truth.ReadOptionalRegionsMap)
	optionalLines := regionsToLines(optionalRegions, counts)
	precision := ratio(intersectionSize(predLines, coreLines), len(predLines))
	recall := ratio(intersectionSize(predLines, coreLines), len(coreLines))
	f1 := 0.0
	if precision+recall != 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	visited := stringSetFromRegions(predictions)
	coreFiles := stringSet(truth.ReadCoreFiles)
	optionalFiles := stringSet(flattenStrings(truth.ReadOptionalFilesMap))
	noiseFiles := 0
	for path := range visited {
		if _, core := coreFiles[path]; core {
			continue
		}
		if _, optional := optionalFiles[path]; !optional {
			noiseFiles++
		}
	}
	hitRegions := 0
	for _, core := range truth.ReadCoreRegions {
		if overlapsAny(core, predictions, counts) {
			hitRegions++
		}
	}
	noiseRegions := 0
	for _, prediction := range predictions {
		if !overlapsAny(prediction, truth.ReadCoreRegions, counts) && !overlapsAny(prediction, optionalRegions, counts) {
			noiseRegions++
		}
	}
	useful := union(coreLines, optionalLines)
	metrics := Metrics{
		Precision: precision, Recall: recall, F1Score: f1,
		HitFileRate:          ratio(intersectionStringSize(visited, coreFiles), len(coreFiles)),
		NoiseFileRate:        ratio(noiseFiles, len(visited)),
		HitRegionRate:        ratio(hitRegions, len(truth.ReadCoreRegions)),
		NoiseRegionRate:      ratio(noiseRegions, len(predictions)),
		WeightedCoreCoverage: weightedCoreCoverage(predLines, truth, counts),
		ContextEfficiency:    ratio(intersectionSize(predLines, useful), len(predLines)),
		OptionalCoverage:     ratio(intersectionSize(predLines, optionalLines), len(optionalLines)),
		NDCGAt100:            ndcgAt(predictions, truth, counts, 100),
		NDCGAt300:            ndcgAt(predictions, truth, counts, 300),
		NDCGAt500:            ndcgAt(predictions, truth, counts, 500),
		RecallAt100:          recallAt(predictions, coreLines, counts, 100),
		RecallAt300:          recallAt(predictions, coreLines, counts, 300),
		RecallAt500:          recallAt(predictions, coreLines, counts, 500),
		FirstUsefulHit:       firstUsefulHit(predictions, coreLines, counts),
	}
	return metrics, nil
}

func lineCounts(root string, predictions []Region, truth GroundTruth) (map[string]int, error) {
	paths := stringSetFromRegions(predictions)
	for _, region := range truth.ReadCoreRegions {
		paths[region.Path] = struct{}{}
	}
	for _, region := range flattenRegions(truth.ReadOptionalRegionsMap) {
		paths[region.Path] = struct{}{}
	}
	counts := make(map[string]int, len(paths))
	for path := range paths {
		count, err := countLines(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("count benchmark file %q: %w", path, err)
		}
		counts[path] = count
	}
	return counts, nil
}

func resolve(region Region, counts map[string]int) (int, int, bool) {
	lineCount := counts[region.Path]
	needsCount := region.End == -1 || region.Start < 0
	if needsCount && lineCount < 1 {
		return 0, 0, false
	}
	if needsCount {
		end := region.End
		if end == -1 {
			end = lineCount
		}
		start := region.Start
		if start < 0 {
			start = lineCount + start + 1
		}
		start = max(1, min(start, lineCount))
		end = max(1, min(end, lineCount))
		return start, end, true
	}
	if region.End < 1 {
		return 0, 0, false
	}
	return max(1, region.Start), region.End, true
}

func regionsToLines(regions []Region, counts map[string]int) map[lineKey]struct{} {
	lines := map[lineKey]struct{}{}
	for _, region := range regions {
		start, end, ok := resolve(region, counts)
		if !ok {
			continue
		}
		for line := start; line <= end; line++ {
			lines[lineKey{path: region.Path, line: line}] = struct{}{}
		}
	}
	return lines
}

func overlapsAny(region Region, candidates []Region, counts map[string]int) bool {
	start, end, ok := resolve(region, counts)
	if !ok {
		return false
	}
	for _, candidate := range candidates {
		if region.Path != candidate.Path {
			continue
		}
		otherStart, otherEnd, otherOK := resolve(candidate, counts)
		if otherOK && start <= otherEnd && otherStart <= end {
			return true
		}
	}
	return false
}

func weightedCoreCoverage(predictions map[lineKey]struct{}, truth GroundTruth, counts map[string]int) float64 {
	mainFiles := stringSet(truth.MainFiles)
	weighted, total := 0.0, 0.0
	for _, region := range truth.ReadCoreRegions {
		lines := regionsToLines([]Region{region}, counts)
		if len(lines) == 0 {
			continue
		}
		weight := 2.0
		if _, main := mainFiles[region.Path]; main {
			weight = 3
		}
		weighted += weight * float64(intersectionSize(predictions, lines)) / float64(len(lines))
		total += weight
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

func ndcgAt(predictions []Region, truth GroundTruth, counts map[string]int, budget int) float64 {
	if len(predictions) == 0 || len(truth.ReadCoreRegions) == 0 {
		return 0
	}
	mainFiles := stringSet(truth.MainFiles)
	coreLines := regionsToLines(truth.ReadCoreRegions, counts)
	items := make([]gainRegion, 0, len(predictions))
	for _, prediction := range predictions {
		start, end, ok := resolve(prediction, counts)
		if !ok {
			items = append(items, gainRegion{})
			continue
		}
		item := gainRegion{lines: end - start + 1}
		for line := start; line <= end; line++ {
			if _, core := coreLines[lineKey{path: prediction.Path, line: line}]; core {
				item.gain++
				if _, main := mainFiles[prediction.Path]; main {
					item.gain += 0.5
				}
			}
		}
		items = append(items, item)
	}
	dcg := discountedGain(items, budget)
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].gain/float64(max(items[left].lines, 1)) > items[right].gain/float64(max(items[right].lines, 1))
	})
	ideal := discountedGain(items, budget)
	if ideal == 0 {
		return 0
	}
	return min(dcg/ideal, 1.0)
}

func discountedGain(items []gainRegion, budget int) float64 {
	total, cumulative := 0.0, 0
	for index, item := range items {
		cumulative += item.lines
		if cumulative > budget && index > 0 {
			break
		}
		total += item.gain / math.Log2(float64(index+2))
	}
	return total
}

func recallAt(predictions []Region, core map[lineKey]struct{}, counts map[string]int, budget int) float64 {
	if len(core) == 0 {
		return 0
	}
	covered := map[lineKey]struct{}{}
	seen := 0
	for _, prediction := range predictions {
		start, end, ok := resolve(prediction, counts)
		if !ok {
			continue
		}
		for line := start; line <= end; line++ {
			seen++
			key := lineKey{path: prediction.Path, line: line}
			if _, useful := core[key]; useful {
				covered[key] = struct{}{}
			}
			if seen >= budget {
				break
			}
		}
		if seen >= budget {
			break
		}
	}
	return float64(len(covered)) / float64(len(core))
}

func firstUsefulHit(predictions []Region, core map[lineKey]struct{}, counts map[string]int) float64 {
	if len(predictions) == 0 || len(core) == 0 {
		return 0
	}
	for index, prediction := range predictions {
		start, end, ok := resolve(prediction, counts)
		if !ok {
			continue
		}
		for line := start; line <= end; line++ {
			if _, useful := core[lineKey{path: prediction.Path, line: line}]; useful {
				return 1 - float64(index)/float64(len(predictions))
			}
		}
	}
	return 0
}

func flattenRegions(values map[string][]Region) []Region {
	var flattened []Region
	for _, regions := range values {
		flattened = append(flattened, regions...)
	}
	return flattened
}

func flattenStrings(values map[string][]string) []string {
	var flattened []string
	for _, strings := range values {
		flattened = append(flattened, strings...)
	}
	return flattened
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func stringSetFromRegions(regions []Region) map[string]struct{} {
	set := make(map[string]struct{}, len(regions))
	for _, region := range regions {
		set[region.Path] = struct{}{}
	}
	return set
}

func intersectionSize[T comparable](left, right map[T]struct{}) int {
	count := 0
	for value := range left {
		if _, ok := right[value]; ok {
			count++
		}
	}
	return count
}

func intersectionStringSize(left, right map[string]struct{}) int {
	return intersectionSize(left, right)
}

func union(left, right map[lineKey]struct{}) map[lineKey]struct{} {
	result := make(map[lineKey]struct{}, len(left)+len(right))
	for value := range left {
		result[value] = struct{}{}
	}
	for value := range right {
		result[value] = struct{}{}
	}
	return result
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
