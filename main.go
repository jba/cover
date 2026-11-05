// Command cover reports statement and branch points not reached by Go tests.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jba/cover/internal/runner"
)

func main() {
	if err := runner.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "cover:", err)
		os.Exit(1)
	}
}
