package main

import (
	"os"

	"github.com/pbsladek/k8s-safed/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
