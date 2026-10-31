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
func (d Duration) D() time.Duration          { return time.Duration(d) }

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
	Server        Server          `yaml:"server"`
	Storage       Storage         `yaml:"storage"`
	Providers     []Provider      `yaml:"providers"`
	Deployments   []Deployment    `yaml:"deployments"`
	VirtualModels []VirtualModel  `yaml:"virtual_models"`
	Policies      []Policy        `yaml:"policies"`
	Tenants       []Tenant        `yaml:"tenants"`
	Admins        []AdminCred     `yaml:"admins"`
	Budgets       []Budget        `yaml:"budgets"`
	Alerts        []AlertSink     `yaml:"alerts"`
	Cache         Cache           `yaml:"cache"`
	Guardrails    Guardrails      `yaml:"guardrails"`
	Resilience    Resilience      `yaml:"resilience"`
	Observability Observability   `yaml:"observability"`
	Defaults      RoutingDefaults `yaml:"defaults"`
	Evals         Evals           `yaml:"evals"`

	// Files lists the files this config was loaded from (for messages).
	Files []string `yaml:"-"`
	// origin maps "kind/id" to the file that defined it.
	origin map[string]string
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
	Listen        string   `yaml:"listen"`
	AdminListen   string   `yaml:"admin_listen"` // empty: admin API on the main listener
	DrainTimeout  Duration `yaml:"drain_timeout"`
	MaxBodyBytes  int64    `yaml:"max_body_bytes"`
	MaxInFlight   int      `yaml:"max_in_flight"` // load shedding threshold [RES-09]
	ReloadEvery   Duration `yaml:"reload_every"`  // config file poll interval
	TLSCertFile   string   `yaml:"tls_cert_file"`
	TLSKeyFile    string   `yaml:"tls_key_file"`
	InstanceID    string   `yaml:"instance_id"`
	TrustProxyIPs []string `yaml:"trust_proxy_ips"`
}

type Storage struct {
	RedisURL    SecretRef `yaml:"redis_url"`
	PostgresDSN SecretRef `yaml:"postgres_dsn"`
	// BudgetsFailOpen lets requests through when Redis is unreachable (default: fail closed) [NFR-08].
	BudgetsFailOpen bool   `yaml:"budgets_fail_open"`
	WALDir          string `yaml:"wal_dir"`
}

type Provider struct {
	ID       string            `yaml:"id"`
	Type     string            `yaml:"type"` // openai | anthropic
	BaseURL  string            `yaml:"base_url"`
	APIKey   SecretRef         `yaml:"api_key"`
	Headers  map[string]string `yaml:"headers"`
	Discover bool              `yaml:"discover"` // list models via /v1/models [PRV-06]
	// AutoApprove makes discovered models usable immediately with default metadata.
	AutoApprove bool `yaml:"auto_approve"`
	// AllowPrivateNetwork permits base URLs in private ranges (SSRF guard) [NFR-14].
	AllowPrivateNetwork *bool    `yaml:"allow_private_network"`
	Timeouts            Timeouts `yaml:"timeouts"`
	Enabled             *bool    `yaml:"enabled"`
	// ProbeModel is used for active health probes; empty uses the model list.
	ProbeEvery Duration `yaml:"probe_every"`
	// DefaultDiscovered holds metadata applied to auto-approved discovered models.
	DefaultDiscovered *Deployment `yaml:"discovered_defaults"`
}

type Timeouts struct {
	Connect Duration `yaml:"connect"`
	TTFT    Duration `yaml:"ttft"`
	Idle    Duration `yaml:"idle"`
	Total   Duration `yaml:"total"`
}

type PriceCard struct {
	EffectiveFrom time.Time `yaml:"effective_from"`
	Input         float64   `yaml:"input"` // USD per 1M tokens
	CachedInput   *float64  `yaml:"cached_input"`
	Output        float64   `yaml:"output"`
	Reasoning     *float64  `yaml:"reasoning"`
}

type Latency struct {
	TTFTMs       float64 `yaml:"ttft_ms"`
	TokensPerSec float64 `yaml:"tokens_per_sec"`
}

type Deployment struct {
	ID              string      `yaml:"id"`
	Provider        string      `yaml:"provider"`
	Model           string      `yaml:"model"` // upstream model name
	Capabilities    []string    `yaml:"capabilities"`
	ContextWindow   int         `yaml:"context_window"`
	MaxOutputTokens int         `yaml:"max_output_tokens"`
	Tier            int         `yaml:"tier"`
	Prices          []PriceCard `yaml:"prices"`
	Latency         Latency     `yaml:"latency"`
	Concurrency     int         `yaml:"concurrency"`
	QueueWait       Duration    `yaml:"queue_wait"`
	Residency       []string    `yaml:"residency"`
	Enabled         *bool       `yaml:"enabled"`
	Pool            string      `yaml:"pool"`
	Weight          int         `yaml:"weight"`
	Timeouts        Timeouts    `yaml:"timeouts"`
	// Warm reports whether a local model is loaded; nil means unknown (treated warm).
	Warm *bool `yaml:"warm"`
}

