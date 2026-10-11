package controls_test

import (
	"os/exec"
	"strings"
	"testing"
)

// runtimeForbidden is what controls' own package may not pull in: framework-free
// means its only external dependency is gitlab.com/phpboyscout/go/errors, and a
// regression that reintroduces framework coupling bloats every downstream binary.
var runtimeForbidden = []string{
	"gitlab.com/phpboyscout/go-tool-base",
	"github.com/spf13/viper",
	"github.com/spf13/pflag",
	"github.com/spf13/cobra",
	"github.com/charmbracelet",
	"go.opentelemetry.io",
	"github.com/aws/aws-sdk-go",
	"github.com/aws/aws-sdk-go-v2",
	"cloud.google.com/go",
	"github.com/Azure/azure-sdk-for-go",
}

// TestDependencyFootprint scans the package, not ./..., so a subpackage's
// dependencies would never count as runtime. lint/ holds its own go.mod and is
// outside either scan (spec 0005 D1).
func TestDependencyFootprint(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps",
		"-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Path}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	sawErrors := false
	reported := map[string]bool{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg, module, _ := strings.Cut(line, "\t")

		// The one dependency that must be there proves the scan walked the graph.
		if module == "gitlab.com/phpboyscout/go/errors" {
			sawErrors = true
		}

		if !matchesModule(runtimeForbidden, module) || reported[module] {
			continue
		}

		reported[module] = true

		t.Errorf("forbidden module in the runtime graph: %s (package %s)", module, pkg)
	}

	if !sawErrors {
		t.Fatal("the scan does not contain gitlab.com/phpboyscout/go/errors, so it did not see the graph")
	}
}

// testGraphForbidden is what the repository may not touch at all, test files
// included. It is narrower than the runtime list on purpose: tests may exercise
// what the runtime keeps out, and godog brings pflag, testcontainers brings
// OpenTelemetry.
var testGraphForbidden = []string{
	"gitlab.com/phpboyscout/go-tool-base",
	"github.com/spf13/viper",
	"github.com/spf13/cobra",
	// The estate's one Discord SDK belongs to comms-discord.
	"github.com/disgoorg",
}

// TestTestGraphFootprint covers what the runtime scan cannot see: a module
// imported only by a _test.go file still becomes a direct requirement in go.mod.
func TestTestGraphFootprint(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps", "-test",
		"-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Path}}{{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps -test: %v", err)
	}

	sawTestMain := false
	reported := map[string]bool{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg, module, _ := strings.Cut(line, "\t")

		// Synthetic .test packages exist only under -test, so one proves the
		// scan walked the test graph rather than the build graph.
		if strings.HasSuffix(pkg, ".test") {
			sawTestMain = true
		}

		if !matchesModule(testGraphForbidden, module) || reported[module] {
			continue
		}

		reported[module] = true

		t.Errorf("forbidden module in the test graph: %s (package %s)", module, pkg)
	}

	if !sawTestMain {
		t.Fatal("the scan contains no .test package, so it did not see the test files")
	}
}

// TestMatcherRespectsPathBoundary is the guards' self-test: a healthy repository
// matches nothing, so a matcher that stopped matching would look clean.
func TestMatcherRespectsPathBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		list      []string
		forbidden []string
		permitted []string
	}{
		{
			list: testGraphForbidden,
			forbidden: []string{
				"gitlab.com/phpboyscout/go-tool-base",
				"github.com/spf13/viper",
				"github.com/spf13/cobra",
				"github.com/disgoorg",
				"gitlab.com/phpboyscout/go-tool-base/pkg",
			},
			permitted: []string{"github.com/spf13/viper-fork", "github.com/stretchr/testify"},
		},
		{
			list: runtimeForbidden,
			forbidden: []string{
				"github.com/charmbracelet/bubbletea",
				"go.opentelemetry.io/otel",
				"github.com/aws/aws-sdk-go",
				"github.com/aws/aws-sdk-go-v2/service/s3",
				"github.com/Azure/azure-sdk-for-go/sdk/azcore",
				"cloud.google.com/go/storage",
			},
			permitted: []string{
				"gitlab.com/phpboyscout/go/errors",
				"github.com/spf13/pflag-extra",
				"github.com/charmbracelet-fork/x",
				"cloud.google.com/gopher",
			},
		},
	}

	for _, c := range cases {
		for _, module := range c.forbidden {
			if !matchesModule(c.list, module) {
				t.Errorf("%s should be forbidden", module)
			}
		}

		for _, module := range c.permitted {
			if matchesModule(c.list, module) {
				t.Errorf("%s should be permitted", module)
			}
		}
	}
}

func matchesModule(list []string, module string) bool {
	for _, bad := range list {
		if module == bad || strings.HasPrefix(module, bad+"/") {
			return true
		}
	}

	return false
}
