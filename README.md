# imagine

A minimal library to replace templated `image:` fields in Kubernetes YAML files with concrete OCI image references.

Phases:

1. **GetImageTemplates:** Scans YAML files for object-style `image` fields and deduplicates them.
2. **RenderManifests:** Replaces matching objects in manifests with the given OCI image references.

Use this library to delegate image building to your build system and inject the resulting images into templates.
