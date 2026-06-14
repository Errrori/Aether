package webhook

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/aether-mq/aether/internal/store"
)

var placeholderRE = regexp.MustCompile(`\{([^}]+)\}`)

// ResolveChannel parses the template and extracts values from the JSON payload
// to produce a channel name.
//
// Template syntax: {path.to.field} where path.to.field is a dot-separated
// navigation into the JSON payload. All placeholders must resolve to scalar
// values (string, number, boolean).
func ResolveChannel(template string, payload []byte) (string, error) {
	matches := placeholderRE.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		// No placeholders, template is the literal channel name.
		if err := store.ValidateChannelName(template); err != nil {
			return "", fmt.Errorf("resolve channel: %w", err)
		}
		return template, nil
	}

	var parsed any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("resolve channel: invalid json payload: %w", err)
	}

	result := template
	for _, m := range matches {
		full := m[0]
		path := m[1]

		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("resolve channel: empty placeholder in template %q", template)
		}

		segments := strings.Split(path, ".")
		val, err := navigateJSON(parsed, segments)
		if err != nil {
			return "", fmt.Errorf("resolve channel: placeholder %q: %w", full, err)
		}

		str, ok := scalarToString(val)
		if !ok {
			return "", fmt.Errorf("resolve channel: placeholder %q resolves to non-scalar value", full)
		}
		result = strings.Replace(result, full, str, 1)
	}

	if err := store.ValidateChannelName(result); err != nil {
		return "", fmt.Errorf("resolve channel: %w", err)
	}
	return result, nil
}

func navigateJSON(v any, segments []string) (any, error) {
	cur := v
	for i, seg := range segments {
		if seg == "" {
			return nil, fmt.Errorf("empty path segment at position %d", i)
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot index into non-object at %q", strings.Join(segments[:i], "."))
		}
		var exists bool
		cur, exists = m[seg]
		if !exists {
			return nil, fmt.Errorf("field %q not found", strings.Join(segments[:i+1], "."))
		}
	}
	return cur, nil
}

func scalarToString(v any) (string, bool) {
	switch val := v.(type) {
	case string:
		return val, true
	case float64:
		return fmt.Sprintf("%v", val), true
	case bool:
		return fmt.Sprintf("%v", val), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}
