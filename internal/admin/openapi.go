package admin

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var pathParam = regexp.MustCompile(`\{([a-z_]+)(\.\.\.)?\}`)

// openapi serves an OpenAPI 3.1 document generated from the route table, so the
// published contract cannot drift from the handlers [ADM-01].
func (x *API) openapi(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, x.OpenAPI())
}

// OpenAPI returns the document (also committed as docs/api/openapi.json).
func (x *API) OpenAPI() map[string]any {
	paths := map[string]map[string]any{}
	for _, rt := range x.Routes() {
		p := paths[rt.Path]
		if p == nil {
			p = map[string]any{}
			paths[rt.Path] = p
		}
		var params []any
		for _, m := range pathParam.FindAllStringSubmatch(rt.Path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
		roles := []string{}
		for role, perms := range RolePerms {
			for _, pm := range perms {
				if pm == rt.Perm || rt.Perm == "" {
					roles = append(roles, role)
					break
				}
			}
		}
		sort.Strings(roles)
		op := map[string]any{
			"summary":      rt.Summary,
			"operationId":  strings.ToLower(rt.Method) + strings.NewReplacer("/admin/v1/", "_", "/", "_", "{", "", "}", "").Replace(rt.Path),
			"security":     []any{map[string]any{"adminKey": []string{}}},
			"x-permission": rt.Perm,
			"x-roles":      roles,
			"responses": map[string]any{
				"200": map[string]any{"description": "OK"},
				"401": map[string]any{"description": "unauthorized"},
				"403": map[string]any{"description": "forbidden (role lacks permission)"},
			},
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if rt.Method == "POST" || rt.Method == "PUT" {
			op["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]string{"type": "object"}}}}
		}
		p[strings.ToLower(rt.Method)] = op
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "LLM Gateway Admin API", "version": "v1"},
		"paths":   paths,
		"components": map[string]any{"securitySchemes": map[string]any{
			"adminKey": map[string]any{"type": "http", "scheme": "bearer", "description": "Admin credential (separate from virtual API keys)"},
		}},
	}
	return doc
}
