package controls_test

import (
	"os/exec"
	"strings"
	"testing"
)

// runtimeAllowed is every module controls' own package may reach besides the
// standard library. Framework-free means go/errors is the only external one, so
// anything new in the graph fails here until it is added on purpose.
var runtimeAllowed = map[string]bool{
	"gitlab.com/phpboyscout/go/controls": true,
	"gitlab.com/phpboyscout/go/errors":   true,
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

		// The standard library has no module.
		if module == "" || runtimeAllowed[module] || reported[module] {
			continue
		}

		reported[module] = true

		t.Errorf("module not on the runtime allowlist: %s (package %s)", module, pkg)
	}

	if !sawErrors {
		t.Fatal("the scan does not contain gitlab.com/phpboyscout/go/errors, so it did not see the graph")
	}
}

// testGraphForbidden is what the repository may not touch at all, test files
// included. It is a denylist where the runtime tier is an allowlist: tests may
// exercise what the runtime keeps out, and godog brings pflag, testcontainers
// brings OpenTelemetry.
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

// TestMatcherRespectsPathBoundary is the test tier's self-test: a healthy
// repository matches nothing, so a matcher that stopped matching would look clean.
func TestMatcherRespectsPathBoundary(t *testing.T) {
	t.Parallel()

	for _, module := range []string{
		"gitlab.com/phpboyscout/go-tool-base",
		"github.com/spf13/viper",
		"github.com/spf13/cobra",
		"github.com/disgoorg",
		"gitlab.com/phpboyscout/go-tool-base/pkg",
	} {
		if !matchesModule(testGraphForbidden, module) {
			t.Errorf("%s should be forbidden in the test graph", module)
		}
	}

	for _, module := range []string{
		"github.com/spf13/viper-fork",
		"github.com/stretchr/testify",
	} {
		if matchesModule(testGraphForbidden, module) {
			t.Errorf("%s should be permitted in the test graph", module)
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
