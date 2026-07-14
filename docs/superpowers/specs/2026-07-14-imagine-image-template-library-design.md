# imagine — image template library design

Date: 2026-07-14

## Purpose

`imagine` is a minimal Go library that replaces templated, object-style `image:`
fields in Kubernetes YAML manifests with concrete OCI image references. It lets a
caller delegate image building to their own build system: first collect the image
templates from a source tree, build each one into an OCI reference, then render
the tree into an output directory with those references injected.

It follows the style of `github.com/draganm/manifestor`: walk the `gopkg.in/yaml.v3`
node tree so that comments, key ordering, and formatting of the rest of the
manifest are preserved and the output is always valid YAML.

## Scope

In scope:

- `GetImageTemplates` — walk a source tree, scan YAML files for object-style
  `image:` fields, deduplicate.
- `RenderManifests` — walk a source tree and reproduce it in an output directory,
  replacing object-style `image:` fields with concrete OCI reference strings.
- Filesystem walking, and reproducing the source directory structure in the
  output.

Out of scope (YAGNI):

- CLI binary.
- Actually building images.
- Git integration.

## Public API

Package `imagine` at the module root (`github.com/draganm/imagine`).
Sole external dependency: `gopkg.in/yaml.v3`.

```go
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
// first-appearance order (deterministic: files are visited in lexical order).
//
// Callers typically pass os.DirFS(dir).
func GetImageTemplates(src fs.FS) ([]ImageTemplate, error)

// RenderManifests walks src and reproduces its structure under dstDir:
//   - .yaml/.yml files are rendered, replacing every object-style image: field
//     with refs[template.Key], and written to the mirrored relative path.
//   - all other files are copied byte-for-byte to the mirrored relative path.
//   - directories (including empty ones) are recreated.
//
// refs is keyed by ImageTemplate.Key. All discovered templates are validated
// against refs before any output is written.
func RenderManifests(refs map[string]string, src fs.FS, dstDir string) error
```

## Definitions

**YAML file:** a regular file whose name ends in `.yaml` or `.yml`
(case-insensitive). Only these are parsed and rendered; every other file is
treated as opaque and copied verbatim.

**Object-style `image:` field (a template):** any mapping node that has a scalar
key `image` whose value node is itself a mapping. A scalar `image: nginx:1.2.3`
is already a concrete reference and is left completely untouched.

Detection is generic: an `image` mapping is matched at any depth and any location
in the document. It is not restricted to Kubernetes container specs.

**Canonical key:** the image object's value node is decoded into `map[string]any`
and serialized with `encoding/json`. Go's `json.Marshal` emits map keys in sorted
order, so two objects with equal content produce an identical key regardless of
source key ordering or comments. This canonical string is both the dedup identity
and the `refs` lookup key. The same function computes the key in both
`GetImageTemplates` and `RenderManifests`, guaranteeing they agree.

## Behavior

### GetImageTemplates

1. Walk `src` with `fs.WalkDir` (lexical order).
2. For each YAML file, decode every `---`-separated document into a `*yaml.Node`.
3. Recursively walk each node tree. At every mapping node, for each `image` key
   whose value is a mapping, decode the value into `map[string]any` and compute
   its canonical key.
4. Collect templates, deduplicating by key, preserving first-appearance order.
5. Return `[]ImageTemplate`.

### RenderManifests

Two passes so that a missing reference never produces a partial output tree:

**Pass 1 — validate.** Run the same discovery as `GetImageTemplates` over `src`.
For every discovered template key, confirm `refs` contains it. If any are
missing, return an error listing the missing keys (and a source file where each
occurs) and write nothing.

**Pass 2 — write.** Walk `src` again. For each entry, mirror it under `dstDir` at
the same relative path:

- Directory: `os.MkdirAll` the mirrored path.
- YAML file: decode its documents, replace each object-style `image:` value node
  in place with a `!!str` scalar holding `refs[key]` (the `image:` key node and
  its comments stay put), re-encode the documents (yaml.v3, 2-space indent,
  `---`-separated), and write to the mirrored path.
- Other file: copy bytes verbatim to the mirrored path.

File permissions from the source entry's `fs.FileInfo` are preserved where
available; `dstDir` is created if it does not exist; files at colliding
destination paths are overwritten.

Extra entries in `refs` that match no template are ignored (the build system may
legitimately produce a superset).

## Error handling

- YAML parse failures are wrapped with the file path and document context.
- A discovered image template with no matching entry in `refs` is an explicit,
  hard error (raised in pass 1) identifying the missing key and a source file.
- Filesystem read/write failures are wrapped with the offending path.

## Internal structure

- `imagine.go` — public API (`ImageTemplate`, `GetImageTemplates`,
  `RenderManifests`), tree walking, node walking, canonical key, rendering.
- `imagine_test.go` — tests.

Keep helpers small and focused: source-tree walking, per-document node walking,
image-mapping detection, canonical key computation, and value-node replacement
are each their own function.

## Testing (TDD, testify)

Tests use `fstest.MapFS` for input and `t.TempDir()` for output.

- Single object-style `image:` field is discovered and rendered.
- Deduplication across multiple documents and across multiple files.
- Templates found at nested / non-container locations.
- Non-`image` mappings are left untouched.
- Scalar (already-concrete) `image:` values are left untouched.
- Multi-document YAML files round-trip with `---` preserved.
- Non-YAML files are copied through byte-for-byte; empty directories are
  reproduced; the output structure mirrors the input.
- Missing ref in `RenderManifests` returns an error and writes no output.
- Comments and key ordering of surrounding YAML are preserved through rendering.
- Equal objects with different source key ordering dedup to one template.
- Deterministic template ordering across a multi-file tree.
