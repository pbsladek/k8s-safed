package examples

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pbsladek/k8s-safed/cmd"
	"github.com/pbsladek/k8s-safed/pkg/config"
	"github.com/spf13/pflag"
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

func TestMarkdownDrainExamplesUseCurrentFlags(t *testing.T) {
	drainCmd := cmd.NewDrainCommand()
	allowed := map[string]bool{
		"context": true, // kubeconfig persistent flag commonly shown with plugins
	}
	drainCmd.Flags().VisitAll(func(flag *pflag.Flag) {
		allowed[flag.Name] = true
	})

	for _, path := range []string{
		filepath.Join("..", "..", "README.md"),
		filepath.Join("..", "user-guide.md"),
		filepath.Join("..", "index.md"),
		"README.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, command := range markdownDrainCommands(string(data)) {
			if strings.Contains(command, "--skip-daemon-sets") {
				t.Fatalf("%s uses hidden compatibility flag spelling in command:\n%s", path, command)
			}
			for _, flag := range flagsInCommand(command) {
				if !allowed[flag] {
					t.Fatalf("%s documents unknown drain flag --%s in command:\n%s", path, flag, command)
				}
			}
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

func markdownDrainCommands(markdown string) []string {
	var commands []string
	for _, block := range markdownCodeBlocks(markdown, "bash") {
		normalized := strings.ReplaceAll(block, "\\\n", " ")
		for _, line := range strings.Split(normalized, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "kubectl safed drain") {
				commands = append(commands, line)
			}
		}
	}
	return commands
}

var longFlagPattern = regexp.MustCompile(`(?:^|\s)--([A-Za-z0-9-]+)(?:[=\s]|$)`)

func flagsInCommand(command string) []string {
	matches := longFlagPattern.FindAllStringSubmatch(command, -1)
	flags := make([]string, 0, len(matches))
	for _, match := range matches {
		flags = append(flags, match[1])
	}
	return flags
}
