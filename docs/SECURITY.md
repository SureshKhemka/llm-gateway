# Security: threat model and ASVS Level 2 checklist

Scope: the gateway process, its admin API and console, and its stores (Redis,
Postgres). Requirement NFR-13 asks for OWASP ASVS 4.0.3 Level 2 on the admin API and
console. This document records the threat model, how each relevant ASVS chapter is met,
the automated test that checks it where there is one, and the remaining gaps.

## 1. Assets

| Asset | Where | Why it matters |
|---|---|---|
| Provider API keys | environment / secret files, referenced as `env:`/`file:` | spend on someone else's account |
| Virtual API keys | SHA-256 hashes in Postgres/config; plaintext shown once | spend against a tenant's budget |
| Admin keys | hashes in the snapshot; plaintext in the operator's secret store | full control of routing, budgets, keys |
| Prompts and responses | in flight; at rest only when a tenant opts in (encrypted) | customer data, PII |
| Cost records, audit log | Postgres | billing and accountability |
| Budgets and rate limits | Redis | overspend if bypassed |

## 2. Trust boundaries and threats (STRIDE)

```
 clients ──(virtual key)──▶ gateway ──(provider key)──▶ LM Studio / simulators / cloud
 operators ─(admin key)──▶ admin API + console
                            │  Redis (budgets, limits, cache)   Postgres (state, audit)
                            └─ webhooks (alerts, guardrails, graders) ─▶ operator URLs
```

| Threat | Example | Mitigation | Test |
|---|---|---|---|
| **S**poofing a tenant | stolen or guessed virtual key | 128+-bit random keys, hashed at rest, revocation propagates to every instance within ~1 s, expiry, IP allow-lists, rotation with overlap | `TestTEN02_KeyLifecycle` |
| Spoofing an admin | brute force of the admin API | random admin keys; failed authentication throttled per address (10/min → 60 s lockout, `Retry-After`) and logged without the credential | `TestNFR13_AdminAuthThrottle` |
| **T**ampering with cost history | edit or delete audit rows | `audit_log` is append-only via a trigger (also after restore); every admin write records actor, before, after, reason | `TestOBS06_AuditLogAppendOnly`, `TestNFR19_BackupAndRestore` |
| Tampering with config through the API | override a file-managed provider | file-defined IDs are read-only through the API (409); org-wide objects need `org_admin` | `TestADM01_AdminAPICoversCapabilitiesAndOpenAPI` |
| **R**epudiation | "I didn't raise that budget" | audit log with actor and role for every write; grants require a reason | `TestScenario16_TenantIsolation`, `TestOBS06_AuditLogAppendOnly` |
| **I**nformation disclosure: secrets | keys in config dumps, logs, errors | config holds references only (validation rejects literals); `GET /admin/v1/config` shows references; logs never contain prompts, responses or keys; repository secret scan | `TestPRV08_SecretsNeverReturned`, `TestNFR15_NoSecretsInRepository` |
| Information disclosure: prompts | body capture, cache across tenants | capture is opt-in per tenant, AES-256-GCM encrypted, separate retention; cache keys are tenant-scoped; PII redaction/blocking | `TestGRD04_DetectsIndianAndGenericPII`, M6 tests |
| Information disclosure: cross-tenant | tenant admin reads another tenant | tenant admins are scoped to their tenant on every endpoint | `TestTEN04_RolePermissions`, `TestScenario16_TenantIsolation` |
| **D**enial of service | floods, huge bodies, slow loris | rate limits (requests and tokens), concurrency caps, load shedding (`max_in_flight`), body and message caps, `ReadHeaderTimeout`, per-deployment queues | `TestRES09_LoadSheddingRejectsLowPriorityFirst` |
| Denial of wallet | runaway client spends the budget | hard budgets with reservations before the call (no overshoot under concurrency), per-request cost ceilings, alerts at soft thresholds | `TestScenario06_BudgetDegradationAndHardStop`, `TestBUD03_NoOvershootUnderConcurrency100Runs` |
| **E**levation of privilege | finops viewer writes a budget | one permission table for API, console and CLI; every route declares its permission (OpenAPI `x-permission`) | `TestTEN04_RolePermissions`, `TestADM04_CLI`, `TestADM01_AdminAPICoversCapabilitiesAndOpenAPI` |
| SSRF via provider or webhook URLs | `base_url: http://169.254.169.254/` | outbound dialer refuses loopback, private, link-local and metadata ranges unless allow-listed; resolves DNS itself (no rebinding) | `TestNFR14_WebhooksRefusePrivateAddressesUnlessAllowed` |
| XSS in the console | model output or tenant names rendered as HTML | every value interpolated into console markup goes through `esc()` (HTML-escapes `& < > " '`); strict CSP (`default-src 'self'`, no inline script or style) as a backstop; `frame-ancestors 'none'` | `TestADM02_ConsoleAndPlayground`; `scenarios/test_console_browser.py` stores a tenant ID containing an `onerror` payload and checks in Chromium that it renders as text and never runs |
| CSRF | a page makes the browser call the admin API | admin auth is a bearer header held in `sessionStorage`, never a cookie, so cross-site requests carry no credential | by construction |
| Supply chain | vulnerable or malicious dependency | govulncheck in the release gate, SBOMs, pinned toolchain, signed images (`scripts/supply-chain.sh`) | release gate |

