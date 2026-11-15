package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LeaderboardURL is the public Artificial Analysis model leaderboard.
const LeaderboardURL = "https://artificialanalysis.ai/leaderboards/models"

const (
	capabilityCacheTTL  = 10 * time.Minute
	capabilityTimeout   = 15 * time.Second
	capabilityMaxBody   = 8 * 1024 * 1024
	capabilityUserAgent = "Mozilla/5.0 (compatible; literouter/0.1; +https://github.com/Schallophy/literouter)"
)

var (
	effortSuffixes = []string{
		"none", "minimal", "low", "medium", "high", "xhigh", "max", "thinking", "non-reasoning", "reasoning",
	}
	variantSuffixes = []string{
		"free", "batch", "realtime", "online", "turbo", "latest", "preview", "exp", "it", "instruct", "highspeed", "ultraspeed",
	}
	vendorPrefixes = []string{"nvidia", "tencent"}

	flightPushPattern = regexp.MustCompile(`self\.__next_f\.push\(\[1,("(?:[^"\\]|\\.)*")\]\)`)
	dateSuffixPattern = regexp.MustCompile(`-(20\d{2}[-.]\d{2}[-.]\d{2}|\d{8}|\d{6}|\d{4})$`)
	parameterPattern  = regexp.MustCompile(`-([a-z]?\d+(?:\.\d+)?[bm])$`)
)

// CapabilityModel is one leaderboard row.
type CapabilityModel struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Creator       *string  `json:"creator"`
	Score         *float64 `json:"score"`
	ContextWindow *int     `json:"contextWindow"`
	Reasoning     bool     `json:"reasoning"`
}

// IndexedModel ties a leaderboard row to its match cost.
type IndexedModel struct {
	Model CapabilityModel
	Cost  int
}

// CapabilityIndex maps normalized name variants to candidates.
type CapabilityIndex map[string][]IndexedModel

// CapabilityMatch is a resolved leaderboard row for an upstream model name.
type CapabilityMatch struct {
	Model CapabilityModel
	Cost  int
}

// CapabilitySnapshot is what the admin API serves.
type CapabilitySnapshot struct {
	Models    []CapabilityModel `json:"models"`
	FetchedAt *string           `json:"fetchedAt"`
	Cached    bool              `json:"cached"`
	Total     int               `json:"total"`
	Scored    int               `json:"scored"`
	Error     *string           `json:"error"`
	Index     CapabilityIndex   `json:"-"`
}

type capabilityDataset struct {
	models    []CapabilityModel
	index     CapabilityIndex
	fetchedAt time.Time
	total     int
	scored    int
}

// CapabilityService caches the leaderboard in memory; nothing is persisted.
type CapabilityService struct {
	mu       sync.Mutex
	cache    *capabilityDataset
	fetching bool
	wait     chan struct{}
	client   *http.Client
}

// NewCapabilityService creates a service using the given HTTP client (nil uses
// a plain client without a global timeout).
func NewCapabilityService(client *http.Client) *CapabilityService {
	if client == nil {
		client = &http.Client{}
	}
	return &CapabilityService{client: client}
}

// Snapshot returns the cached leaderboard, refreshing when stale or forced.
func (s *CapabilityService) Snapshot(refresh bool, now time.Time) CapabilitySnapshot {
	s.mu.Lock()
	if !refresh && s.cache != nil && now.Sub(s.cache.fetchedAt) < capabilityCacheTTL {
		snapshot := snapshotOf(s.cache, true, nil)
		s.mu.Unlock()
		return snapshot
	}
	if s.fetching {
		wait := s.wait
		s.mu.Unlock()
		<-wait
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cache != nil {
			return snapshotOf(s.cache, true, nil)
		}
		return emptySnapshot(nil)
	}
	s.fetching = true
	s.wait = make(chan struct{})
	s.mu.Unlock()

	dataset, err := s.fetchDataset(now)
	s.mu.Lock()
	var cached bool
	var previous *capabilityDataset
	if err == nil {
		s.cache = dataset
		previous = dataset
	} else {
		previous = s.cache
		cached = previous != nil
	}
	s.fetching = false
	close(s.wait)
	s.mu.Unlock()

	if err != nil {
		message := describeCapabilityError(err)
		if previous != nil {
			return snapshotOf(previous, true, &message)
		}
		return emptySnapshot(&message)
	}
	return snapshotOf(dataset, cached, nil)
}

func snapshotOf(dataset *capabilityDataset, cached bool, failure *string) CapabilitySnapshot {
	fetchedAt := dataset.fetchedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	return CapabilitySnapshot{
		Models:    dataset.models,
		FetchedAt: &fetchedAt,
		Cached:    cached,
		Total:     dataset.total,
		Scored:    dataset.scored,
		Error:     failure,
		Index:     dataset.index,
	}
}

