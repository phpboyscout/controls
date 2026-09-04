package lint_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"gitlab.com/phpboyscout/go/controls/lint"
)

func TestFlagsEveryMeasuredShape(t *testing.T) {
	t.Parallel()

	analysistest.Run(t, analysistest.TestData(), lint.Analyzer, "capture")
}

func TestStaysSilentOnCleanShapes(t *testing.T) {
	t.Parallel()

	analysistest.Run(t, analysistest.TestData(), lint.Analyzer, "clean")
}