type VirtualModel struct {
	Name        string   `yaml:"name"`
	Policy      string   `yaml:"policy"`
	Deployments []string `yaml:"deployments"` // empty: every chat deployment
	Description string   `yaml:"description"`
}

type DifficultyCfg struct {
	Estimator  string    `yaml:"estimator"`  // rules | similarity | classifier
	Thresholds []float64 `yaml:"thresholds"` // score cut-offs for tiers 1..n
	Classifier string    `yaml:"classifier"` // deployment for classifier estimator
	Examples   string    `yaml:"examples"`   // JSONL file of labelled prompts (similarity)
	Embedder   string    `yaml:"embedder"`   // embeddings deployment (similarity)
}

type CascadeCfg struct {
	Enabled        bool     `yaml:"enabled"`
	MaxEscalations int      `yaml:"max_escalations"`
	Validators     []string `yaml:"validators"` // json_schema, non_empty, not_truncated, no_refusal, judge
	Judge          string   `yaml:"judge"`      // deployment ID
	JudgeMinScore  float64  `yaml:"judge_min_score"`
	BufferStreams  bool     `yaml:"buffer_streams"`
}

type DegradeStep struct {
	AtPct   float64 `yaml:"at_pct"`
	MaxTier int     `yaml:"max_tier"`
}

type Split struct {
	Policy string `yaml:"policy"`
	Weight int    `yaml:"weight"`
}

type Shadow struct {
	Policy     string  `yaml:"policy"`
	Deployment string  `yaml:"deployment"`
	SampleRate float64 `yaml:"sample_rate"`
}

type PolicyCache struct {
	SemanticThreshold *float64 `yaml:"semantic_threshold"`
	AllowTools        bool     `yaml:"allow_tools"`
	Disabled          bool     `yaml:"disabled"`
}

type Weights struct {
	Cost    float64 `yaml:"cost"`
	Latency float64 `yaml:"latency"`
	Quality float64 `yaml:"quality"`
}

type Policy struct {
	Name             string        `yaml:"name"`
	Version          int           `yaml:"version"`
	Objective        string        `yaml:"objective"` // min_cost | min_latency | max_quality | weighted
	Weights          Weights       `yaml:"weights"`
	Difficulty       DifficultyCfg `yaml:"difficulty"`
	MinTier          *int          `yaml:"min_tier"`
	MaxTier          *int          `yaml:"max_tier"`
	MaxCostUSD       *float64      `yaml:"max_cost_usd"`
	LatencyTargetMs  *float64      `yaml:"latency_target_ms"`
	AllowedProviders []string      `yaml:"allowed_providers"`
	DeniedProviders  []string      `yaml:"denied_providers"`
	Residency        []string      `yaml:"residency"`
	Cascade          CascadeCfg    `yaml:"cascade"`
	Degrade          []DegradeStep `yaml:"degrade"`
	Cache            PolicyCache   `yaml:"cache"`
	Split            []Split       `yaml:"split"`
	Shadow           *Shadow       `yaml:"shadow"`
	MaxAttempts      int           `yaml:"max_attempts"`
	ExpectedOutput   int           `yaml:"expected_output_tokens"`
	Baseline         string        `yaml:"baseline"`
	Description      string        `yaml:"description"`
}

type RateLimits struct {
	RPM        int `yaml:"rpm"`
	ITPM       int `yaml:"itpm"`
	OTPM       int `yaml:"otpm"`
	Concurrent int `yaml:"concurrent"`
}

type Caps struct {
	MaxRequestBytes int `yaml:"max_request_bytes"`
	MaxMessages     int `yaml:"max_messages"`
	MaxOutputTokens int `yaml:"max_output_tokens"`
}

type Scopes struct {
	Models           []string   `yaml:"models"` // glob patterns; empty = all
	Endpoints        []string   `yaml:"endpoints"`
	Providers        []string   `yaml:"providers"`
	MaxCostUSD       *float64   `yaml:"max_cost_usd"`
	RateLimits       RateLimits `yaml:"rate_limits"`
	Priority         int        `yaml:"priority"` // higher is more important [RES-09]
	IPAllow          []string   `yaml:"ip_allow"`
	DefaultTags      []string   `yaml:"default_tags"`
	Policy           string     `yaml:"policy"`
	Caps             Caps       `yaml:"caps"`
	Baseline         string     `yaml:"baseline"`
	AllowedResidency []string   `yaml:"allowed_residency"`
}

type Key struct {
	ID        string     `yaml:"id"`
	Name      string     `yaml:"name"`
	Secret    SecretRef  `yaml:"secret"` // bootstrap keys only; API-created keys are stored hashed
	ExpiresAt *time.Time `yaml:"expires_at"`
	Scopes    Scopes     `yaml:"scopes"`
}

type Project struct {
	ID       string     `yaml:"id"`
	Name     string     `yaml:"name"`
	Policy   string     `yaml:"policy"`
	Limits   RateLimits `yaml:"rate_limits"`
	Baseline string     `yaml:"baseline"`
	Keys     []Key      `yaml:"keys"`
}

type TenantCache struct {
	Shared bool `yaml:"shared"` // opt into the cross-tenant namespace [CCH-03]
}

