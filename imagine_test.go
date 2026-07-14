package imagine

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
