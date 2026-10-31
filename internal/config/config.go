// Package config defines the declarative configuration, loads it from YAML files
// and validates it with file/path-precise errors [CFG-01, CFG-02, CFG-04].
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration parses Go duration strings ("30s", "250ms") in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON / UnmarshalJSON use the same "30s" form as YAML (the admin API).
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}
func (d Duration) D() time.Duration { return time.Duration(d) }

// SecretRef is a reference to a secret, never the secret itself [CFG-04, PRV-08]:
// "env:NAME" reads an environment variable, "file:/path" reads a mounted secret file
// (Kubernetes secrets, Vault agent, etc.).
type SecretRef string

func (s SecretRef) IsZero() bool { return s == "" }

// Resolve returns the secret's value.
func (s SecretRef) Resolve() (string, error) {
	switch {
	case s == "":
		return "", nil
	case strings.HasPrefix(string(s), "env:"):
		name := strings.TrimPrefix(string(s), "env:")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return v, nil
	case strings.HasPrefix(string(s), "file:"):
		b, err := os.ReadFile(strings.TrimPrefix(string(s), "file:"))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	default:
		return "", errors.New("not a secret reference")
	}
}

func (s SecretRef) valid() bool {
	return s == "" || strings.HasPrefix(string(s), "env:") || strings.HasPrefix(string(s), "file:")
}

// String never reveals anything but the reference.
func (s SecretRef) String() string { return string(s) }

type Config struct {
	Server        Server          `yaml:"server" json:"server,omitempty"`
	Storage       Storage         `yaml:"storage" json:"storage,omitempty"`
	Providers     []Provider      `yaml:"providers" json:"providers,omitempty"`
	Deployments   []Deployment    `yaml:"deployments" json:"deployments,omitempty"`
	VirtualModels []VirtualModel  `yaml:"virtual_models" json:"virtual_models,omitempty"`
	Policies      []Policy        `yaml:"policies" json:"policies,omitempty"`
	Tenants       []Tenant        `yaml:"tenants" json:"tenants,omitempty"`
	Admins        []AdminCred     `yaml:"admins" json:"admins,omitempty"`
	Budgets       []Budget        `yaml:"budgets" json:"budgets,omitempty"`
	Alerts        []AlertSink     `yaml:"alerts" json:"alerts,omitempty"`
	Cache         Cache           `yaml:"cache" json:"cache,omitempty"`
	Guardrails    Guardrails      `yaml:"guardrails" json:"guardrails,omitempty"`
	Resilience    Resilience      `yaml:"resilience" json:"resilience,omitempty"`
	Observability Observability   `yaml:"observability" json:"observability,omitempty"`
	Defaults      RoutingDefaults `yaml:"defaults" json:"defaults,omitempty"`
	Evals         Evals           `yaml:"evals" json:"evals,omitempty"`

	// Files lists the files this config was loaded from (for messages).
	Files []string `yaml:"-" json:"-"`
	// origin maps "kind/id" to the file that defined it.
	origin map[string]string
}

// CopyOrigins copies file-origin bookkeeping from another config (after cloning).
func (c *Config) CopyOrigins(from *Config) {
	c.origin = map[string]string{}
	for k, v := range from.origin {
		c.origin[k] = v
	}
}

// FileOf returns the file that defined the object kind/id (or the first file).
func (c *Config) FileOf(kind, id string) string {
	if f, ok := c.origin[kind+"/"+id]; ok {
		return f
	}
	if len(c.Files) > 0 {
		return c.Files[0]
	}
	return "<config>"
}

type Server struct {
	Listen        string   `yaml:"listen" json:"listen,omitempty"`
	AdminListen   string   `yaml:"admin_listen" json:"admin_listen,omitempty"` // empty: admin API on the main listener
	DrainTimeout  Duration `yaml:"drain_timeout" json:"drain_timeout,omitempty"`
	MaxBodyBytes  int64    `yaml:"max_body_bytes" json:"max_body_bytes,omitempty"`
	MaxInFlight   int      `yaml:"max_in_flight" json:"max_in_flight,omitempty"` // load shedding threshold [RES-09]
	ReloadEvery   Duration `yaml:"reload_every" json:"reload_every,omitempty"`   // config file poll interval
	TLSCertFile   string   `yaml:"tls_cert_file" json:"tls_cert_file,omitempty"`
	TLSKeyFile    string   `yaml:"tls_key_file" json:"tls_key_file,omitempty"`
	InstanceID    string   `yaml:"instance_id" json:"instance_id,omitempty"`
	TrustProxyIPs []string `yaml:"trust_proxy_ips" json:"trust_proxy_ips,omitempty"`
}

