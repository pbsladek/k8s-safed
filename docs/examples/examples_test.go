package examples

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pbsladek/k8s-safed/pkg/config"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestProfilesExampleLoads(t *testing.T) {
	cfg, err := config.Load("profiles.yaml")
	if err != nil {
		t.Fatalf("profiles.yaml should parse as safed config: %v", err)
	}
	for _, name := range []string{"prod", "staging", "spot-scale-down", "audit-json"} {
		if _, err := cfg.GetProfile(name); err != nil {
			t.Fatalf("profiles.yaml missing profile %q: %v", name, err)
		}
	}
}

func TestRBACExampleContainsRequiredRules(t *testing.T) {
	data, err := os.ReadFile("rbac.yaml")
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Rules      []struct {
			APIgroups []string `json:"apiGroups"`
			Resources []string `json:"resources"`
			Verbs     []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := sigsyaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("rbac.yaml should parse as YAML: %v", err)
	}
	if doc.APIVersion != "rbac.authorization.k8s.io/v1" || doc.Kind != "ClusterRole" {
		t.Fatalf("rbac.yaml should be a ClusterRole, got %s %s", doc.APIVersion, doc.Kind)
	}
	for _, want := range []struct {
		resource string
		verb     string
	}{
		{"nodes", "patch"},
		{"pods", "list"},
		{"pods/eviction", "create"},
		{"events", "create"},
		{"deployments", "patch"},
		{"statefulsets", "patch"},
		{"replicasets", "list"},
		{"poddisruptionbudgets", "list"},
	} {
		if !hasRBACRule(doc.Rules, want.resource, want.verb) {
			t.Fatalf("rbac.yaml missing %s/%s rule", want.resource, want.verb)
		}
	}
}

func TestReadmeCommandBlocksUseCurrentFlags(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	for _, block := range markdownCodeBlocks(string(data), "bash") {
		if strings.Contains(block, "--skip-daemon-sets") {
			t.Fatalf("README.md uses removed flag spelling in block:\n%s", block)
		}
	}
}

func hasRBACRule(rules []struct {
	APIgroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}, resource, verb string) bool {
	for _, rule := range rules {
		if contains(rule.Resources, resource) && contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var fencedBlockPattern = regexp.MustCompile("(?s)```([a-zA-Z0-9_-]*)\\n(.*?)```")

func markdownCodeBlocks(markdown, language string) []string {
	matches := fencedBlockPattern.FindAllStringSubmatch(markdown, -1)
	blocks := make([]string, 0, len(matches))
	for _, match := range matches {
		if match[1] == language {
			blocks = append(blocks, match[2])
		}
	}
	return blocks
}
