package gateway

import (
	"encoding/json"
	"sort"

	"github.com/SureshKhemka/llm-gateway/internal/observe"
)

// CandidateInfo is one deployment that passed every hard constraint.
type CandidateInfo struct {
	Deployment   string  `json:"deployment"`
	Provider     string  `json:"provider"`
	Tier         int     `json:"tier"`
	EstCostUSD   float64 `json:"est_cost_usd"`
	EstLatencyMs float64 `json:"est_latency_ms"`
	Score        float64 `json:"score"`
	Health       string  `json:"health,omitempty"`
}

// Exclusion records why a deployment was not a candidate [RTE-02].
type Exclusion struct {
	Deployment string   `json:"deployment"`
	Reasons    []string `json:"reasons"`
}

// Decision is the route decision record [RTE-14].
type Decision struct {
	RequestID         string             `json:"request_id"`
	RequestedModel    string             `json:"requested_model"`
	Policy            string             `json:"policy"`
	PolicyVersion     int                `json:"policy_version"`
	PolicySource      string             `json:"policy_source"`
	Objective         string             `json:"objective"`
	Estimator         string             `json:"estimator,omitempty"`
	Difficulty        float64            `json:"difficulty_score"`
	DifficultySignals map[string]float64 `json:"difficulty_signals,omitempty"`
	MinTier           int                `json:"min_tier"`
	MaxTier           int                `json:"max_tier"`
	PromptTokens      int                `json:"prompt_tokens"`
	MaxOutputTokens   int                `json:"max_output_tokens"`
	Required          []string           `json:"required_capabilities,omitempty"`
	Candidates        []CandidateInfo    `json:"candidates"`
	Exclusions        []Exclusion        `json:"exclusions,omitempty"`
	Ordered           []string           `json:"ordered"`
	Chosen            string             `json:"chosen"`
	Reason            string             `json:"reason"`
	Degraded          string             `json:"degraded,omitempty"`
	Pinned            bool               `json:"session_pinned,omitempty"`
	SplitVariant      string             `json:"split_variant,omitempty"`
	Cascade           bool               `json:"cascade"`
	Attempts          []observe.Attempt  `json:"attempts"`
	EstCostUSD        float64            `json:"est_cost_usd"`
	ClassifierCostUSD float64            `json:"classifier_cost_usd,omitempty"`
	ClassifierMs      float64            `json:"classifier_ms,omitempty"`
	RoutingMs         float64            `json:"routing_ms"`
	TotalCostUSD      float64            `json:"total_cost_usd"`
}

func (d *Decision) JSON() json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}

func sortStrings(s []string) { sort.Strings(s) }