func emptySnapshot(failure *string) CapabilitySnapshot {
	return CapabilitySnapshot{
		Models:  []CapabilityModel{},
		Total:   0,
		Scored:  0,
		Error:   failure,
		Index:   CapabilityIndex{},
	}
}

func describeCapabilityError(err error) string {
	if err == nil {
		return ""
	}
	var netErr interface{ Timeout() bool }
	if failure, ok := err.(interface{ Timeout() bool }); ok && failure.Timeout() {
		return "请求 Artificial Analysis 超时"
	}
	_ = netErr
	if err == context.DeadlineExceeded {
		return "请求 Artificial Analysis 超时"
	}
	return err.Error()
}

func (s *CapabilityService) fetchDataset(now time.Time) (*capabilityDataset, error) {
	ctx, cancel := context.WithTimeout(context.Background(), capabilityTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, LeaderboardURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "text/html,application/xhtml+xml")
	request.Header.Set("user-agent", capabilityUserAgent)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, capabilityMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > capabilityMaxBody {
		return nil, fmt.Errorf("排行榜页面过大")
	}
	models := ParseLeaderboardHTML(string(body))
	if len(models) == 0 {
		return nil, fmt.Errorf("页面里没有找到模型数据")
	}
	scored := 0
	for _, model := range models {
		if model.Score != nil {
			scored++
		}
	}
	if scored == 0 {
		return nil, fmt.Errorf("排行榜里还没有评分数据")
	}
	return &capabilityDataset{
		models:    models,
		index:     BuildCapabilityIndex(models),
		fetchedAt: now,
		total:     len(models),
		scored:    scored,
	}, nil
}

// ParseLeaderboardHTML extracts initialModels from Next.js flight chunks.
func ParseLeaderboardHTML(html string) []CapabilityModel {
	payload := flightPayload(html)
	if payload == "" {
		return nil
	}
	raw := extractJSONArray(payload, "initialModels")
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	models := make([]CapabilityModel, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		slug := asString(record["slug"])
		if slug == "" {
			continue
		}
		name := asString(record["name"])
		if name == "" {
			name = slug
		}
		models = append(models, CapabilityModel{
			Slug:          slug,
			Name:          name,
			Creator:       stringOrNil(record["modelCreatorName"]),
			Score:         numberOrNil(record["intelligenceIndex"]),
			ContextWindow: intOrNil(record["contextWindowTokens"]),
			Reasoning:     record["isReasoning"] == true,
		})
	}
	return models
}

func flightPayload(html string) string {
	var builder strings.Builder
	for _, match := range flightPushPattern.FindAllStringSubmatch(html, -1) {
		if len(match) < 2 {
			continue
		}
		var decoded string
		if err := json.Unmarshal([]byte(match[1]), &decoded); err == nil {
			builder.WriteString(decoded)
		}
	}
	return builder.String()
}

func extractJSONArray(text, key string) any {
	marker := `"` + key + `":`
	markerIndex := strings.Index(text, marker)
	if markerIndex < 0 {
		return nil
	}
	start := strings.Index(text[markerIndex+len(marker):], "[")
	if start < 0 {
		return nil
	}
	start += markerIndex + len(marker)
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
		} else if char == '[' {
			depth++
		} else if char == ']' {
			depth--
			if depth == 0 {
				var parsed any
				if err := json.Unmarshal([]byte(text[start:index+1]), &parsed); err != nil {
					return nil
				}
				return parsed
			}
		}
	}
	return nil
}

// BuildCapabilityIndex indexes every variant of every leaderboard slug.
func BuildCapabilityIndex(models []CapabilityModel) CapabilityIndex {
	superseded := dateSupersededSlugs(models)
	index := CapabilityIndex{}
	for _, model := range models {
		if model.Score == nil && superseded[NormalizeModelName(model.Slug)] {
			continue
		}
		for _, variant := range nameVariants(model.Slug, false) {
			list := index[variant.key]
			found := -1
			for i := range list {
				if list[i].Model.Slug == model.Slug {
					found = i
					break
				}
			}
			if found >= 0 {
				if variant.cost < list[found].Cost {
					list[found].Cost = variant.cost
				}
			} else {
				list = append(list, IndexedModel{Model: model, Cost: variant.cost})
			}
			index[variant.key] = list
		}
	}
	return index
}

func dateSupersededSlugs(models []CapabilityModel) map[string]bool {
	superseded := map[string]bool{}
	for _, model := range models {
		if model.Score == nil {
			continue
		}
		base := NormalizeModelName(model.Slug)
		stripped := stripDate(base)
		if stripped != base {
			superseded[stripped] = true
		}
	}
	return superseded
}

