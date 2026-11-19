// forgecore: the native forge server. It binds a TCP port, prints "LISTEN <port>" on stdout and serves
// until killed (or, with --watch-stdin, until its parent closes stdin). main.ts owns the env lifecycle.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
)

// startHooks run after flag parsing (the prof build tag adds a CPU profiler here).
var startHooks []func()

func main() {
	port := flag.Int("port", 0, "TCP port to bind (0 picks an ephemeral one)")
	host := flag.String("host", "", "address to bind (default all interfaces)")
	token := flag.String("token", os.Getenv("MAKEFUN_SHOP_TOKEN"), "the bearer token every request must carry (empty accepts any)")
	watchStdin := flag.Bool("watch-stdin", false, "exit when stdin reaches end of file")
	flag.Parse()
	for _, hook := range startHooks {
		hook()
	}

	// The working set is small and short-lived: trade memory for fewer collections.
	debug.SetGCPercent(400)

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forgecore] listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "LISTEN %d\n", ln.Addr().(*net.TCPAddr).Port)
	if *watchStdin {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}()
	}
	app := NewApp(*token)
	if err := serve(ln, app.handle); err != nil {
		fmt.Fprintf(os.Stderr, "[forgecore] serve: %v\n", err)
		os.Exit(1)
	}
}
