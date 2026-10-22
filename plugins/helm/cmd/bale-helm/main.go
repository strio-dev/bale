// bale-helm is a WASI command that converts one render request into dependencies.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	helmplugin "github.com/strio-dev/bale/plugins/helm"
)

func run(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, helmplugin.MaxRequestBytes+1))
	if err != nil {
		return err
	}
	result, err := helmplugin.Discover(context.Background(), data)
	if err != nil {
		return err
	}
	_, err = output.Write(result)
	return err
}
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
