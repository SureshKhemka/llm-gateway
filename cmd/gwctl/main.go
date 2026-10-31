// Command gwctl is the admin CLI: keys, policies, budgets, providers, deployments,
// config apply and eval runs, over the same admin API and permissions as the console
// [ADM-04, ADM-05].
//
//	export GWCTL_URL=http://localhost:8080 GWCTL_KEY=$ADMIN_KEY
//	gwctl keys create --tenant acme --project web --name ci
//	gwctl policies apply -f policy.yaml && gwctl policies activate default --version 3 --eval-run run_x
//	gwctl evals run -f run.yaml --wait
//	gwctl api GET /admin/v1/health
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var version = "dev"

const usage = `gwctl — LLM gateway admin CLI (GWCTL_URL, GWCTL_KEY)

  whoami | health | version
  tenants list | create -f FILE | add-project TENANT -f FILE
  keys list [--tenant T] | create --tenant T --project P --name N [-f scopes.yaml] | rotate ID | revoke ID [--reason R]
  policies list | apply -f FILE | activate NAME --version N [--eval-run ID | --override-reason R] | rollback NAME [--version N] | diff NAME
  budgets list | apply -f FILE | grant ID --extra-usd X --expires RFC3339 --reason R
  providers list | apply -f FILE | delete ID
  deployments list | apply -f FILE | delete ID
  config show | diff | reload
  evals suites | upload -f FILE | run -f FILE [--wait] | list | status ID | recommend ID --policy P
  costs [--group-by tenant,model] [--from RFC3339] [--format csv]
  cache invalidate [-f FILE]
  api METHOD PATH [-f FILE]          raw call (escape hatch)
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gwctl:", err)
		os.Exit(1)
	}
}

type client struct {
	base, key string
	out       io.Writer
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	if args[0] == "version" {
		fmt.Fprintln(out, version)
		return nil
	}
	c := &client{base: strings.TrimRight(envOr("GWCTL_URL", "http://localhost:8080"), "/"), key: os.Getenv("GWCTL_KEY"), out: out}
	res, rest := args[0], args[1:]
	verb := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		verb, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("gwctl", flag.ContinueOnError)
	file := fs.String("f", "", "YAML or JSON body file ('-' for stdin)")
	tenant := fs.String("tenant", "", "tenant")
	project := fs.String("project", "", "project")
	name := fs.String("name", "", "name")
	ver := fs.Int("version", 0, "policy version")
	evalRun := fs.String("eval-run", "", "eval run that passed the gate")
	override := fs.String("override-reason", "", "activate without a passing eval (audited)")
	reason := fs.String("reason", "", "reason (audited)")
	extra := fs.Float64("extra-usd", 0, "grant amount")
	expires := fs.String("expires", "", "grant expiry (RFC3339)")
	wait := fs.Bool("wait", false, "wait for an eval run to finish")
	policy := fs.String("policy", "", "policy")
	groupBy := fs.String("group-by", "", "cost grouping")
	from := fs.String("from", "", "from (RFC3339)")
	format := fs.String("format", "", "json or csv")
	// Positional arguments may come before or after flags.
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos, rest = append(pos, rest[0]), rest[1:]
		}
	}
	arg := func(i int, what string) (string, error) {
		if i >= len(pos) {
			return "", fmt.Errorf("%s %s: missing %s", res, verb, what)
		}
		return pos[i], nil
	}
	body := func() (any, error) { return readBody(*file) }
	switch res + " " + verb {
	case "whoami ", "health ":
		return c.do("GET", "/admin/v1/"+res, nil)
	case "tenants list":
		return c.do("GET", "/admin/v1/tenants", nil)
	case "tenants create":
		return c.withBody("POST", "/admin/v1/tenants", body)
	case "tenants add-project":
		t, err := arg(0, "TENANT")
		if err != nil {
			return err
		}
		return c.withBody("POST", "/admin/v1/tenants/"+url.PathEscape(t)+"/projects", body)
	case "keys list":
		return c.do("GET", "/admin/v1/keys"+query("tenant", *tenant), nil)
	case "keys create":
		b := map[string]any{"tenant": *tenant, "project": *project, "name": *name}
		if *file != "" {
			scopes, err := body()
			if err != nil {
				return err
			}
			b["scopes"] = scopes
		}
		return c.do("POST", "/admin/v1/keys", b)
	case "keys rotate", "keys revoke":
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		return c.do("POST", "/admin/v1/keys/"+url.PathEscape(id)+"/"+verb, map[string]any{"reason": *reason})
	case "policies list":
		return c.do("GET", "/admin/v1/policies", nil)
	case "policies apply":
		return c.withBody("POST", "/admin/v1/policies", body)
	case "policies activate":
		n, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		return c.do("POST", "/admin/v1/policies/"+url.PathEscape(n)+"/activate",
			map[string]any{"version": *ver, "eval_run": *evalRun, "override_reason": *override})
	case "policies rollback":
		n, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		return c.do("POST", "/admin/v1/policies/"+url.PathEscape(n)+"/rollback", map[string]any{"version": *ver, "reason": *reason})
	case "policies diff":
		n, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		return c.do("GET", "/admin/v1/policies/"+url.PathEscape(n)+"/diff", nil)
	case "budgets list":
		return c.do("GET", "/admin/v1/budgets", nil)
	case "budgets apply":
		return c.withBody("POST", "/admin/v1/budgets", body)
	case "budgets grant":
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		exp, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return fmt.Errorf("--expires: %w", err)
		}
		return c.do("POST", "/admin/v1/budgets/"+url.PathEscape(id)+"/grants", map[string]any{"extra_usd": *extra, "expires_at": exp, "reason": *reason})
	case "providers list", "deployments list":
		return c.do("GET", "/admin/v1/"+res, nil)
	case "providers apply", "deployments apply":
		return c.withBody("POST", "/admin/v1/"+res, body)
	case "providers delete", "deployments delete":
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		return c.do("DELETE", "/admin/v1/"+res+"/"+id, nil)
	case "config show":
		return c.do("GET", "/admin/v1/config", nil)
	case "config diff", "config reload":
		return c.do("POST", "/admin/v1/config/"+verb, nil)
	case "evals suites":
		return c.do("GET", "/admin/v1/evals/suites", nil)
	case "evals upload":
		return c.withBody("POST", "/admin/v1/evals/suites", body)
	case "evals run":
		b, err := body()
		if err != nil {
			return err
		}
		if m, ok := b.(map[string]any); ok && *wait {
			m["wait"] = true
		}
		return c.do("POST", "/admin/v1/evals/runs", b)
	case "evals list":
		return c.do("GET", "/admin/v1/evals/runs", nil)
	case "evals status":
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		return c.do("GET", "/admin/v1/evals/runs/"+url.PathEscape(id), nil)
	case "evals recommend":
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		return c.do("POST", "/admin/v1/evals/runs/"+url.PathEscape(id)+"/recommend", map[string]any{"policy": *policy})
	case "costs ":
		return c.do("GET", "/admin/v1/costs"+query("group_by", *groupBy, "from", *from, "format", *format), nil)
	case "cache invalidate":
		b := any(map[string]any{})
		if *file != "" {
			var err error
			if b, err = body(); err != nil {
				return err
			}
		}
		return c.do("POST", "/admin/v1/cache/invalidate", b)
	case "api " + verb:
		p, err := arg(0, "PATH")
		if err != nil {
			return err
		}
		var b any
		if *file != "" {
			if b, err = body(); err != nil {
				return err
			}
		}
		return c.do(strings.ToUpper(verb), p, b)
	}
	return fmt.Errorf("unknown command %q (gwctl help)", strings.TrimSpace(res+" "+verb))
}

func (c *client) withBody(method, path string, body func() (any, error)) error {
	b, err := body()
	if err != nil {
		return err
	}
	return c.do(method, path, b)
}

func (c *client) do(method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var pretty bytes.Buffer
	if json.Indent(&pretty, b, "", "  ") == nil {
		b = append(pretty.Bytes(), '\n')
	}
	_, _ = c.out.Write(b)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return nil
}

// readBody reads YAML or JSON (JSON is YAML) and returns it as JSON-compatible values.
func readBody(path string) (any, error) {
	if path == "" {
		return nil, fmt.Errorf("-f FILE is required")
	}
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func query(kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
