# imagine

A minimal library to replace templated `image:` fields in Kubernetes YAML files with concrete OCI image references.

Phases:

1. **GetImageTemplates:** Scans YAML files for object-style `image` fields and deduplicates them.
2. **RenderManifests:** Replaces matching objects in manifests with the given OCI image references.

Use this library to delegate image building to your build system and inject the resulting images into templates.

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

An object-style `image:` field is any `image:` whose value is a mapping:

```yaml
containers:
  - name: api
    image:
      context: ./api
      dockerfile: Dockerfile
```

`GetImageTemplates` returns each unique image object (deduplicated by canonical
content) with a stable `Key`. After your build system turns each into an OCI
reference, `RenderManifests` reproduces the source tree under the output
directory, replacing every matching `image:` object with its reference and
copying all other files verbatim. A plain string `image: nginx:1.25` is already
concrete and is left untouched.
