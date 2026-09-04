// Command singleuse runs the singleuse analyzer over the packages named on
// the command line, in the manner of go vet.
//
//	go run gitlab.com/phpboyscout/go/controls/lint/cmd/singleuse@latest ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"gitlab.com/phpboyscout/go/controls/lint"
)

func main() { singlechecker.Main(lint.Analyzer) }