type Storage struct {
	RedisURL    SecretRef `yaml:"redis_url" json:"redis_url,omitempty"`
	PostgresDSN SecretRef `yaml:"postgres_dsn" json:"postgres_dsn,omitempty"`
	// BudgetsFailOpen lets requests through when Redis is unreachable (default: fail closed) [NFR-08].
	BudgetsFailOpen bool   `yaml:"budgets_fail_open" json:"budgets_fail_open,omitempty"`
	WALDir          string `yaml:"wal_dir" json:"wal_dir,omitempty"`
}

type Provider struct {
	ID       string            `yaml:"id" json:"id,omitempty"`
	Type     string            `yaml:"type" json:"type,omitempty"` // openai | anthropic
	BaseURL  string            `yaml:"base_url" json:"base_url,omitempty"`
	APIKey   SecretRef         `yaml:"api_key" json:"api_key,omitempty"`
	Headers  map[string]string `yaml:"headers" json:"headers,omitempty"`
	Discover bool              `yaml:"discover" json:"discover,omitempty"` // list models via /v1/models [PRV-06]
	// AutoApprove makes discovered models usable immediately with default metadata.
	AutoApprove bool `yaml:"auto_approve" json:"auto_approve,omitempty"`
	// AllowPrivateNetwork permits base URLs in private ranges (SSRF guard) [NFR-14].
	AllowPrivateNetwork *bool    `yaml:"allow_private_network" json:"allow_private_network,omitempty"`
	Timeouts            Timeouts `yaml:"timeouts" json:"timeouts,omitempty"`
	Enabled             *bool    `yaml:"enabled" json:"enabled,omitempty"`
	// ProbeModel is used for active health probes; empty uses the model list.
	ProbeEvery Duration `yaml:"probe_every" json:"probe_every,omitempty"`
	// DefaultDiscovered holds metadata applied to auto-approved discovered models.
	DefaultDiscovered *Deployment `yaml:"discovered_defaults" json:"discovered_defaults,omitempty"`
}

type Timeouts struct {
	Connect Duration `yaml:"connect" json:"connect,omitempty"`
	TTFT    Duration `yaml:"ttft" json:"ttft,omitempty"`
	Idle    Duration `yaml:"idle" json:"idle,omitempty"`
	Total   Duration `yaml:"total" json:"total,omitempty"`
}

type PriceCard struct {
	EffectiveFrom time.Time `yaml:"effective_from" json:"effective_from,omitempty"`
	Input         float64   `yaml:"input" json:"input,omitempty"` // USD per 1M tokens
	CachedInput   *float64  `yaml:"cached_input" json:"cached_input,omitempty"`
	Output        float64   `yaml:"output" json:"output,omitempty"`
	Reasoning     *float64  `yaml:"reasoning" json:"reasoning,omitempty"`
}

type Latency struct {
	TTFTMs       float64 `yaml:"ttft_ms" json:"ttft_ms,omitempty"`
	TokensPerSec float64 `yaml:"tokens_per_sec" json:"tokens_per_sec,omitempty"`
}

