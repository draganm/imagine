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
