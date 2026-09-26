package launch

// yaml_document.go — editing a YAML file that belongs to the user.
//
// Several integrations keep their settings in YAML. oaica owns a handful of
// keys in those documents and nothing else: the rest is the user's file, with
// their comments, key order, anchors, and their own choice of scalar spelling
// (`1.0` is not `1`; a hand-written date is not the encoder's timestamp; a
// quoted string keeps its quotes).
//
// Decoding such a file into a map[string]any and marshalling it back deletes
// every comment and re-renders every scalar, which is a silent edit to a file
// oaica was only supposed to add a key to (2026-09-27 audit, round 17 — hermes
// and omp did exactly that). These helpers keep the document as a yaml.Node and
// set only the keys oaica manages, so everything it does not touch survives
// byte for byte. This is the pattern deepseek_harness.go already used.

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// readYAMLDocument parses path into a document node whose root is a mapping.
// A missing file is an empty mapping document, not an error — the callers all
// create the file on write. A file whose root is not a mapping IS an error:
// oaica cannot add a key to it, and rewriting it would delete everything.
func readYAMLDocument(path string) (*yaml.Node, error) {
	document := &yaml.Node{Kind: yaml.DocumentNode}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
		return document, nil
	}
	if err := yaml.Unmarshal(data, document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind == yaml.ScalarNode && document.Content[0].Tag == "!!null" {
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("root must be a mapping")
	}
	return document, nil
}

// yamlRootMapping is the document's top-level mapping.
func yamlRootMapping(document *yaml.Node) *yaml.Node {
	return document.Content[0]
}

// yamlEnsureMapping returns the mapping at key, creating an empty one (and the
// key) when it is absent or holds something else.
func yamlEnsureMapping(mapping *yaml.Node, key string) *yaml.Node {
	if value := yamlNodeValue(mapping, key); value != nil && value.Kind == yaml.MappingNode {
		return value
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	yamlSetNode(mapping, key, value)
	return value
}

// yamlSetValue encodes value and sets it at key, replacing any node there.
func yamlSetValue(mapping *yaml.Node, key string, value any) error {
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return err
	}
	yamlSetNode(mapping, key, node)
	return nil
}

// yamlSetNode sets an already-built node at key, appending the key when it is
// absent (so a new key lands at the end of the mapping, as a hand-edit would).
//
// A replacement inherits the comments the node it displaces carried. On
// `provider: ollama # the active provider` the line comment belongs to the
// VALUE node, so replacing the value would silently delete the user's
// annotation of the very line oaica edits — the one comment a reader is most
// likely to be looking for when they open the file afterwards.
func yamlSetNode(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			previous := mapping.Content[i+1]
			if previous != nil {
				if value.HeadComment == "" {
					value.HeadComment = previous.HeadComment
				}
				if value.LineComment == "" {
					value.LineComment = previous.LineComment
				}
				if value.FootComment == "" {
					value.FootComment = previous.FootComment
				}
			}
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// yamlDeleteKey removes key from the mapping, if it is there.
func yamlDeleteKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// yamlNodeValue returns the node at key, or nil.
func yamlNodeValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// yamlNodeAsAny decodes a node into the map-shaped value the existing
// integrations build: the parts of a document that are read, merged, or
// filtered in Go, and then written back whole with yamlSetValue. A nil node
// decodes to nil, so a caller can pass a missing key straight through.
func yamlNodeAsAny(node *yaml.Node) (any, error) {
	if node == nil {
		return nil, nil
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// yamlMarshalDocument renders the document back, comments and all.
func yamlMarshalDocument(document *yaml.Node) ([]byte, error) {
	return yaml.Marshal(document)
}