type Deployment struct {
	ID              string      `yaml:"id" json:"id,omitempty"`
	Provider        string      `yaml:"provider" json:"provider,omitempty"`
	Model           string      `yaml:"model" json:"model,omitempty"` // upstream model name
	Capabilities    []string    `yaml:"capabilities" json:"capabilities,omitempty"`
	ContextWindow   int         `yaml:"context_window" json:"context_window,omitempty"`
	MaxOutputTokens int         `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
	Tier            int         `yaml:"tier" json:"tier,omitempty"`
	Prices          []PriceCard `yaml:"prices" json:"prices,omitempty"`
	Latency         Latency     `yaml:"latency" json:"latency,omitempty"`
	Concurrency     int         `yaml:"concurrency" json:"concurrency,omitempty"`
	QueueWait       Duration    `yaml:"queue_wait" json:"queue_wait,omitempty"`
	Residency       []string    `yaml:"residency" json:"residency,omitempty"`
	Enabled         *bool       `yaml:"enabled" json:"enabled,omitempty"`
	Pool            string      `yaml:"pool" json:"pool,omitempty"`
	Weight          int         `yaml:"weight" json:"weight,omitempty"`
	Timeouts        Timeouts    `yaml:"timeouts" json:"timeouts,omitempty"`
	// Warm reports whether a local model is loaded; nil means unknown (treated warm).
	Warm *bool `yaml:"warm" json:"warm,omitempty"`
}

type VirtualModel struct {
	Name        string   `yaml:"name" json:"name,omitempty"`
	Policy      string   `yaml:"policy" json:"policy,omitempty"`
	Deployments []string `yaml:"deployments" json:"deployments,omitempty"` // empty: every chat deployment
	Description string   `yaml:"description" json:"description,omitempty"`
}

type DifficultyCfg struct {
	Estimator  string    `yaml:"estimator" json:"estimator,omitempty"`   // rules | similarity | classifier
	Thresholds []float64 `yaml:"thresholds" json:"thresholds,omitempty"` // score cut-offs for tiers 1..n
	Classifier string    `yaml:"classifier" json:"classifier,omitempty"` // deployment for classifier estimator
	Examples   string    `yaml:"examples" json:"examples,omitempty"`     // JSONL file of labelled prompts (similarity)
	Embedder   string    `yaml:"embedder" json:"embedder,omitempty"`     // embeddings deployment (similarity)
}

type CascadeCfg struct {
	Enabled        bool     `yaml:"enabled" json:"enabled,omitempty"`
	MaxEscalations int      `yaml:"max_escalations" json:"max_escalations,omitempty"`
	Validators     []string `yaml:"validators" json:"validators,omitempty"` // json_schema, non_empty, not_truncated, no_refusal, judge
	Judge          string   `yaml:"judge" json:"judge,omitempty"`           // deployment ID
	JudgeMinScore  float64  `yaml:"judge_min_score" json:"judge_min_score,omitempty"`
	BufferStreams  bool     `yaml:"buffer_streams" json:"buffer_streams,omitempty"`
}

type DegradeStep struct {
	AtPct   float64 `yaml:"at_pct" json:"at_pct,omitempty"`
	MaxTier int     `yaml:"max_tier" json:"max_tier,omitempty"`
}

type Split struct {
	Policy string `yaml:"policy" json:"policy,omitempty"`
	Weight int    `yaml:"weight" json:"weight,omitempty"`
}

type Shadow struct {
	Policy     string  `yaml:"policy" json:"policy,omitempty"`
	Deployment string  `yaml:"deployment" json:"deployment,omitempty"`
	SampleRate float64 `yaml:"sample_rate" json:"sample_rate,omitempty"`
}

type PolicyCache struct {
	SemanticThreshold *float64 `yaml:"semantic_threshold" json:"semantic_threshold,omitempty"`
	AllowTools        bool     `yaml:"allow_tools" json:"allow_tools,omitempty"`
	Disabled          bool     `yaml:"disabled" json:"disabled,omitempty"`
}

type Weights struct {
	Cost    float64 `yaml:"cost" json:"cost,omitempty"`
	Latency float64 `yaml:"latency" json:"latency,omitempty"`
	Quality float64 `yaml:"quality" json:"quality,omitempty"`
}

type Policy struct {
	Name             string        `yaml:"name" json:"name,omitempty"`
	Version          int           `yaml:"version" json:"version,omitempty"`
	Objective        string        `yaml:"objective" json:"objective,omitempty"` // min_cost | min_latency | max_quality | weighted
	Weights          Weights       `yaml:"weights" json:"weights,omitempty"`
	Difficulty       DifficultyCfg `yaml:"difficulty" json:"difficulty,omitempty"`
	MinTier          *int          `yaml:"min_tier" json:"min_tier,omitempty"`
	MaxTier          *int          `yaml:"max_tier" json:"max_tier,omitempty"`
	MaxCostUSD       *float64      `yaml:"max_cost_usd" json:"max_cost_usd,omitempty"`
	LatencyTargetMs  *float64      `yaml:"latency_target_ms" json:"latency_target_ms,omitempty"`
	AllowedProviders []string      `yaml:"allowed_providers" json:"allowed_providers,omitempty"`
	DeniedProviders  []string      `yaml:"denied_providers" json:"denied_providers,omitempty"`
	Residency        []string      `yaml:"residency" json:"residency,omitempty"`
	Cascade          CascadeCfg    `yaml:"cascade" json:"cascade,omitempty"`
	Degrade          []DegradeStep `yaml:"degrade" json:"degrade,omitempty"`
	Cache            PolicyCache   `yaml:"cache" json:"cache,omitempty"`
	Split            []Split       `yaml:"split" json:"split,omitempty"`
	Shadow           *Shadow       `yaml:"shadow" json:"shadow,omitempty"`
	MaxAttempts      int           `yaml:"max_attempts" json:"max_attempts,omitempty"`
	ExpectedOutput   int           `yaml:"expected_output_tokens" json:"expected_output_tokens,omitempty"`
	Baseline         string        `yaml:"baseline" json:"baseline,omitempty"`
	Description      string        `yaml:"description" json:"description,omitempty"`
}

type RateLimits struct {
	RPM        int `yaml:"rpm" json:"rpm,omitempty"`
	ITPM       int `yaml:"itpm" json:"itpm,omitempty"`
	OTPM       int `yaml:"otpm" json:"otpm,omitempty"`
	Concurrent int `yaml:"concurrent" json:"concurrent,omitempty"`
}

type Caps struct {
	MaxRequestBytes int `yaml:"max_request_bytes" json:"max_request_bytes,omitempty"`
	MaxMessages     int `yaml:"max_messages" json:"max_messages,omitempty"`
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
}

type Scopes struct {
	Models           []string   `yaml:"models" json:"models,omitempty"` // glob patterns; empty = all
	Endpoints        []string   `yaml:"endpoints" json:"endpoints,omitempty"`
	Providers        []string   `yaml:"providers" json:"providers,omitempty"`
	MaxCostUSD       *float64   `yaml:"max_cost_usd" json:"max_cost_usd,omitempty"`
	RateLimits       RateLimits `yaml:"rate_limits" json:"rate_limits,omitempty"`
	Priority         int        `yaml:"priority" json:"priority,omitempty"` // higher is more important [RES-09]
	IPAllow          []string   `yaml:"ip_allow" json:"ip_allow,omitempty"`
	DefaultTags      []string   `yaml:"default_tags" json:"default_tags,omitempty"`
	Policy           string     `yaml:"policy" json:"policy,omitempty"`
	Caps             Caps       `yaml:"caps" json:"caps,omitempty"`
	Baseline         string     `yaml:"baseline" json:"baseline,omitempty"`
	AllowedResidency []string   `yaml:"allowed_residency" json:"allowed_residency,omitempty"`
}

type Key struct {
	ID        string     `yaml:"id" json:"id,omitempty"`
	Name      string     `yaml:"name" json:"name,omitempty"`
	Secret    SecretRef  `yaml:"secret" json:"secret,omitempty"` // bootstrap keys only; API-created keys are stored hashed
	ExpiresAt *time.Time `yaml:"expires_at" json:"expires_at,omitempty"`
	Scopes    Scopes     `yaml:"scopes" json:"scopes,omitempty"`
}

type Project struct {
	ID       string     `yaml:"id" json:"id,omitempty"`
	Name     string     `yaml:"name" json:"name,omitempty"`
	Policy   string     `yaml:"policy" json:"policy,omitempty"`
	Limits   RateLimits `yaml:"rate_limits" json:"rate_limits,omitempty"`
	Baseline string     `yaml:"baseline" json:"baseline,omitempty"`
	Keys     []Key      `yaml:"keys" json:"keys,omitempty"`
}

type TenantCache struct {
	Shared bool `yaml:"shared" json:"shared,omitempty"` // opt into the cross-tenant namespace [CCH-03]
}

type BodyLogging struct {
	Enabled   bool     `yaml:"enabled" json:"enabled,omitempty"`
	Retention Duration `yaml:"retention" json:"retention,omitempty"`
}

type Tenant struct {
	ID            string      `yaml:"id" json:"id,omitempty"`
	Name          string      `yaml:"name" json:"name,omitempty"`
	Policy        string      `yaml:"policy" json:"policy,omitempty"`
	Limits        RateLimits  `yaml:"rate_limits" json:"rate_limits,omitempty"`
	Caps          Caps        `yaml:"caps" json:"caps,omitempty"`
	Baseline      string      `yaml:"baseline" json:"baseline,omitempty"`
	Cache         TenantCache `yaml:"cache" json:"cache,omitempty"`
	BodyLogging   BodyLogging `yaml:"body_logging" json:"body_logging,omitempty"`
	PII           string      `yaml:"pii" json:"pii,omitempty"` // off | log | redact | block
	AllowedModels []string    `yaml:"allowed_models" json:"allowed_models,omitempty"`
	Projects      []Project   `yaml:"projects" json:"projects,omitempty"`
}

type AdminCred struct {
	ID     string    `yaml:"id" json:"id,omitempty"`
	Role   string    `yaml:"role" json:"role,omitempty"` // org_admin | tenant_admin | finops_viewer | auditor
	Tenant string    `yaml:"tenant" json:"tenant,omitempty"`
	Secret SecretRef `yaml:"secret" json:"secret,omitempty"`
}

type Budget struct {
	ID        string    `yaml:"id" json:"id,omitempty"`
	Scope     string    `yaml:"scope" json:"scope,omitempty"` // tenant | project | key | end_user | eval | shadow
	ScopeID   string    `yaml:"scope_id" json:"scope_id,omitempty"`
	Period    string    `yaml:"period" json:"period,omitempty"` // daily | weekly | monthly | custom
	Custom    Duration  `yaml:"custom_period" json:"custom_period,omitempty"`
	Anchor    time.Time `yaml:"anchor" json:"anchor,omitempty"`
	Timezone  string    `yaml:"timezone" json:"timezone,omitempty"`
	HardUSD   float64   `yaml:"hard_usd" json:"hard_usd,omitempty"`
	SoftPct   []float64 `yaml:"soft_pct" json:"soft_pct,omitempty"`
	AlertSink []string  `yaml:"alerts" json:"alerts,omitempty"`
}

type AlertSink struct {
	ID      string    `yaml:"id" json:"id,omitempty"`
	Type    string    `yaml:"type" json:"type,omitempty"` // webhook | slack
	URL     SecretRef `yaml:"url" json:"url,omitempty"`
	Timeout Duration  `yaml:"timeout" json:"timeout,omitempty"`
	// AllowPrivateNetwork permits a sink on a private or loopback address (SSRF guard) [NFR-14].
	AllowPrivateNetwork bool `yaml:"allow_private_network" json:"allow_private_network,omitempty"`
}

type Cache struct {
	Enabled        bool     `yaml:"enabled" json:"enabled,omitempty"`
	TTL            Duration `yaml:"ttl" json:"ttl,omitempty"`
	MaxEntries     int      `yaml:"max_entries" json:"max_entries,omitempty"`
	MaxTemperature float64  `yaml:"max_temperature" json:"max_temperature,omitempty"`
	// DefaultTemperature is assumed when a request sets none (providers default to ~1).
	DefaultTemperature   float64 `yaml:"default_temperature" json:"default_temperature,omitempty"`
	Semantic             bool    `yaml:"semantic" json:"semantic,omitempty"`
	SemanticThreshold    float64 `yaml:"semantic_threshold" json:"semantic_threshold,omitempty"`
	Embedder             string  `yaml:"embedder" json:"embedder,omitempty"`
	PriceUSD             float64 `yaml:"price_usd" json:"price_usd,omitempty"`
	JudgeHits            string  `yaml:"judge_hits" json:"judge_hits,omitempty"` // deployment to verify semantic hits [CCH-09]
	SampleRate           float64 `yaml:"sample_rate" json:"sample_rate,omitempty"`
	MaxSemanticPerBucket int     `yaml:"max_semantic_per_bucket" json:"max_semantic_per_bucket,omitempty"`
}

type Guardrails struct {
	Caps           Caps                `yaml:"caps" json:"caps,omitempty"`
	PII            string              `yaml:"pii" json:"pii,omitempty"`                 // off | log | redact | block (default for tenants)
	Sensitivity    map[string][]string `yaml:"sensitivity" json:"sensitivity,omitempty"` // level -> allowed residency tags
	PreWebhook     string              `yaml:"pre_webhook" json:"pre_webhook,omitempty"`
	PostWebhook    string              `yaml:"post_webhook" json:"post_webhook,omitempty"`
	WebhookTimeout Duration            `yaml:"webhook_timeout" json:"webhook_timeout,omitempty"`
	FailOpen       bool                `yaml:"fail_open" json:"fail_open,omitempty"`
}

type Breaker struct {
	Window       Duration `yaml:"window" json:"window,omitempty"`
	MinRequests  int      `yaml:"min_requests" json:"min_requests,omitempty"`
	ErrorRate    float64  `yaml:"error_rate" json:"error_rate,omitempty"`
	LatencyP95Ms float64  `yaml:"latency_p95_ms" json:"latency_p95_ms,omitempty"`
	CoolDown     Duration `yaml:"cool_down" json:"cool_down,omitempty"`
	Consecutive  int      `yaml:"consecutive_failures" json:"consecutive_failures,omitempty"`
}

type Resilience struct {
	MaxAttempts   int      `yaml:"max_attempts" json:"max_attempts,omitempty"`
	TotalDeadline Duration `yaml:"total_deadline" json:"total_deadline,omitempty"`
	BackoffBase   Duration `yaml:"backoff_base" json:"backoff_base,omitempty"`
	BackoffMax    Duration `yaml:"backoff_max" json:"backoff_max,omitempty"`
	Breaker       Breaker  `yaml:"breaker" json:"breaker,omitempty"`
	Timeouts      Timeouts `yaml:"timeouts" json:"timeouts,omitempty"`
	MaxTimeouts   Timeouts `yaml:"max_timeouts" json:"max_timeouts,omitempty"` // bounds for per-request overrides
	LatencyAlpha  float64  `yaml:"latency_alpha" json:"latency_alpha,omitempty"`
}

type Observability struct {
	LogRequests  *bool                `yaml:"log_requests" json:"log_requests,omitempty"`
	OTLPEndpoint string               `yaml:"otlp_endpoint" json:"otlp_endpoint,omitempty"`
	OTLPHeaders  map[string]SecretRef `yaml:"otlp_headers" json:"otlp_headers,omitempty"`
	Langfuse     *Exporter            `yaml:"langfuse" json:"langfuse,omitempty"`
	LangSmith    *Exporter            `yaml:"langsmith" json:"langsmith,omitempty"`
	TraceSample  float64              `yaml:"trace_sample" json:"trace_sample,omitempty"`
}

type Exporter struct {
	Endpoint string    `yaml:"endpoint" json:"endpoint,omitempty"`
	Key      SecretRef `yaml:"key" json:"key,omitempty"`
	Secret   SecretRef `yaml:"secret" json:"secret,omitempty"`
}

type RoutingDefaults struct {
	Policy          string  `yaml:"policy" json:"policy,omitempty"`
	Baseline        string  `yaml:"baseline" json:"baseline,omitempty"`
	MaxOutputTokens int     `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
	Currency        string  `yaml:"display_currency" json:"display_currency,omitempty"`
	ExchangeRate    float64 `yaml:"exchange_rate" json:"exchange_rate,omitempty"`
}

