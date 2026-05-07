package framework

import (
	"io"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestManifestTemplatesRenderValidYAML(t *testing.T) {
	manifests := map[string]string{
		"deployment-default": DeploymentManifest(DeploymentManifestOptions{
			Namespace: E2ENamespace,
			Name:      "template-deployment",
			Priority:  10,
		}),
		"deployment-options": DeploymentManifest(DeploymentManifestOptions{
			Namespace:               E2ENamespace,
			Name:                    "template-options",
			Priority:                20,
			Replicas:                2,
			MinReadySeconds:         5,
			ProgressDeadlineSeconds: 30,
			Recreate:                true,
			ReadinessCommand:        "test -f /tmp/ready",
		}),
		"statefulset": StatefulSetManifest(StatefulSetManifestOptions{
			Namespace: E2ENamespace,
			Name:      "template-statefulset",
			Priority:  30,
		}),
		"standalone-pod":          StandalonePodManifest(E2ENamespace, "template-pod", true),
		"replicaset":              ReplicaSetManifest(E2ENamespace, "template-rs", true),
		"job":                     JobManifest(E2ENamespace, "template-job"),
		"pdb":                     PDBManifest(E2ENamespace, "template-pdb", "template-pdb", 1),
		"worker":                  WorkerManifest,
		"daemonset":               DaemonSetManifest,
		"blocking-pdb":            BlockingPDBManifest,
		"crashing-deployment":     CrashingDeploymentManifest,
		"standalone-pod-with-pdb": StandalonePodWithPDBManifest,
	}

	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			docs := decodeManifestDocuments(t, manifest)
			if len(docs) == 0 {
				t.Fatalf("rendered no YAML documents")
			}
			for i, doc := range docs {
				if doc["apiVersion"] == nil || doc["kind"] == nil {
					t.Fatalf("document %d missing apiVersion/kind: %#v", i, doc)
				}
			}
		})
	}
}

func decodeManifestDocuments(t *testing.T, manifest string) []map[string]any {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)

	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode manifest:\n%s\nerror: %v", manifest, err)
		}
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}
