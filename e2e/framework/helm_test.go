package framework

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestHelmReleaseValuesFilesAreValidYAML(t *testing.T) {
	releases := []HelmRelease{
		NATSRelease(E2ENamespace),
		GrafanaRelease(E2ENamespace),
		KubeStateMetricsRelease(E2ENamespace),
	}

	for _, release := range releases {
		t.Run(release.ReleaseName, func(t *testing.T) {
			if release.ValuesFile == "" {
				t.Fatal("ValuesFile is empty")
			}

			f, err := os.Open(release.ValuesFile)
			if err != nil {
				t.Fatalf("open values file: %v", err)
			}
			defer func() { _ = f.Close() }()

			decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
			var doc map[string]any
			if err := decoder.Decode(&doc); err != nil {
				t.Fatalf("decode values file: %v", err)
			}
			if len(doc) == 0 {
				t.Fatal("values file decoded to an empty document")
			}

			var extra map[string]any
			err = decoder.Decode(&extra)
			if err != nil && err != io.EOF {
				t.Fatalf("decode extra document: %v", err)
			}
		})
	}
}

func TestFrameworkGoFilesDoNotEmbedManifestYAML(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "*.go"))
	if err != nil {
		t.Fatalf("glob framework go files: %v", err)
	}

	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read source: %v", err)
			}
			if strings.Contains(string(src), "Values"+"YAML") {
				t.Fatalf("%s uses embedded Helm values; put values under testdata/helm-values", path)
			}

			fset := token.NewFileSet()
			fileAST, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Fatalf("parse source: %v", err)
			}

			ast.Inspect(fileAST, func(node ast.Node) bool {
				lit, ok := node.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote string literal at %s: %v", fset.Position(lit.Pos()), err)
				}
				if looksLikeEmbeddedManifestYAML(value) {
					t.Fatalf("embedded manifest-like YAML at %s; put manifests under testdata", fset.Position(lit.Pos()))
				}
				return true
			})
		})
	}
}

func looksLikeEmbeddedManifestYAML(value string) bool {
	markers := []string{
		"apiVersion:",
		"\nkind:",
		"\nmetadata:",
		"\nspec:",
		"\nresources:",
		"\nreplicas:",
		"topologySpreadConstraints:",
		"kubectl.safed.io/drain-priority:",
	}

	count := 0
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			count++
		}
	}
	return count >= 2
}