// MatchCapability resolves an upstream model name to its leaderboard row.
// Only the lowest-cost candidate tier is considered.
func MatchCapability(index CapabilityIndex, upstream string) (CapabilityMatch, bool) {
	type candidate struct {
		model CapabilityModel
		cost  int
	}
	var candidates []candidate
	for _, variant := range nameVariants(upstream, true) {
		for _, entry := range index[variant.key] {
			cost := variant.cost
			if entry.Cost > cost {
				cost = entry.Cost
			}
			candidates = append(candidates, candidate{model: entry.Model, cost: cost})
		}
	}
	if len(candidates) == 0 {
		return CapabilityMatch{}, false
	}
	lowest := candidates[0].cost
	for _, entry := range candidates[1:] {
		if entry.cost < lowest {
			lowest = entry.cost
		}
	}
	var best CapabilityMatch
	bestScore := 0.0
	found := false
	for _, entry := range candidates {
		if entry.cost != lowest || entry.model.Score == nil {
			continue
		}
		score := *entry.model.Score
		if !found || score > bestScore {
			best = CapabilityMatch{Model: entry.model, Cost: entry.cost}
			bestScore = score
			found = true
		}
	}
	return best, found
}

type nameVariant struct {
	key  string
	cost int
}

func nameVariants(value string, parameterAlias bool) []nameVariant {
	base := NormalizeModelName(value)
	if base == "" {
		return nil
	}
	order := make([]string, 0, 8)
	costs := map[string]int{}
	add := func(key string, cost int) {
		if key == "" {
			return
		}
		if previous, ok := costs[key]; ok {
			if cost < previous {
				costs[key] = cost
			}
			return
		}
		costs[key] = cost
		order = append(order, key)
	}

	noEffort := stripSuffix(base, effortSuffixes)
	noVariant := stripSuffix(base, variantSuffixes)
	noVariantDate := stripDate(noVariant)

	add(base, 0)
	add(noEffort, 1)
	add(noVariant, 1)
	add(stripDate(base), 2)
	add(noVariantDate, 2)
	add(stripSuffix(stripDate(base), effortSuffixes), 3)
	add(stripSuffix(noVariantDate, effortSuffixes), 3)
	add(stripTrailingZero(base), 2)
	if parameterAlias {
		add(stripParameter(base), 2)
	}
	for _, prefix := range vendorPrefixes {
		if strings.HasPrefix(base, prefix+"-") {
			add(base[len(prefix)+1:], 1)
		}
	}

	variants := make([]nameVariant, 0, len(order))
	for _, key := range order {
		variants = append(variants, nameVariant{key: key, cost: costs[key]})
	}
	return variants
}

var normalizeRuns = regexp.MustCompile(`-+`)
var normalizeSeparators = regexp.MustCompile(`[._\s]+`)
var normalizeTag = regexp.MustCompile(`:[a-z0-9._-]+$`)

// NormalizeModelName lowercases a model name and removes provider paths,
// separators, tags and duplicate dashes.
func NormalizeModelName(value string) string {
	name := strings.ToLower(strings.TrimSpace(value))
	if slash := strings.Index(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	name = normalizeSeparators.ReplaceAllString(name, "-")
	name = normalizeTag.ReplaceAllString(name, "")
	name = normalizeRuns.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	return name
}

func stripSuffix(value string, suffixes []string) string {
	out := value
	changed := true
	for changed {
		changed = false
		for _, suffix := range suffixes {
			if strings.HasSuffix(out, "-"+suffix) {
				out = out[:len(out)-len(suffix)-1]
				changed = true
			}
		}
	}
	return out
}

func stripDate(value string) string {
	return dateSuffixPattern.ReplaceAllString(value, "")
}

func stripTrailingZero(value string) string {
	if strings.HasSuffix(value, "-0") {
		return value[:len(value)-2]
	}
	return value
}

func stripParameter(value string) string {
	match := parameterPattern.FindStringSubmatchIndex(value)
	if match == nil {
		return value
	}
	head := value[:match[0]]
	if !strings.ContainsAny(head, "abcdefghijklmnopqrstuvwxyz") || strings.ContainsAny(head, "0123456789") {
		return value
	}
	segments := 0
	for _, segment := range strings.Split(head, "-") {
		if segment != "" {
			segments++
		}
	}
	if segments < 2 {
		return value
	}
	return head
}

func asString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func stringOrNil(value any) *string {
	text := asString(value)
	if text == "" {
		return nil
	}
	return &text
}

func numberOrNil(value any) *float64 {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	return &number
}

func intOrNil(value any) *int {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	converted := int(number)
	return &converted
}

// SortCatalogEntries sorts by family then code, matching the dashboard order.
func SortCatalogEntries(entries []string) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i] < entries[j] })
}
