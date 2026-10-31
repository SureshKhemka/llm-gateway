package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/guard"
	"github.com/SureshKhemka/llm-gateway/internal/netguard"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// resolveAlias maps a requested model through configured aliases (for example every
// "claude-*" name to "auto", so Claude Code works unchanged) [API-07].
func resolveAlias(s *Snapshot, model string) string {
	for _, a := range s.Config.ModelAliases {
		if ok, _ := path.Match(a.Match, model); ok || a.Match == model {
			return a.Model
		}
	}
	return model
}

func minCap(vals ...int) int {
	out := 0
	for _, v := range vals {
		if v > 0 && (out == 0 || v < out) {
			out = v
		}
	}
	return out
}

// guardRequest applies size, message-count and output caps per key, tenant and globally
// [GRD-01], validates the sensitivity label [GRD-02] and runs the pre-request webhook
// [GRD-05].
func (e *Engine) guardRequest(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest) *api.Error {
	g := s.Config.Guardrails
	var tc, kc config.Caps
	if rc.Principal.Tenant != nil {
		tc = rc.Principal.Tenant.Caps
	}
	kc = rc.Principal.Scopes.Caps
	if c := minCap(g.Caps.MaxRequestBytes, tc.MaxRequestBytes, kc.MaxRequestBytes); c > 0 && rc.BodyBytes > c {
		e := api.NewError(api.CodeInvalidRequest, "request body of %d bytes exceeds the %d-byte cap", rc.BodyBytes, c)
		e.Status = http.StatusRequestEntityTooLarge
		return e
	}
	if c := minCap(g.Caps.MaxMessages, tc.MaxMessages, kc.MaxMessages); c > 0 && len(req.Messages) > c {
		return api.NewError(api.CodeInvalidRequest, "%d messages exceed the cap of %d", len(req.Messages), c)
	}
	if c := minCap(g.Caps.MaxOutputTokens, tc.MaxOutputTokens, kc.MaxOutputTokens); c > 0 {
		if mo := req.MaxOutput(0); mo == 0 || mo > c {
			req.MaxTokens, req.MaxCompletionTokens = &c, nil
		}
	}
	if sv := rc.Hints.Sensitivity; sv != "" {
		if _, ok := g.Sensitivity[sv]; !ok {
			return api.NewError(api.CodeInvalidRequest, "unknown sensitivity %q", sv)
		}
	}
	if g.PreWebhook != "" {
		if err := e.callWebhook(ctx, s, g.PreWebhook, map[string]any{"stage": "pre", "request_id": rc.ID, "tenant": rc.Principal.TenantID(),
			"model": req.Model, "messages": redactedMessages(req)}); err != nil {
			return err
		}
	}
	return nil
}

func redactedMessages(req *api.ChatRequest) []map[string]string {
	out := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		t, _ := guard.Redact(m.Content.PlainText())
		out = append(out, map[string]string{"role": m.Role, "content": t})
	}
	return out
}

// callWebhook runs an operator-defined check. A response {"allow": false} blocks; a
// timeout or error blocks unless guardrails.fail_open [GRD-05].
func (e *Engine) callWebhook(ctx context.Context, s *Snapshot, url string, payload any) *api.Error {
	g := s.Config.Guardrails
	b, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, g.WebhookTimeout.D())
	defer cancel()
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	hr.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: netguard.Transport(g.WebhookAllow)}
	resp, err := client.Do(hr)
	if err != nil {
		if g.FailOpen {
			return nil
		}
		return api.NewError(api.CodePolicyBlocked, "guardrail webhook unavailable")
	}
	defer resp.Body.Close()
	var verdict struct {
		Allow  *bool  `json:"allow"`
		Reason string `json:"reason"`
	}
	if resp.StatusCode >= 300 || json.NewDecoder(resp.Body).Decode(&verdict) != nil || verdict.Allow == nil {
		if g.FailOpen {
			return nil
		}
		return api.NewError(api.CodePolicyBlocked, "guardrail webhook gave no verdict")
	}
	if !*verdict.Allow {
		return api.NewError(api.CodePolicyBlocked, "blocked by guardrail: %s", verdict.Reason)
	}
	return nil
}

