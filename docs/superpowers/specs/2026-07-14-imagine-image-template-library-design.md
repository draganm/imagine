# imagine — image template library design

Date: 2026-07-14

## Purpose

`imagine` is a minimal Go library that replaces templated, object-style `image:`
fields in Kubernetes YAML manifests with concrete OCI image references. It lets a
caller delegate image building to their own build system: first collect the image
templates, build each one into an OCI reference, then render the manifests with
those references injected.

It follows the style of `github.com/draganm/manifestor`: walk the `gopkg.in/yaml.v3`
node tree so that comments, key ordering, and formatting of the rest of the
manifest are preserved and the output is always valid YAML.

## Scope

In scope:

- `GetImageTemplates` — scan YAML for object-style `image:` fields, deduplicate.
- `RenderManifests` — replace those objects with concrete OCI reference strings.

Out of scope (YAGNI):

- CLI binary.
- Filesystem / glob walking (caller reads and writes files).
- Actually building images.
- Git integration.

## Public API

Package `imagine` at the module root (`github.com/draganm/imagine`).
Sole dependency: `gopkg.in/yaml.v3`.

```go
// ImageTemplate is a deduplicated object-style image: field discovered in the
// manifests.
type ImageTemplate struct {
    // Key is a stable, canonical identifier for the template. It is also the
    // key used to look the template up in the refs map passed to
    // RenderManifests.
    Key string

    // Content is the decoded image object, provided so the caller's build
    // system can read fields such as context, dockerfile, etc.
    Content map[string]any
}

// GetImageTemplates scans one or more YAML byte streams (each stream may contain
// multiple ---separated documents) and returns the deduplicated set of
// object-style image: fields, in first-appearance order.
func GetImageTemplates(manifests ...[]byte) ([]ImageTemplate, error)

// RenderManifests replaces every object-style image: field with its concrete OCI
// reference. refs is keyed by ImageTemplate.Key. It returns one rendered byte
// stream per input stream, preserving multi-document structure.
func RenderManifests(refs map[string]string, manifests ...[]byte) ([][]byte, error)
```

## Definitions

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

1. For each input stream, decode every document into a `*yaml.Node`.
2. Recursively walk each node tree. At every mapping node, for each `image` key
   whose value is a mapping, decode the value into `map[string]any` and compute
   its canonical key.
3. Collect templates, deduplicating by key. Preserve first-appearance order.
4. Return `[]ImageTemplate`.

### RenderManifests

1. For each input stream, decode every document into a `*yaml.Node`.
2. Recursively walk each node tree. For each object-style `image:` field, compute
   its canonical key and look up `refs[key]`.
   - If present, replace the value node in place with a `!!str` scalar holding the
     reference. The `image:` key node and its comments stay put.
   - If absent, return an error naming the missing key (a missing image would
     otherwise emit a broken manifest).
3. Re-encode each stream's documents back to bytes (yaml.v3 with 2-space indent),
   preserving `---` document separation. Return `[][]byte`, one per input stream.

Extra entries in `refs` that match no template are ignored (the build system may
legitimately produce a superset).

## Error handling

- YAML parse failures are wrapped with document/stream context.
- A discovered image template with no matching entry in `refs` is an explicit,
  hard error identifying the missing key.

## Internal structure

- `imagine.go` — public API, `ImageTemplate`, node walking, canonical key,
  rendering.
- `imagine_test.go` — tests.

Keep helpers small and focused: node walking, image-mapping detection, canonical
key computation, and value-node replacement are each their own function.

## Testing (TDD, testify)

- Single object-style `image:` field is discovered and rendered.
- Deduplication across multiple documents and across multiple input streams.
- Templates found at nested / non-container locations.
- Non-`image` mappings are left untouched.
- Scalar (already-concrete) `image:` values are left untouched.
- Multi-document streams round-trip with `---` preserved.
- Missing ref in `RenderManifests` returns an error.
- Comments and key ordering of surrounding YAML are preserved through rendering.
- Equal objects with different source key ordering dedup to one template.
