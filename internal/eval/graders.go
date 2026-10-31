// Package eval runs evaluation suites and grades responses [EVL-*].
package eval

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Answer is the part of a model response graders look at.
type Answer struct {
	Text      string
	ToolNames []string
	ToolArgs  []string
}

// GradeSimple scores an answer in [0,1] with the deterministic graders [EVL-03]:
// exact, contains (all ";"-separated parts, case-insensitive), regex, json_schema,
// tool_call and non_empty. It returns ok=false for graders it does not implement.
func GradeSimple(grader, reference string, schema json.RawMessage, a Answer) (float64, bool) {
	switch grader {
	case "exact":
		return b2f(normalize(a.Text) == normalize(reference)), true
	case "contains":
		text := strings.ToLower(a.Text)
		for _, part := range strings.Split(reference, ";") {
			if p := strings.ToLower(strings.TrimSpace(part)); p != "" && !strings.Contains(text, p) {
				return 0, true
			}
		}
		return 1, true
	case "regex":
		re, err := regexp.Compile(reference)
		if err != nil {
			return 0, true
		}
		return b2f(re.MatchString(a.Text)), true
	case "json_schema":
		return b2f(ValidJSONSchema(schema, a.Text)), true
	case "tool_call":
		for _, n := range a.ToolNames {
			if n == reference {
				return 1, true
			}
		}
		return 0, true
	case "non_empty":
		return b2f(strings.TrimSpace(a.Text) != "" || len(a.ToolNames) > 0), true
	}
	return 0, false
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.Trim(s, " .!\n\t\"'"))), " ")
}

var schemaCache sync.Map // string(schema) -> *jsonschema.Schema

// ValidJSONSchema reports whether text is JSON that validates against schema. With an
// empty schema it only checks that text is JSON.
func ValidJSONSchema(schema json.RawMessage, text string) bool {
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(strings.TrimSpace(text)))
	if err != nil {
		return false
	}
	if len(schema) == 0 {
		return true
	}
	sch, err := compileSchema(schema)
	if err != nil {
		return false
	}
	return sch.Validate(v) == nil
}

func compileSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
	if s, ok := schemaCache.Load(string(schema)); ok {
		return s.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, err
	}
	schemaCache.Store(string(schema), sch)
	return sch, nil
}
