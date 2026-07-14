# imagine Image Template Library Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a minimal Go library that finds object-style `image:` fields in Kubernetes YAML and mirrors a source tree into an output directory with those fields replaced by concrete OCI image references.

**Architecture:** A single package `imagine` at the module root. Both public functions walk an `fs.FS` and parse YAML files into `gopkg.in/yaml.v3` node trees so comments and formatting survive. `GetImageTemplates` collects content-deduplicated templates; `RenderManifests` validates all templates against the provided references, then mirrors the tree (rendering YAML, copying everything else). Templates are identified by a canonical JSON serialization of the image object, so the two functions always agree on keys.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, `github.com/stretchr/testify` (tests), stdlib `io/fs`, `encoding/json`, `testing/fstest`.

## Global Constraints

- Module path: `github.com/draganm/imagine` (go directive `go 1.26.4`, already set).
- Sole external runtime dependency: `gopkg.in/yaml.v3`. Test-only: `github.com/stretchr/testify`.
- YAML file = a regular file whose name ends in `.yaml` or `.yml` (case-insensitive). Only these are parsed/rendered; all other files are copied byte-for-byte.
- Object-style `image:` field = a mapping node with a scalar key `image` whose value node is itself a mapping. Scalar `image:` values are left untouched. Detection is generic (any depth, any location).
- Canonical key = `json.Marshal` of the image object decoded to `map[string]any`. This is both the dedup identity and the `refs` lookup key.
- Missing reference in `RenderManifests` is a hard error raised before any output is written.
- Remove any binaries produced during the work.

---

### Task 1: Package skeleton, types, and `GetImageTemplates`

**Files:**
- Create: `imagine.go`
- Create: `imagine_test.go`
- Modify: `go.mod` (via `go get` / `go mod tidy`)
- Create: `go.sum` (via `go mod tidy`)

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `type ImageTemplate struct { Key string; Content map[string]any }`
  - `func GetImageTemplates(src fs.FS) ([]ImageTemplate, error)`
  - Unexported helpers reused by later tasks: `func isYAML(p string) bool`, `func parseDocuments(data []byte) ([]*yaml.Node, error)`, `func walkImageObjects(n *yaml.Node, fn func(value *yaml.Node) error) error`, `func canonicalize(v *yaml.Node) (string, map[string]any, error)`, `func templatesFromYAML(data []byte) ([]ImageTemplate, error)`.

- [ ] **Step 1: Add dependencies**

Run:
```bash
go get gopkg.in/yaml.v3@v3.0.1
go get github.com/stretchr/testify@v1.9.0
```
Expected: `go.mod` gains both requires; `go.sum` created.

- [ ] **Step 2: Write the failing tests**

Create `imagine_test.go`:
```go
package imagine

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestGetImageTemplatesFindsObjectImage(t *testing.T) {
	src := fstest.MapFS{
		"deploy.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte(`apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: api
          image:
            context: ./api
            dockerfile: Dockerfile
        - name: sidecar
          image: busybox:1.36
`)},
	}

	tmpls, err := GetImageTemplates(src)
	require.NoError(t, err)
	require.Len(t, tmpls, 1)
	require.Equal(t, map[string]any{
		"context":    "./api",
		"dockerfile": "Dockerfile",
	}, tmpls[0].Content)
	require.Equal(t, `{"context":"./api","dockerfile":"Dockerfile"}`, tmpls[0].Key)
}

func TestGetImageTemplatesDeduplicatesAcrossFilesAndKeyOrder(t *testing.T) {
	// Same object, different key order, in two files.
	src := fstest.MapFS{
		"a.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./api\n  dockerfile: Dockerfile\n")},
		"z.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  dockerfile: Dockerfile\n  context: ./api\n")},
	}
	tmpls, err := GetImageTemplates(src)
	require.NoError(t, err)
	require.Len(t, tmpls, 1)
}

func TestGetImageTemplatesIgnoresNonYAMLAndScalarImage(t *testing.T) {
	src := fstest.MapFS{
		"note.txt":    &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./nope\n")},
		"scalar.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image: nginx:1.25\n")},
	}
	tmpls, err := GetImageTemplates(src)
	require.NoError(t, err)
	require.Empty(t, tmpls)
}

func TestGetImageTemplatesDeterministicOrder(t *testing.T) {
	src := fstest.MapFS{
		"b.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./b\n")},
		"a.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./a\n")},
	}
	tmpls, err := GetImageTemplates(src)
	require.NoError(t, err)
	require.Len(t, tmpls, 2)
	require.Equal(t, "./a", tmpls[0].Content["context"]) // a.yaml visited before b.yaml
	require.Equal(t, "./b", tmpls[1].Content["context"])
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: GetImageTemplates` (does not compile).

