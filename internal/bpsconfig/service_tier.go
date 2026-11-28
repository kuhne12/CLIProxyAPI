package bpsconfig

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceTier is shared by the plugin's configuration validation and executor.
// Selected credential IDs are exact matches, never substrings or account claims.
type ServiceTier struct {
	ForceUltraFast bool     `yaml:"force-ultrafast"`
	AuthIDs        []string `yaml:"force-ultrafast-auth-ids"`
}

// UnmarshalYAML rejects coercions and dropped null list entries. Unrelated
// plugin settings are intentionally ignored; null settings mean disabled.
func (c *ServiceTier) UnmarshalYAML(node *yaml.Node) error {
	*c = ServiceTier{}
	if node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("BPS settings must be an object")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if key != "force-ultrafast" && key != "force-ultrafast-auth-ids" {
			continue
		}
		if seen[key] {
			return fmt.Errorf("duplicate BPS ultra fast setting")
		}
		seen[key] = true
		if value.Tag == "!!null" {
			continue
		}
		switch key {
		case "force-ultrafast":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return fmt.Errorf("force-ultrafast must be a boolean")
			}
			if err := value.Decode(&c.ForceUltraFast); err != nil {
				return err
			}
		case "force-ultrafast-auth-ids":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("force-ultrafast-auth-ids must be an array of strings")
			}
			for _, item := range value.Content {
				if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
					return fmt.Errorf("force-ultrafast-auth-ids must contain only strings")
				}
				c.AuthIDs = append(c.AuthIDs, item.Value)
			}
		}
	}
	return c.Validate()
}

func (c ServiceTier) Validate() error {
	if len(c.AuthIDs) > 256 {
		return fmt.Errorf("BPS ultra fast supports at most 256 credential IDs")
	}
	seen := make(map[string]bool, len(c.AuthIDs))
	for _, id := range c.AuthIDs {
		if id == "" || strings.TrimSpace(id) != id || len(id) > 1024 || strings.ContainsAny(id, "\r\n\x00") || seen[id] {
			return fmt.Errorf("BPS ultra fast credential IDs must be unique, nonempty exact IDs")
		}
		seen[id] = true
	}
	return nil
}

func (c ServiceTier) Forced(ids ...string) bool {
	if c.ForceUltraFast {
		return true
	}
	for _, selected := range c.AuthIDs {
		for _, id := range ids {
			if id != "" && selected == id {
				return true
			}
		}
	}
	return false
}
