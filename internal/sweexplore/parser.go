package sweexplore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var regionLine = regexp.MustCompile(`^[-*]\s+(.+):(\-?\d+)-(\-?\d+)\s*$`)

// ParsedTrace is the provider-neutral evidence extracted from a machine-readable
// client trace.
type ParsedTrace struct {
	FinalMessage  string
	LCTKToolCalls []string
	ActualModels  []string
	Usage         Usage
	ProviderStats ProviderMetrics
}

// ParseTrace extracts the final message, usage, and LCTK tool evidence without
// accepting a provider's prose summary as proof of a tool call.
func ParseTrace(provider Provider, raw []byte) (ParsedTrace, error) {
	switch provider {
	case ProviderCodex:
		return parseCodexTrace(raw)
	case ProviderClaude:
		return parseClaudeTrace(raw)
	case ProviderFixture:
		var fixture struct {
			FinalMessage  string   `json:"final_message"`
			LCTKToolCalls []string `json:"lctk_tool_calls"`
			Usage         Usage    `json:"usage"`
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			return ParsedTrace{}, fmt.Errorf("decode fixture trace: %w", err)
		}
		return ParsedTrace{FinalMessage: fixture.FinalMessage, LCTKToolCalls: fixture.LCTKToolCalls, Usage: fixture.Usage}, nil
	default:
		return ParsedTrace{}, fmt.Errorf("unsupported trace provider %q", provider)
	}
}

// ParseRegions requires the benchmark's explicit final block. There is no
// fallback regex sweep because accepting incidental paths makes malformed agent
// output look successful.
func ParseRegions(message, _ string, topK int) ([]Region, error) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	header := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "RELEVANT_FILES:" {
			if header >= 0 {
				return nil, errors.New("agent output contains more than one RELEVANT_FILES block")
			}
			header = index
		}
	}
	if header < 0 {
		return nil, errors.New("agent output has no RELEVANT_FILES block")
	}
	regions := make([]Region, 0, topK)
	for _, line := range lines[header+1:] {
		if strings.TrimSpace(line) == "" {
			if len(regions) > 0 {
				break
			}
			continue
		}
		match := regionLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			return nil, fmt.Errorf("invalid region line %q", strings.TrimSpace(line))
		}
		start, _ := strconv.Atoi(match[2])
		end, _ := strconv.Atoi(match[3])
		region, err := validateRegion(Region{Path: match[1], Start: start, End: end})
		if err != nil {
			return nil, err
		}
		regions = append(regions, region)
		if len(regions) > topK {
			return nil, fmt.Errorf("agent returned %d regions, top_k is %d", len(regions), topK)
		}
	}
	if len(regions) == 0 {
		return nil, errors.New("RELEVANT_FILES block contains no regions")
	}
	return regions, nil
}

// validateRegion canonicalizes only the path safety boundary. SWE-Explore's
// official evaluator intentionally preserves positive intervals outside a
// file's line count and scores those nonexistent lines as prediction noise, so
// the harness must not reject or clamp the model's measured output first.
func validateRegion(region Region) (Region, error) {
	path := filepath.Clean(filepath.FromSlash(strings.TrimSpace(region.Path)))
	if path == "." || filepath.IsAbs(path) || strings.HasPrefix(path, ".."+string(filepath.Separator)) || path == ".." {
		return Region{}, fmt.Errorf("region path %q is not project-relative", region.Path)
	}
	region.Path = filepath.ToSlash(path)
	return region, nil
}

func countLines(path string) (int, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(body) == 0 {
		return 0, nil
	}
	count := bytes.Count(body, []byte{'\n'})
	if body[len(body)-1] != '\n' {
		count++
	}
	return count, nil
}

func parseCodexTrace(raw []byte) (ParsedTrace, error) {
	var parsed ParsedTrace
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return ParsedTrace{}, fmt.Errorf("decode Codex JSONL: %w", err)
		}
		typeName, _ := event["type"].(string)
		if typeName == "item.completed" {
			item, _ := event["item"].(map[string]any)
			itemType, _ := item["type"].(string)
			if itemType == "agent_message" {
				parsed.FinalMessage, _ = item["text"].(string)
			}
			if strings.Contains(itemType, "mcp") {
				server := firstString(item, "server", "server_name")
				tool := firstString(item, "tool", "name", "tool_name")
				status := firstString(item, "status")
				if isLCTKServer(server) && status != "failed" && item["error"] == nil {
					parsed.LCTKToolCalls = append(parsed.LCTKToolCalls, "mcp__"+server+"__"+tool)
				} else if server == "" && isLCTKTool(tool) && status != "failed" && item["error"] == nil {
					parsed.LCTKToolCalls = append(parsed.LCTKToolCalls, tool)
				}
			}
		}
		if typeName == "turn.completed" {
			if usage, ok := event["usage"].(map[string]any); ok {
				parsed.Usage = usageFromMap(usage)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ParsedTrace{}, err
	}
	if parsed.FinalMessage == "" {
		return ParsedTrace{}, errors.New("Codex trace contains no completed agent message")
	}
	return parsed, nil
}

func parseClaudeTrace(raw []byte) (ParsedTrace, error) {
	var parsed ParsedTrace
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return ParsedTrace{}, fmt.Errorf("decode Claude JSONL: %w", err)
		}
		typeName, _ := event["type"].(string)
		if typeName == "assistant" {
			message, _ := event["message"].(map[string]any)
			if model, ok := message["model"].(string); ok {
				parsed.ActualModels = appendUnique(parsed.ActualModels, model)
			}
			content, _ := message["content"].([]any)
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				switch block["type"] {
				case "text":
					if value, ok := block["text"].(string); ok {
						parsed.FinalMessage = value
					}
				case "tool_use":
					name, _ := block["name"].(string)
					if isLCTKTool(name) {
						parsed.LCTKToolCalls = append(parsed.LCTKToolCalls, name)
					}
				}
			}
		}
		if typeName == "result" {
			if value, ok := event["result"].(string); ok && value != "" {
				parsed.FinalMessage = value
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				parsed.Usage = usageFromMap(usage)
			}
			parsed.ProviderStats = ProviderMetrics{
				ReportedCostUSD: number(event, "total_cost_usd"),
				DurationMS:      integer(event, "duration_ms"),
				APIDurationMS:   integer(event, "duration_api_ms"),
				Turns:           integer(event, "num_turns"),
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ParsedTrace{}, err
	}
	if parsed.FinalMessage == "" {
		return ParsedTrace{}, errors.New("Claude trace contains no final result")
	}
	return parsed, nil
}

// appendUnique preserves first-observed model order while avoiding duplicate
// entries from provider traces that repeat the model on every message.
func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func isLCTKTool(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "lctk") && strings.Contains(lower, "mcp")
}

func isLCTKServer(name string) bool {
	return strings.Contains(strings.ToLower(name), "lctk")
}

func usageFromMap(values map[string]any) Usage {
	return Usage{
		InputTokens:              integer(values, "input_tokens"),
		CachedInputTokens:        integer(values, "cached_input_tokens", "cache_read_input_tokens"),
		CacheCreationInputTokens: integer(values, "cache_creation_input_tokens"),
		OutputTokens:             integer(values, "output_tokens"),
		ReasoningOutputTokens:    integer(values, "reasoning_output_tokens"),
	}
}

func integer(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := values[key].(float64); ok {
			return int64(value)
		}
	}
	return 0
}

func number(values map[string]any, key string) float64 {
	value, _ := values[key].(float64)
	return value
}
