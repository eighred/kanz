package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/eighred/kanz/services/web-bff/internal/edgeconfig"
)

// This init-container mode reads no session, signing, OIDC or tunnel credentials.
// The only values emitted are the closed proxy configuration and bounded ports.
func runEdgeConfig(args []string) int {
	o := edgeconfig.Default()
	flags := flag.NewFlagSet("envoy-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("output", "", "configuration output file in an existing directory")
	flags.IntVar(&o.ListenPort, "listen-port", o.ListenPort, "loopback application port")
	flags.IntVar(&o.BFFPort, "bff-port", o.BFFPort, "loopback BFF upstream port")
	flags.IntVar(&o.AdminPort, "admin-port", o.AdminPort, "readiness/metrics port")
	flags.IntVar(&o.RequestsPerSecond, "requests-per-second", o.RequestsPerSecond, "per-process request budget")
	flags.BoolVar(&o.AdminLoopback, "admin-loopback", false, "bind the control listener to loopback for local use")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *path == "" {
		fmt.Fprintln(os.Stderr, "invalid edge configuration arguments")
		return 2
	}
	if err := edgeconfig.Write(*path, o); err != nil {
		fmt.Fprintln(os.Stderr, "edge configuration generation failed")
		return 2
	}
	return 0
}