- [ ] **Step 4: Write the implementation**

Create `imagine.go`:
```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (all four tests).

- [ ] **Step 6: Commit**

```bash
go mod tidy
git add imagine.go imagine_test.go go.mod go.sum
git commit -m "feat: GetImageTemplates discovers and dedups object-style image fields"
```

---

### Task 2: `renderYAML` single-stream rendering

**Files:**
- Modify: `imagine.go` (append `encodeDocuments` and `renderYAML`)
- Modify: `imagine_test.go` (append rendering tests)

**Interfaces:**
- Consumes: `parseDocuments`, `walkImageObjects`, `canonicalize` (Task 1).
- Produces:
  - `func encodeDocuments(docs []*yaml.Node) ([]byte, error)`
  - `func renderYAML(data []byte, refs map[string]string) ([]byte, error)`

- [ ] **Step 1: Write the failing tests**

Append to `imagine_test.go` (add `"gopkg.in/yaml.v3"` to the import block):
```go
func TestRenderYAMLReplacesImageObject(t *testing.T) {
	in := []byte(`containers:
  - name: api
    image:
      context: ./api
`)
	refs := map[string]string{`{"context":"./api"}`: "registry.example.com/api@sha256:abc"}

	out, err := renderYAML(in, refs)
	require.NoError(t, err)
	require.NotContains(t, string(out), "context")

	var got struct {
		Containers []struct {
			Name  string `yaml:"name"`
			Image string `yaml:"image"`
		} `yaml:"containers"`
	}
	require.NoError(t, yaml.Unmarshal(out, &got))
	require.Equal(t, "registry.example.com/api@sha256:abc", got.Containers[0].Image)
}

func TestRenderYAMLLeavesScalarImageUntouched(t *testing.T) {
	in := []byte("image: nginx:1.25\n")
	out, err := renderYAML(in, map[string]string{})
	require.NoError(t, err)
	require.Contains(t, string(out), "nginx:1.25")
}

func TestRenderYAMLPreservesComments(t *testing.T) {
	in := []byte(`# top comment
kind: Deployment # inline
spec:
  image:
    context: ./api
`)
	refs := map[string]string{`{"context":"./api"}`: "reg/api@sha256:abc"}
	out, err := renderYAML(in, refs)
	require.NoError(t, err)
	s := string(out)
	require.Contains(t, s, "# top comment")
	require.Contains(t, s, "# inline")
	require.Contains(t, s, "reg/api@sha256:abc")
}

func TestRenderYAMLMultiDoc(t *testing.T) {
	in := []byte(`image:
  context: ./a
---
image:
  context: ./b
`)
	refs := map[string]string{
		`{"context":"./a"}`: "reg/a@sha256:1",
		`{"context":"./b"}`: "reg/b@sha256:2",
	}
	out, err := renderYAML(in, refs)
	require.NoError(t, err)

	docs, err := parseDocuments(out) // round-trips to two documents
	require.NoError(t, err)
	require.Len(t, docs, 2)
	require.Contains(t, string(out), "reg/a@sha256:1")
	require.Contains(t, string(out), "reg/b@sha256:2")
}