type BodyLogging struct {
	Enabled   bool     `yaml:"enabled"`
	Retention Duration `yaml:"retention"`
}

type Tenant struct {
	ID            string      `yaml:"id"`
	Name          string      `yaml:"name"`
	Policy        string      `yaml:"policy"`
	Limits        RateLimits  `yaml:"rate_limits"`
	Caps          Caps        `yaml:"caps"`
	Baseline      string      `yaml:"baseline"`
	Cache         TenantCache `yaml:"cache"`
	BodyLogging   BodyLogging `yaml:"body_logging"`
	PII           string      `yaml:"pii"` // off | log | redact | block
	AllowedModels []string    `yaml:"allowed_models"`
	Projects      []Project   `yaml:"projects"`
}

type AdminCred struct {
	ID     string    `yaml:"id"`
	Role   string    `yaml:"role"` // org_admin | tenant_admin | finops_viewer | auditor
	Tenant string    `yaml:"tenant"`
	Secret SecretRef `yaml:"secret"`
}

type Budget struct {
	ID        string    `yaml:"id"`
	Scope     string    `yaml:"scope"` // tenant | project | key | end_user | eval | shadow
	ScopeID   string    `yaml:"scope_id"`
	Period    string    `yaml:"period"` // daily | weekly | monthly | custom
	Custom    Duration  `yaml:"custom_period"`
	Anchor    time.Time `yaml:"anchor"`
	Timezone  string    `yaml:"timezone"`
	HardUSD   float64   `yaml:"hard_usd"`
	SoftPct   []float64 `yaml:"soft_pct"`
	AlertSink []string  `yaml:"alerts"`
}

type AlertSink struct {
	ID      string    `yaml:"id"`
	Type    string    `yaml:"type"` // webhook | slack
	URL     SecretRef `yaml:"url"`
	Timeout Duration  `yaml:"timeout"`
}

type Cache struct {
	Enabled              bool     `yaml:"enabled"`
	TTL                  Duration `yaml:"ttl"`
	MaxEntries           int      `yaml:"max_entries"`
	MaxTemperature       float64  `yaml:"max_temperature"`
	Semantic             bool     `yaml:"semantic"`
	SemanticThreshold    float64  `yaml:"semantic_threshold"`
	Embedder             string   `yaml:"embedder"`
	PriceUSD             float64  `yaml:"price_usd"`
	JudgeHits            string   `yaml:"judge_hits"` // deployment to verify semantic hits [CCH-09]
	SampleRate           float64  `yaml:"sample_rate"`
	MaxSemanticPerBucket int      `yaml:"max_semantic_per_bucket"`
}

type Guardrails struct {
	Caps           Caps                `yaml:"caps"`
	PII            string              `yaml:"pii"`         // off | log | redact | block (default for tenants)
	Sensitivity    map[string][]string `yaml:"sensitivity"` // level -> allowed residency tags
	PreWebhook     string              `yaml:"pre_webhook"`
	PostWebhook    string              `yaml:"post_webhook"`
	WebhookTimeout Duration            `yaml:"webhook_timeout"`
	FailOpen       bool                `yaml:"fail_open"`
}

type Breaker struct {
	Window       Duration `yaml:"window"`
	MinRequests  int      `yaml:"min_requests"`
	ErrorRate    float64  `yaml:"error_rate"`
	LatencyP95Ms float64  `yaml:"latency_p95_ms"`
	CoolDown     Duration `yaml:"cool_down"`
}

type Resilience struct {
	MaxAttempts   int      `yaml:"max_attempts"`
	TotalDeadline Duration `yaml:"total_deadline"`
	BackoffBase   Duration `yaml:"backoff_base"`
	BackoffMax    Duration `yaml:"backoff_max"`
	Breaker       Breaker  `yaml:"breaker"`
	Timeouts      Timeouts `yaml:"timeouts"`
	MaxTimeouts   Timeouts `yaml:"max_timeouts"` // bounds for per-request overrides
	LatencyAlpha  float64  `yaml:"latency_alpha"`
}

type Observability struct {
	LogRequests  *bool                `yaml:"log_requests"`
	OTLPEndpoint string               `yaml:"otlp_endpoint"`
	OTLPHeaders  map[string]SecretRef `yaml:"otlp_headers"`
	Langfuse     *Exporter            `yaml:"langfuse"`
	LangSmith    *Exporter            `yaml:"langsmith"`
	TraceSample  float64              `yaml:"trace_sample"`
}

type Exporter struct {
	Endpoint string    `yaml:"endpoint"`
	Key      SecretRef `yaml:"key"`
	Secret   SecretRef `yaml:"secret"`
}

type RoutingDefaults struct {
	Policy          string  `yaml:"policy"`
	Baseline        string  `yaml:"baseline"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
	Currency        string  `yaml:"display_currency"`
	ExchangeRate    float64 `yaml:"exchange_rate"`
}

type Evals struct {
	Dir         string  `yaml:"dir"`
	GateMaxDrop float64 `yaml:"gate_max_drop"`
	Judge       string  `yaml:"judge"`
	Embedder    string  `yaml:"embedder"`
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
