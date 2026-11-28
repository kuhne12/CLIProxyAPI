package bpsconfig

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	DefaultCooldownSeconds   = 60
	DefaultCooldownThreshold = 3
	MaxCooldownSeconds       = 86400
	MaxCooldownThreshold     = 100
)

type RateLimitCooldown struct {
	Enabled   bool
	Seconds   int
	Threshold int
}

func ParseRateLimitCooldown(raw []byte) (RateLimitCooldown, error) {
	var node yaml.Node
	if errParse := yaml.Unmarshal(raw, &node); errParse != nil {
		return RateLimitCooldown{}, errParse
	}
	var policy RateLimitCooldown
	if len(node.Content) != 0 {
		if errDecode := node.Content[0].Decode(&policy); errDecode != nil {
			return RateLimitCooldown{}, errDecode
		}
	}
	return policy, nil
}

func (c *RateLimitCooldown) UnmarshalYAML(node *yaml.Node) error {
	*c = RateLimitCooldown{Seconds: DefaultCooldownSeconds, Threshold: DefaultCooldownThreshold}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("BPS settings must be an object")
	}
	seen := make(map[string]bool, 3)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "rate-limit-cooldown-enabled", "rate-limit-cooldown-seconds", "rate-limit-cooldown-threshold":
		default:
			continue
		}
		if seen[key] {
			return fmt.Errorf("duplicate BPS setting %s", key)
		}
		seen[key] = true
		if key == "rate-limit-cooldown-enabled" {
			if value.Tag != "!!bool" || value.Decode(&c.Enabled) != nil {
				return fmt.Errorf("%s must be a boolean", key)
			}
			continue
		}
		target, limit := &c.Seconds, MaxCooldownSeconds
		if key == "rate-limit-cooldown-threshold" {
			target, limit = &c.Threshold, MaxCooldownThreshold
		}
		if value.Tag != "!!int" || value.Decode(target) != nil || *target < 1 || *target > limit {
			return fmt.Errorf("%s must be an integer between 1 and %d", key, limit)
		}
	}
	return nil
}
