// Command cover reports statement and branch points not reached by Go tests.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "cover:", err)
		os.Exit(1)
	}
}
