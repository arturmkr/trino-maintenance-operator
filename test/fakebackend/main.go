// fakebackend serves the in-memory Trino Gateway or Trino coordinator fake over
// HTTP. It is packaged into a tiny image for the Argo CD test so that no real
// Trino or Trino Gateway is needed.
//
//	fakebackend --mode=gateway --backend general-1=http://fake-trino-1:8080=general ...
//	fakebackend --mode=trino --version=481
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/arturmkr/trino-maintenance-operator/internal/fakes"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
)

type backendFlags []string

func (b *backendFlags) String() string     { return strings.Join(*b, ",") }
func (b *backendFlags) Set(v string) error { *b = append(*b, v); return nil }

func main() {
	var (
		mode     = flag.String("mode", "", "gateway or trino")
		addr     = flag.String("listen", ":8080", "listen address")
		version  = flag.String("version", "481", "Trino version reported by /v1/info (trino mode)")
		backends backendFlags
	)
	flag.Var(&backends, "backend", "backend to pre-register as name=proxyTo=routingGroup (gateway mode, repeatable)")
	flag.Parse()

	var handler http.Handler
	switch *mode {
	case "gateway":
		list := make([]gateway.Backend, 0, len(backends))
		for _, spec := range backends {
			parts := strings.SplitN(spec, "=", 3)
			if len(parts) != 3 {
				fmt.Fprintf(os.Stderr, "invalid --backend %q, want name=proxyTo=routingGroup\n", spec)
				os.Exit(2)
			}
			list = append(list, gateway.Backend{
				Name: parts[0], ProxyTo: parts[1], RoutingGroup: parts[2], Active: true, Status: "HEALTHY",
			})
		}
		handler = fakes.NewGateway(list...)
	case "trino":
		handler = fakes.NewCoordinator(*version)
	default:
		fmt.Fprintln(os.Stderr, "--mode must be gateway or trino")
		os.Exit(2)
	}

	log.Printf("fake %s listening on %s", *mode, *addr)
	server := &http.Server{Addr: *addr, Handler: handler}
	log.Fatal(server.ListenAndServe())
}