## 3. ASVS 4.0.3 Level 2 — admin API and console

Status: **Met**, **Met (deployment)** = met when deployed as documented, **Gap** =
not met by the product alone. Chapters that don't apply (e.g. V12 file upload, beyond
eval JSONL, which is parsed as data) are omitted.

| ASVS | Requirement (summary) | Status | How |
|---|---|---|---|
| V1.1 | Secure development lifecycle, threat model | Met | this document; requirement IDs traced to tests (`docs/TRACEABILITY.md`) |
| V1.4 | Access control enforced server-side at a single point | Met | admin router checks the route's permission before every handler |
| V2.1 / V2.2 | Credential strength, anti-automation | Met | admin keys are random secrets (no passwords); failed attempts throttled and logged |
| V2.2.3 / V2.5 | Notifications, credential recovery | Met (deployment) | admin keys are issued and rotated through the operator's secret store |
| V2.8 / V2.2.7 | Multi-factor for admins | **Gap** | no built-in MFA/SSO. Deploy the console behind an identity-aware proxy (OIDC with MFA) on the separate admin listener |
| V3 | Session management | Met | no server-side sessions or cookies; the console keeps the key in `sessionStorage` (cleared on sign-out and tab close) |
| V4.1 / V4.2 | Least privilege, deny by default, IDOR | Met | four roles; tenant admins scoped to their tenant on every object lookup |
| V4.3 | Admin interfaces protected | Met (deployment) | `server.admin_listen` puts admin API, console and metrics on a separate listener |
| V5.1 / V5.2 | Input validation, sanitisation | Met | admin bodies decoded into typed structs with unknown fields rejected, config validation with paths, body/message caps |
| V5.3 | Output encoding, injection | Met | console HTML-escapes all data (`esc()`), checked in a real browser; Postgres access is parameterised (pgx); no shell or template injection paths |
| V6.2 | Approved cryptography | Met | SHA-256 key hashes (keys are high-entropy, so no slow KDF needed), AES-256-GCM for stored bodies, `crypto/rand` for keys |
| V6.4 | Secret management | Met | `env:`/`file:` references only; literals rejected by validation |
| V7.1 / V7.2 | Logging without sensitive data; security events logged | Met | structured logs without prompts/keys; auth failures, admin writes and breaker transitions logged/audited |
| V7.3 | Log integrity | Met | audit log append-only in the database |
| V8.1 / V8.3 | Data protection, no sensitive data in caches/URLs | Met | `Cache-Control: no-store` on admin responses; keys never in URLs |
| V9.1 | TLS for all connections | Met (deployment) | `server.tls_cert_file`/`tls_key_file` (TLS 1.2 minimum) or TLS at the ingress |
| V10.3 | Application integrity, signed releases | Met (deployment) | `scripts/supply-chain.sh` signs images with cosign when a key is configured |
| V13.1 / V13.2 | RESTful API security, schema | Met | OpenAPI 3.1 generated from the route table; methods enforced per route |
| V14.2 | Dependencies, no known vulnerabilities | Met | govulncheck: no reachable vulnerabilities at release |
| V14.4 | Security headers | Met | `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, CSP on admin API and console |
| V14.5 | HTTP request validation | Met | method-specific routing; unknown methods 405 |

### Remaining gaps and what to do about them

1. **MFA/SSO for administrators (V2.8):** put the admin listener behind an identity-aware
   proxy (OIDC with MFA). Built-in OIDC is not in 1.0.
2. **Signed images in CI:** the signing step exists but needs a release key and registry;
   no image has been published from this repository yet.
3. **Third-party penetration test:** not performed. The checklist above is a
   self-assessment.

## 4. Reporting

Report vulnerabilities privately to the repository owner rather than in public issues.