type Evals struct {
	Dir         string  `yaml:"dir" json:"dir,omitempty"`
	GateMaxDrop float64 `yaml:"gate_max_drop" json:"gate_max_drop,omitempty"`
	Judge       string  `yaml:"judge" json:"judge,omitempty"`
	Embedder    string  `yaml:"embedder" json:"embedder,omitempty"`
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

func expandEnv(b []byte) []byte {
	return envPattern.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := envPattern.FindSubmatch(m)
		if v, ok := os.LookupEnv(string(sub[1])); ok {
			return []byte(v)
		}
		return sub[3]
	})
}

// Load reads a file, or every *.yaml / *.yml file in a directory (lexical order),
// merging list sections by concatenation and scalar sections by override.
func Load(path string) (*Config, error) {
	files, err := configFiles(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var part Config
		raw = expandEnv(raw)
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&part); err != nil && !errors.Is(err, io.EOF) {
			return nil, &ValidationError{Issues: []Issue{{File: f, Path: "", Reason: cleanYAMLErr(err)}}}
		}
		merge(cfg, &part, topLevelKeys(raw), f)
	}
	cfg.Files = files
	ApplyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse loads configuration from bytes (used by tests and the admin API).
func Parse(name string, raw []byte) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(expandEnv(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, &ValidationError{Issues: []Issue{{File: name, Reason: cleanYAMLErr(err)}}}
	}
	cfg.Files = []string{name}
	tagSources(cfg, cfg, name)
	ApplyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func cleanYAMLErr(err error) string {
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

func configFiles(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	var files []string
	for _, pat := range []string{"*.yaml", "*.yml"} {
		m, _ := filepath.Glob(filepath.Join(path, pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no *.yaml files", path)
	}
	return files, nil
}

// tagSources records in dst which file defined each object of src.
func tagSources(dst, src *Config, file string) {
	if dst.origin == nil {
		dst.origin = map[string]string{}
	}
	set := func(kind, id string) { dst.origin[kind+"/"+id] = file }
	for _, p := range src.Providers {
		set("providers", p.ID)
	}
	for _, d := range src.Deployments {
		set("deployments", d.ID)
	}
	for _, v := range src.VirtualModels {
		set("virtual_models", v.Name)
	}
	for _, p := range src.Policies {
		set("policies", p.Name)
	}
	for _, t := range src.Tenants {
		set("tenants", t.ID)
	}
	for _, b := range src.Budgets {
		set("budgets", b.ID)
	}
}

// merge folds one file into dst: list sections are concatenated; any other top-level
// section present in the file replaces the earlier value.
func merge(dst, src *Config, present map[string]bool, file string) {
	tagSources(dst, src, file)
	dv := reflect.ValueOf(dst).Elem()
	sv := reflect.ValueOf(src).Elem()
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" || !present[name] {
			continue
		}
		if dv.Field(i).Kind() == reflect.Slice {
			dv.Field(i).Set(reflect.AppendSlice(dv.Field(i), sv.Field(i)))
		} else {
			dv.Field(i).Set(sv.Field(i))
		}
	}
}

func topLevelKeys(raw []byte) map[string]bool {
	var m map[string]yaml.Node
	_ = yaml.Unmarshal(raw, &m)
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return keys
}

func boolPtr(b bool) *bool { return &b }

// ApplyDefaults fills unset fields with documented defaults.
func ApplyDefaults(c *Config) {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.DrainTimeout == 0 {
		c.Server.DrainTimeout = Duration(30 * time.Second)
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = 32 << 20
	}
	if c.Server.ReloadEvery == 0 {
		c.Server.ReloadEvery = Duration(5 * time.Second)
	}
	if c.Defaults.Policy == "" {
		c.Defaults.Policy = "default"
	}
	if c.Defaults.MaxOutputTokens == 0 {
		c.Defaults.MaxOutputTokens = 4096
	}
	if c.Defaults.Currency == "" {
		c.Defaults.Currency = "USD"
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type == "" {
			p.Type = "openai"
		}
		if p.Enabled == nil {
			p.Enabled = boolPtr(true)
		}
		if p.ProbeEvery == 0 {
			p.ProbeEvery = Duration(15 * time.Second)
		}
	}
	for i := range c.Deployments {
		d := &c.Deployments[i]
		if d.Enabled == nil {
			d.Enabled = boolPtr(true)
		}
		if len(d.Capabilities) == 0 {
			d.Capabilities = []string{"chat"}
		}
		if d.ContextWindow == 0 {
			d.ContextWindow = 8192
		}
		if d.MaxOutputTokens == 0 {
			d.MaxOutputTokens = 4096
		}
		if d.Weight == 0 {
			d.Weight = 1
		}
		if d.QueueWait == 0 {
			d.QueueWait = Duration(2 * time.Second)
		}
	}
	r := &c.Resilience
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 3
	}
	if r.TotalDeadline == 0 {
		r.TotalDeadline = Duration(120 * time.Second)
	}
	if r.BackoffBase == 0 {
		r.BackoffBase = Duration(100 * time.Millisecond)
	}
	if r.BackoffMax == 0 {
		r.BackoffMax = Duration(2 * time.Second)
	}
	if r.Timeouts.Connect == 0 {
		r.Timeouts.Connect = Duration(5 * time.Second)
	}
	if r.Timeouts.TTFT == 0 {
		r.Timeouts.TTFT = Duration(60 * time.Second)
	}
	if r.Timeouts.Idle == 0 {
		r.Timeouts.Idle = Duration(30 * time.Second)
	}
	if r.Timeouts.Total == 0 {
		r.Timeouts.Total = Duration(10 * time.Minute)
	}
	if r.MaxTimeouts.Total == 0 {
		r.MaxTimeouts = Timeouts{Connect: Duration(30 * time.Second), TTFT: Duration(5 * time.Minute), Idle: Duration(5 * time.Minute), Total: Duration(30 * time.Minute)}
	}
	if r.Breaker.Window == 0 {
		r.Breaker.Window = Duration(30 * time.Second)
	}
	if r.Breaker.MinRequests == 0 {
		r.Breaker.MinRequests = 5
	}
	if r.Breaker.ErrorRate == 0 {
		r.Breaker.ErrorRate = 0.5
	}
	if r.Breaker.CoolDown == 0 {
		r.Breaker.CoolDown = Duration(10 * time.Second)
	}
	if r.LatencyAlpha == 0 {
		r.LatencyAlpha = 0.2
	}
	if c.Cache.TTL == 0 {
		c.Cache.TTL = Duration(time.Hour)
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 100000
	}
	if c.Cache.MaxTemperature == 0 {
		c.Cache.MaxTemperature = 0.3
	}
	if c.Cache.DefaultTemperature == 0 {
		c.Cache.DefaultTemperature = 1
	}
	if c.Cache.SemanticThreshold == 0 {
		c.Cache.SemanticThreshold = 0.92
	}
	if c.Cache.MaxSemanticPerBucket == 0 {
		c.Cache.MaxSemanticPerBucket = 2000
	}
	if c.Guardrails.PII == "" {
		c.Guardrails.PII = "off"
	}
	if c.Guardrails.WebhookTimeout == 0 {
		c.Guardrails.WebhookTimeout = Duration(2 * time.Second)
	}
	if c.Guardrails.Sensitivity == nil {
		c.Guardrails.Sensitivity = map[string][]string{"confidential": {"local"}}
	}
	if c.Observability.LogRequests == nil {
		c.Observability.LogRequests = boolPtr(true)
	}
	if c.Evals.GateMaxDrop == 0 {
		c.Evals.GateMaxDrop = 2
	}
	for i := range c.Policies {
		applyPolicyDefaults(&c.Policies[i])
	}
	for i := range c.Budgets {
		b := &c.Budgets[i]
		if b.Timezone == "" {
			b.Timezone = "UTC"
		}
		if b.Period == "" {
			b.Period = "monthly"
		}
	}
}

func applyPolicyDefaults(p *Policy) {
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Objective == "" {
		p.Objective = "min_cost"
	}
	if p.Difficulty.Estimator == "" {
		p.Difficulty.Estimator = "rules"
	}
	if len(p.Difficulty.Thresholds) == 0 {
		p.Difficulty.Thresholds = []float64{0.2, 0.45, 0.7}
	}
	if p.Cascade.MaxEscalations == 0 {
		p.Cascade.MaxEscalations = 2
	}
	if len(p.Cascade.Validators) == 0 {
		p.Cascade.Validators = []string{"json_schema", "non_empty", "not_truncated", "no_refusal"}
	}
	if p.ExpectedOutput == 0 {
		p.ExpectedOutput = 256
	}
	if p.Weights == (Weights{}) {
		p.Weights = Weights{Cost: 1, Latency: 1, Quality: 1}
	}
}

// BuiltinPolicies are the default policies used when the config defines none of these
// names [CFG-07].
func BuiltinPolicies() []Policy {
	ps := []Policy{
		{Name: "default", Objective: "min_cost", Description: "Cheapest deployment meeting the difficulty tier"},
		{Name: "auto-fast", Objective: "min_latency", Description: "Fastest deployment within max cost"},
		{Name: "auto-cheap", Objective: "min_cost", Description: "Cheapest deployment, difficulty tier lowered by one",
			Difficulty: DifficultyCfg{Thresholds: []float64{0.35, 0.65, 0.9}}},
	}
	for i := range ps {
		applyPolicyDefaults(&ps[i])
	}
	return ps
}