func TestRenderYAMLMissingRef(t *testing.T) {
	in := []byte("image:\n  context: ./api\n")
	_, err := renderYAML(in, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "context")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: renderYAML`.

- [ ] **Step 3: Write the implementation**

Append to `imagine.go`:
```go
func encodeDocuments(docs []*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("could not encode YAML: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("could not finalize YAML: %w", err)
	}
	return buf.Bytes(), nil
}

func renderYAML(data []byte, refs map[string]string) ([]byte, error) {
	docs, err := parseDocuments(data)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		err := walkImageObjects(doc, func(v *yaml.Node) error {
			key, _, err := canonicalize(v)
			if err != nil {
				return err
			}
			ref, ok := refs[key]
			if !ok {
				return fmt.Errorf("no image reference provided for image template %s", key)
			}
			*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ref}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return encodeDocuments(docs)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (all tests, including Task 1's).

- [ ] **Step 5: Commit**

```bash
git add imagine.go imagine_test.go
git commit -m "feat: renderYAML replaces image objects while preserving YAML"
```

---

### Task 3: `RenderManifests` tree mirroring + two-pass validation

**Files:**
- Modify: `imagine.go` (append `RenderManifests`; add `os` and `path/filepath` imports)
- Modify: `imagine_test.go` (append tree tests; add `os` and `path/filepath` imports)

**Interfaces:**
- Consumes: `GetImageTemplates`, `renderYAML`, `isYAML` (earlier tasks).
- Produces: `func RenderManifests(refs map[string]string, src fs.FS, dstDir string) error`

- [ ] **Step 1: Write the failing tests**

Append to `imagine_test.go` (add `"os"` and `"path/filepath"` to imports):
```go
func TestRenderManifestsMirrorsTree(t *testing.T) {
	src := fstest.MapFS{
		"app/deploy.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./api\n")},
		"app/config.json": &fstest.MapFile{Mode: 0o644, Data: []byte(`{"k":"v"}`)},
		"README.md":       &fstest.MapFile{Mode: 0o644, Data: []byte("hi")},
	}
	refs := map[string]string{`{"context":"./api"}`: "reg/api@sha256:abc"}
	dst := t.TempDir()

	require.NoError(t, RenderManifests(refs, src, dst))

	deploy, err := os.ReadFile(filepath.Join(dst, "app", "deploy.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(deploy), "reg/api@sha256:abc")

	cfg, err := os.ReadFile(filepath.Join(dst, "app", "config.json"))
	require.NoError(t, err)
	require.Equal(t, `{"k":"v"}`, string(cfg)) // copied verbatim

	readme, err := os.ReadFile(filepath.Join(dst, "README.md"))
	require.NoError(t, err)
	require.Equal(t, "hi", string(readme))
}

func TestRenderManifestsMissingRefWritesNothing(t *testing.T) {
	src := fstest.MapFS{
		"deploy.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte("image:\n  context: ./api\n")},
	}
	dst := t.TempDir()

	err := RenderManifests(map[string]string{}, src, dst)
	require.Error(t, err)

	entries, rerr := os.ReadDir(dst)
	require.NoError(t, rerr)
	require.Empty(t, entries) // nothing written
}

func TestRenderManifestsReproducesEmptyDir(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "empty"), 0o755))
	dst := t.TempDir()

	require.NoError(t, RenderManifests(map[string]string{}, os.DirFS(srcDir), dst))

	info, err := os.Stat(filepath.Join(dst, "empty"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestRenderManifestsPreservesFileMode(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	dst := t.TempDir()

	require.NoError(t, RenderManifests(map[string]string{}, os.DirFS(srcDir), dst))

	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL — `undefined: RenderManifests`.

- [ ] **Step 3: Write the implementation**

Add `"os"` and `"path/filepath"` to the import block in `imagine.go`, then append:
```go
// RenderManifests walks src and reproduces its structure under dstDir. YAML
// files have every object-style image: field replaced with refs[template.Key];
// all other files are copied verbatim; directories (including empty ones) are
// recreated. All discovered templates are validated against refs before any
// output is written, so a missing reference never leaves a partial tree.
func RenderManifests(refs map[string]string, src fs.FS, dstDir string) error {
	// Pass 1: validate every discovered template has a reference.
	tmpls, err := GetImageTemplates(src)
	if err != nil {
		return err
	}
	var missing []string
	for _, t := range tmpls {
		if _, ok := refs[t.Key]; !ok {
			missing = append(missing, t.Key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing image references for templates: %s", strings.Join(missing, ", "))
	}

	// Pass 2: mirror the source tree into dstDir.
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dstDir, filepath.FromSlash(p))
		if d.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("could not create directory %s: %w", dst, err)
			}
			return nil
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return fmt.Errorf("could not read %s: %w", p, err)
		}
		if isYAML(p) {
			data, err = renderYAML(data, refs)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		if perm == 0 {
			perm = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("could not create directory for %s: %w", dst, err)
		}
		if err := os.WriteFile(dst, data, perm); err != nil {
			return fmt.Errorf("could not write %s: %w", dst, err)
		}
		return nil
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add imagine.go imagine_test.go
git commit -m "feat: RenderManifests mirrors source tree with references injected"
```

---

### Task 4: End-to-end integration test + README usage

**Files:**
- Modify: `imagine_test.go` (append integration test)
- Modify: `README.md` (append Usage section)

**Interfaces:**
- Consumes: `GetImageTemplates`, `RenderManifests` (public API).
- Produces: nothing new (documentation + integration coverage).

- [ ] **Step 1: Write the failing test**

Append to `imagine_test.go`:
```go
func TestEndToEndTwoPhaseFlow(t *testing.T) {
	src := fstest.MapFS{
		"deploy.yaml": &fstest.MapFile{Mode: 0o644, Data: []byte(`spec:
  containers:
    - name: api
      image:
        context: ./api
        dockerfile: Dockerfile
`)},
	}

	// Phase 1: discover templates.
	tmpls, err := GetImageTemplates(src)
	require.NoError(t, err)
	require.Len(t, tmpls, 1)

	// Simulate the build system producing an OCI reference per template.
	refs := map[string]string{}
	for _, tm := range tmpls {
		refs[tm.Key] = "registry.example.com/api@sha256:deadbeef"
	}

	// Phase 2: render, proving the keys agree across both functions.
	dst := t.TempDir()
	require.NoError(t, RenderManifests(refs, src, dst))

	out, err := os.ReadFile(filepath.Join(dst, "deploy.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(out), "registry.example.com/api@sha256:deadbeef")
	require.NotContains(t, string(out), "dockerfile")
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./... -run TestEndToEndTwoPhaseFlow -v`
Expected: PASS (both functions already exist; this is integration coverage that the keys agree).

- [ ] **Step 3: Append Usage section to README.md**

Append to `README.md`:
```markdown

## Usage

```go
package main

import (
	"log"
	"os"

	"github.com/draganm/imagine"
)

func main() {
	src := os.DirFS("manifests")

	// Phase 1: collect the object-style image: fields to build.
	templates, err := imagine.GetImageTemplates(src)
	if err != nil {
		log.Fatal(err)
	}

	// Delegate building to your build system, keyed by template.Key.
	refs := map[string]string{}
	for _, t := range templates {
		refs[t.Key] = buildImage(t.Content) // returns e.g. "reg/app@sha256:..."
	}

	// Phase 2: mirror manifests -> rendered, with references injected.
	if err := imagine.RenderManifests(refs, src, "rendered"); err != nil {
		log.Fatal(err)
	}
}
```
```

- [ ] **Step 4: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add imagine_test.go README.md
git commit -m "test: end-to-end two-phase flow; docs: add usage example"
```

---

## Self-Review

**Spec coverage:**
- `GetImageTemplates` / content-based dedup / deterministic order → Task 1.
- Object vs scalar `image:` detection; non-YAML ignored in discovery → Task 1.
- Canonical key agreement between functions → Tasks 1 & 4.
- `renderYAML` replacement, comment/order preservation, multi-doc, missing-ref error → Task 2.
- `RenderManifests` tree mirror, verbatim copy, empty dirs, mode preservation, missing-ref-writes-nothing → Task 3.
- Two-phase end-to-end + docs → Task 4.

**Placeholder scan:** none — every step contains complete code and exact commands.

**Type consistency:** `ImageTemplate{Key, Content}`, `GetImageTemplates(fs.FS)`, `RenderManifests(map[string]string, fs.FS, string)`, and helpers `isYAML`/`parseDocuments`/`walkImageObjects`/`canonicalize`/`templatesFromYAML`/`encodeDocuments`/`renderYAML` are named identically everywhere they appear.
