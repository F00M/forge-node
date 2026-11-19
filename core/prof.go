//go:build prof

// Profiling hooks for local measurement only (go build -tags prof); the shipped binary has none.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"time"
)

var cpuProfile = flag.String("cpuprofile", "", "write a CPU profile to this file")
var profSeconds = flag.Int("profsecs", 60, "seconds to profile before writing the profile")

func init() {
	startHooks = append(startHooks, func() {
		if *cpuProfile == "" {
			return
		}
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		pprof.StartCPUProfile(f)
		go func() {
			time.Sleep(time.Duration(*profSeconds) * time.Second)
			pprof.StopCPUProfile()
			f.Close()
			fmt.Fprintln(os.Stderr, "[forgecore] profile written")
		}()
	})
}
