package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/strio-dev/bale/plugins/helm-relocate/helmcli"
)

func main() {
	if err := helmcli.Run(context.Background(), os.Args[1:], helmcli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}); err != nil {
		var child *helmcli.ExitError
		if errors.As(err, &child) {
			os.Exit(child.Code)
		}
		fmt.Fprintf(os.Stderr, "bale: %v\n", err)
		os.Exit(1)
	}
}
