package config

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

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
	type scalarEdit struct {
		start, end  int
		placeholder string
	}
	var edits []scalarEdit
	seen := make(map[*yaml.Node]bool)
	protect := func(field string, node *yaml.Node) error {
		if node != nil && node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node == nil || node.Kind != yaml.ScalarNode || seen[node] {
			return nil
		}
		seen[node] = true
		value := restoreEnv(node.Value)
		if value == node.Value {
			return nil
		}
		// Reject before restoring: a tag other than !!str re-types the
		// credential when the document is parsed again, and a type mismatch
		// would echo the whole token in yaml's own decode error.
		if node.Tag != "!!str" {
			return fmt.Errorf("%s: credential scalar at line %d must not carry a non-string YAML tag (%s); drop the tag so the expanded token stays out of parse errors", field, node.Line, node.Tag)
		}
		start, end, err := yamlScalarRange(masked, node)
		if err != nil {
			return err
		}
		// A unique, quoted placeholder prevents aliases or literal ${...} in a
		// credential from being expanded again with the rest of the document.
		placeholder := strconv.Quote("rpc-env-placeholder-" + rand.Text())
		tokenPairs = append(tokenPairs, placeholder, strconv.Quote(value))
		edits = append(edits, scalarEdit{start: start, end: end, placeholder: placeholder})
		return nil
	}
	if err := protect("internal_rpc.client_token", yamlMappingValue(rpc, "client_token", restoreEnv)); err != nil {
		return "", err
	}
	peers := yamlMappingValue(rpc, "peer_tokens", restoreEnv)
	if peers != nil && peers.Kind == yaml.AliasNode {
		peers = peers.Alias
	}
	if peers != nil && peers.Kind == yaml.MappingNode {
		for i := 1; i < len(peers.Content); i += 2 {
			if err := protect("internal_rpc.peer_tokens."+peers.Content[i-1].Value, peers.Content[i]); err != nil {
				return "", err
			}
		}
	}
	if len(tokenPairs) == 0 {
		return expandEnv(string(data)), nil
	}
	// Edit only token scalars, from the end, so other fields retain the exact
	// source layout expected by their multiline environment values.
	slices.SortFunc(edits, func(a, b scalarEdit) int { return cmp.Compare(b.start, a.start) })
	for _, edit := range edits {
		masked = masked[:edit.start] + edit.placeholder + masked[edit.end:]
	}
	expanded := restoreEnv(masked)
	return strings.NewReplacer(tokenPairs...).Replace(expanded), nil
}

func yamlScalarRange(source string, node *yaml.Node) (int, int, error) {
	const lineBreaks = "\r\n\u0085\u2028\u2029"
	const whitespace = " \t" + lineBreaks
	lineStart := 0
	for line := 1; line < node.Line; line++ {
		offset := lineStart + strings.IndexAny(source[lineStart:], lineBreaks)
		r, size := utf8.DecodeRuneInString(source[offset:])
		lineStart = offset + size
		if r == '\r' && lineStart < len(source) && source[lineStart] == '\n' {
			lineStart++
		}
	}
	start := lineStart
	for column := 1; column < node.Column; column++ {
		_, size := utf8.DecodeRuneInString(source[start:])
		start += size
	}
	// Keep tag/anchor properties and their comments outside the replacement.
properties:
	for start < len(source) {
		r, size := utf8.DecodeRuneInString(source[start:])
		switch r {
		case ' ', '\t', '\r', '\n', '\u0085', '\u2028', '\u2029':
			start += size
		case '&', '!':
			for start < len(source) {
				r, size = utf8.DecodeRuneInString(source[start:])
				if strings.ContainsRune(whitespace, r) {
					break
				}
				start += size
			}
		case '#':
			for start < len(source) {
				r, size = utf8.DecodeRuneInString(source[start:])
				if strings.ContainsRune(lineBreaks, r) {
					break
				}
				start += size
			}
		default:
			break properties
		}
	}
	if node.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 &&
		strings.HasPrefix(source[start:], node.Value) {
		return start, start + len(node.Value), nil
	}
	// Ask YAML to identify the scalar's end rather than re-encode its document.
	// Block scalars may follow their key on another line or in a compact
	// sequence mapping. The scalar's source column bounds the parent indents.
	for end := start + len(node.Value); end <= len(source); end++ {
		if end < len(source) {
			r, _ := utf8.DecodeRuneInString(source[end:])
			if !strings.ContainsRune(whitespace+",]}:#", r) {
				continue
			}
		}
		for indent := range node.Column {
			var fragment yaml.Node
			if err := yaml.Unmarshal([]byte(strings.Repeat(" ", indent)+"value: "+source[start:end]), &fragment); err != nil {
				continue
			}
			if len(fragment.Content) == 1 {
				mapping := fragment.Content[0]
				if mapping.Kind == yaml.MappingNode && len(mapping.Content) == 2 &&
					mapping.Content[1].Kind == yaml.ScalarNode && mapping.Content[1].Value == node.Value {
					return start, end, nil
				}
			}
		}
	}
	return 0, 0, errors.New("rpc token scalar could not be located")
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
