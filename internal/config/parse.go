package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	return Parse(data, format)
}

// Parse rejects duplicate keys, unknown fields, nulls, and implicit scalar coercion.
func Parse(data []byte, format string) (Config, error) {
	switch strings.ToLower(strings.TrimPrefix(format, ".")) {
	case "json":
	case "yaml", "yml":
		dec := yaml.NewDecoder(bytes.NewReader(data))
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			return Config{}, fmt.Errorf("YAML: %w", err)
		}
		var extra yaml.Node
		if err := dec.Decode(&extra); err != io.EOF {
			return Config{}, fmt.Errorf("YAML must contain exactly one document")
		}
		if len(node.Content) != 1 {
			return Config{}, fmt.Errorf("empty YAML configuration")
		}
		value, err := yamlJSON(node.Content[0], "$")
		if err != nil {
			return Config{}, err
		}
		data, err = json.Marshal(value)
		if err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("unsupported configuration format %q", format)
	}
	if err := checkJSON(data); err != nil {
		return Config{}, err
	}
	config := Defaults()
	if err := decodeStrict(data, &config); err != nil {
		return Config{}, err
	}
	config.QuotaQuery.Manual = true
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("configuration must contain exactly one value")
	}
	return nil
}
func checkJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := scanJSON(dec, "$", true); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("JSON must contain exactly one value")
	}
	return nil
}
func scanJSON(dec *json.Decoder, path string, root bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return fmt.Errorf("%s: null is not allowed in persisted configuration", path)
	}
	delim, isDelim := tok.(json.Delim)
	if root && (!isDelim || delim != '{') {
		return fmt.Errorf("configuration root must be an object")
	}
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			if seen[key] {
				return fmt.Errorf("%s.%s: duplicate key", path, key)
			}
			seen[key] = true
			if err := scanJSON(dec, path+"."+key, false); err != nil {
				return err
			}
		}
	case '[':
		i := 0
		for dec.More() {
			if err := scanJSON(dec, fmt.Sprintf("%s[%d]", path, i), false); err != nil {
				return err
			}
			i++
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}
func yamlJSON(n *yaml.Node, path string) (any, error) {
	switch n.Kind {
	case yaml.MappingNode:
		result := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				return nil, fmt.Errorf("%s: YAML mapping keys must be strings", path)
			}
			if _, exists := result[k.Value]; exists {
				return nil, fmt.Errorf("%s.%s: duplicate key", path, k.Value)
			}
			v, err := yamlJSON(n.Content[i+1], path+"."+k.Value)
			if err != nil {
				return nil, err
			}
			result[k.Value] = v
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, len(n.Content))
		for i, child := range n.Content {
			v, err := yamlJSON(child, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			result[i] = v
		}
		return result, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!bool":
			v, err := strconv.ParseBool(n.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid boolean", path)
			}
			return v, nil
		case "!!int", "!!float":
			// YAML numeric syntax may be wider than JSON; its standard decoder
			// resolves integer bases. Preserve float typing so YAML 1.0 cannot
			// silently become an integer destination after JSON serialization.
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, err
			}
			if f, ok := v.(float64); ok {
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, fmt.Errorf("%s: number must be finite", path)
				}
				value := strconv.FormatFloat(f, 'g', -1, 64)
				if !strings.ContainsAny(value, ".eE") {
					value += ".0"
				}
				return json.Number(value), nil
			}
			if _, err := json.Marshal(v); err != nil {
				return nil, fmt.Errorf("%s: number must be finite", path)
			}
			return v, nil
		default:
			return nil, fmt.Errorf("%s: unsupported YAML scalar tag %s", path, n.Tag)
		}
	default:
		return nil, fmt.Errorf("%s: aliases and custom YAML nodes are not supported", path)
	}
}
