// Package imagine replaces object-style image: fields in Kubernetes YAML
// manifests with concrete OCI image references.
//
// The typical flow is two-phase: GetImageTemplates collects the deduplicated
// set of object-style image: fields from a source tree, the caller builds each
// into an OCI reference, and RenderManifests mirrors the tree into an output
// directory with those references injected.
package imagine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImageTemplate is a deduplicated object-style image: field discovered in the
// source tree.
type ImageTemplate struct {
	// Key is a stable, canonical identifier for the template. It is also the
	// key used to look the template up in the refs map passed to
	// RenderManifests.
	Key string
	// Content is the decoded image object, provided so the caller's build
	// system can read fields such as context, dockerfile, etc.
	Content map[string]any
}

// GetImageTemplates walks src, scans every YAML file (.yaml/.yml) for
// object-style image: fields, and returns the deduplicated set in
// first-appearance order. Files are visited in lexical order, so the result is
// deterministic. Callers typically pass os.DirFS(dir).
func GetImageTemplates(src fs.FS) ([]ImageTemplate, error) {
	seen := map[string]bool{}
	var out []ImageTemplate

	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isYAML(p) {
			return nil
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return fmt.Errorf("could not read %s: %w", p, err)
		}
		tmpls, err := templatesFromYAML(data)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		for _, t := range tmpls {
			if !seen[t.Key] {
				seen[t.Key] = true
				out = append(out, t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isYAML(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func parseDocuments(data []byte) ([]*yaml.Node, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("could not parse YAML: %w", err)
		}
		docs = append(docs, &doc)
	}
	return docs, nil
}

// walkImageObjects visits every object-style image: field (an "image" key whose
// value is a mapping) reachable from n, calling fn with the value node.
func walkImageObjects(n *yaml.Node, fn func(value *yaml.Node) error) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "image" && v.Kind == yaml.MappingNode {
				if err := fn(v); err != nil {
					return err
				}
			}
		}
	}
	for _, c := range n.Content {
		if err := walkImageObjects(c, fn); err != nil {
			return err
		}
	}
	return nil
}

// canonicalize decodes an image object node into a map and returns a canonical
// JSON key for it. json.Marshal sorts map keys, so equal objects yield equal
// keys regardless of source key order or comments.
func canonicalize(v *yaml.Node) (string, map[string]any, error) {
	var content map[string]any
	if err := v.Decode(&content); err != nil {
		return "", nil, fmt.Errorf("could not decode image object: %w", err)
	}
	key, err := json.Marshal(content)
	if err != nil {
		return "", nil, fmt.Errorf("could not canonicalize image object: %w", err)
	}
	return string(key), content, nil
}

func templatesFromYAML(data []byte) ([]ImageTemplate, error) {
	docs, err := parseDocuments(data)
	if err != nil {
		return nil, err
	}
	var out []ImageTemplate
	for _, doc := range docs {
		err := walkImageObjects(doc, func(v *yaml.Node) error {
			key, content, err := canonicalize(v)
			if err != nil {
				return err
			}
			out = append(out, ImageTemplate{Key: key, Content: content})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
