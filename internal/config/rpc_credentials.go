package config

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// RPCPeerTokens decodes credentials without exposing scalar values in YAML
// type errors. Runtime authentication still consumes map[string]string.
type RPCPeerTokens map[string]string

func (tokens *RPCPeerTokens) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return errors.New("peer_tokens must be a mapping of service identities to token strings")
	}
	decoded := make(RPCPeerTokens, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if value.Kind == yaml.AliasNode {
			value = value.Alias
		}
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return errors.New("peer_tokens identities must be strings")
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return errors.New("peer_tokens values must be strings")
		}
		if _, duplicate := decoded[key.Value]; duplicate {
			return errors.New("peer_tokens repeats a service identity")
		}
		decoded[key.Value] = value.Value
	}
	*tokens = decoded
	return nil
}
