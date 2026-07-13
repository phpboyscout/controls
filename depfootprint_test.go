package controls_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDependencyFootprint is the enforceable statement of "framework-free": the
// module's transitive dependency graph must not pull in go-tool-base, the CLI/
// config/TUI stack, OpenTelemetry, or any cloud SDK. controls is a pure lifecycle
// supervisor whose only external dependency is cockroachdb/errors; a regression
// that reintroduces framework coupling fails here rather than silently bloating
// every downstream binary.
func TestDependencyFootprint(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	forbidden := []string{
		"gitlab.com/phpboyscout/go-tool-base",
		"github.com/spf13/viper",
		"github.com/spf13/pflag",
		"github.com/spf13/cobra",
		"github.com/charmbracelet",
		"go.opentelemetry.io",
		"github.com/aws/aws-sdk-go",
		"cloud.google.com/go",
		"github.com/Azure/azure-sdk",
	}

	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.HasPrefix(dep, bad) {
				t.Errorf("forbidden dependency in graph: %s (matched %q)", dep, bad)
			}
		}
	}
}