// applyPII scans the prompt for PII and secrets; for deployments that are not local it
// logs, redacts or blocks per the tenant's mode [GRD-04].
func (e *Engine) applyPII(s *Snapshot, rc *RequestContext, req *api.ChatRequest, p *Plan, meta *Meta) *api.Error {
	mode := s.Config.Guardrails.PII
	if t := rc.Principal.Tenant; t != nil && t.PII != "" {
		mode = t.PII
	}
	if mode == "off" || p == nil {
		return nil
	}
	var findings []guard.Finding
	for _, m := range req.Messages {
		findings = append(findings, guard.Detect(m.Content.PlainText())...)
	}
	if len(findings) == 0 {
		return nil
	}
	meta.PII = guard.Types(findings)
	switch mode {
	case "redact":
		red := req.Clone()
		for i := range red.Messages {
			c := &red.Messages[i].Content
			if c.Parts == nil {
				c.Text, _ = guard.Redact(c.Text)
			}
			for j := range c.Parts {
				if c.Parts[j].Type == "text" {
					c.Parts[j].Text, _ = guard.Redact(c.Parts[j].Text)
				}
			}
		}
		red.Anthropic = nil // force re-translation from the redacted canonical form
		p.Redacted = red
	case "block":
		var keep []*catalog.Deployment
		for _, d := range p.Attempts {
			if d.IsLocal() {
				keep = append(keep, d)
			} else if p.Decision != nil {
				p.Decision.Exclusions = append(p.Decision.Exclusions, Exclusion{Deployment: d.ID, Reasons: []string{"pii_blocked:" + joinTypes(meta.PII)}})
			}
		}
		if len(keep) == 0 {
			return api.NewError(api.CodePolicyBlocked, "the prompt contains %s and no local deployment can serve it", joinTypes(meta.PII))
		}
		p.Attempts = keep
	}
	return nil
}

func joinTypes(ts []string) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}

// requestFor returns the request to send to d: the redacted copy for non-local
// deployments when PII redaction applies, with max_tokens clamped to what d can produce.
func requestFor(p *Plan, d *catalog.Deployment, req *api.ChatRequest) *api.ChatRequest {
	out := req
	if p != nil && p.Redacted != nil && !d.IsLocal() {
		out = p.Redacted
	}
	if mo := out.MaxOutput(0); d.MaxOutputTokens > 0 && mo > d.MaxOutputTokens {
		c := *out
		cap := d.MaxOutputTokens
		c.MaxTokens, c.MaxCompletionTokens = &cap, nil
		out = &c
	}
	return out
}

// logBody stores the redacted, encrypted request and response for tenants that opted
// in, with an expiry [GRD-03, NFR-12].
func (e *Engine) logBody(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest, resp *api.ChatResponse) {
	t := rc.Principal.Tenant
	if t == nil || !t.BodyLogging.Enabled || e.sealer(s) == nil {
		return
	}
	text, _ := responseText(resp)
	red, _ := guard.Redact(text)
	plain, _ := json.Marshal(map[string]any{"request": redactedMessages(req), "response": red, "model": resp.Model})
	ret := t.BodyLogging.Retention.D()
	if ret == 0 {
		ret = 30 * 24 * time.Hour
	}
	body, _ := json.Marshal(map[string]any{"expires_at": time.Now().Add(ret).UTC(), "sealed": e.sealer(s).Seal(plain)})
	_ = e.Store.PutDoc(ctx, store.Doc{Kind: "body", ID: rc.ID, Tenant: t.ID, Body: body})
}

func (e *Engine) sealer(s *Snapshot) *guard.Sealer {
	key, err := s.Config.Storage.BodyKey.Resolve()
	if err != nil || key == "" {
		return nil
	}
	sl, err := guard.NewSealer(key)
	if err != nil {
		return nil
	}
	return sl
}

// OpenBody decrypts a stored body (admin use, tests).
func (e *Engine) OpenBody(ctx context.Context, id string) ([]byte, error) {
	d, err := e.Store.GetDoc(ctx, "body", id)
	if err != nil {
		return nil, err
	}
	var b struct {
		Sealed []byte `json:"sealed"`
	}
	if err := json.Unmarshal(d.Body, &b); err != nil {
		return nil, err
	}
	return e.sealer(e.Snapshot()).Open(b.Sealed)
}

// PurgeBodies deletes stored bodies past their retention.
func (e *Engine) PurgeBodies(ctx context.Context) int {
	docs, err := e.Store.ListDocs(ctx, "body", "")
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range docs {
		var b struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if json.Unmarshal(d.Body, &b) == nil && time.Now().After(b.ExpiresAt) {
			_ = e.Store.DeleteDoc(ctx, "body", d.ID)
			n++
		}
	}
	return n
}
