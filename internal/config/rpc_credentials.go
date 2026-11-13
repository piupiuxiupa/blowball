package config

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"

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

// expandRPCCredentialEnv shields token scalars while retaining the legacy text
// expansion of other fields, whose environment values may determine YAML types.
func expandRPCCredentialEnv(data []byte) (string, error) {
	// References must be masked before parsing: an unquoted ${VAR} may not
	// itself be valid YAML inside a flow mapping.
	references := make(map[string]string)
	var envPairs []string
	masked := envExpander.ReplaceAllStringFunc(string(data), func(reference string) string {
		value := expandEnv(reference)
		if marker, ok := references[value]; ok {
			return marker
		}
		marker := "rpc-env-reference-" + rand.Text()
		references[value] = marker
		envPairs = append(envPairs, marker, value)
		return marker
	})
	restoreEnv := strings.NewReplacer(envPairs...).Replace
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(masked), &document); err != nil {
		return "", err
	}
	if len(document.Content) == 0 {
		return expandEnv(string(data)), nil
	}
	rpc := yamlMappingValue(document.Content[0], "internal_rpc", restoreEnv)
	var tokenPairs []string
	seen := make(map[*yaml.Node]bool)
	protect := func(node *yaml.Node) {
		if node != nil && node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || seen[node] {
			return
		}
		seen[node] = true
		value := restoreEnv(node.Value)
		// A unique, quoted placeholder prevents aliases or literal ${...} in a
		// credential from being expanded again with the rest of the document.
		marker := "rpc-env-placeholder-" + rand.Text()
		tokenPairs = append(tokenPairs, strconv.Quote(marker), strconv.Quote(value))
		node.Value = marker
		node.Style = yaml.DoubleQuotedStyle
	}
	protect(yamlMappingValue(rpc, "client_token", restoreEnv))
	peers := yamlMappingValue(rpc, "peer_tokens", restoreEnv)
	if peers != nil && peers.Kind == yaml.AliasNode {
		peers = peers.Alias
	}
	if peers != nil && peers.Kind == yaml.MappingNode {
		for i := 1; i < len(peers.Content); i += 2 {
			protect(peers.Content[i])
		}
	}
	if len(tokenPairs) == 0 {
		return expandEnv(string(data)), nil
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	expanded := restoreEnv(encoded.String())
	return strings.NewReplacer(tokenPairs...).Replace(expanded), nil
}

func yamlMappingValue(node *yaml.Node, key string, expand func(string) string) *yaml.Node {
	seen := make(map[*yaml.Node]bool)
	var find func(*yaml.Node) *yaml.Node
	find = func(node *yaml.Node) *yaml.Node {
		if node == nil || seen[node] {
			return nil
		}
		seen[node] = true
		switch node.Kind {
		case yaml.AliasNode:
			return find(node.Alias)
		case yaml.SequenceNode:
			for _, item := range node.Content {
				if value := find(item); value != nil {
					return value
				}
			}
		case yaml.MappingNode:
			for i := 0; i < len(node.Content); i += 2 {
				keyNode := node.Content[i]
				if keyNode.Kind == yaml.AliasNode {
					keyNode = keyNode.Alias
				}
				if expand(keyNode.Value) == key {
					return node.Content[i+1]
				}
			}
			for i := 0; i < len(node.Content); i += 2 {
				keyNode := node.Content[i]
				if keyNode.Tag == "!!merge" || keyNode.Style == 0 && expand(keyNode.Value) == "<<" {
					if value := find(node.Content[i+1]); value != nil {
						return value
					}
				}
			}
		}
		return nil
	}
	return find(node)
}
