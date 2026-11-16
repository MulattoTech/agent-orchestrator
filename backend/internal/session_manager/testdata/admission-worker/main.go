// This deliberately tiny synthetic worker is compiled only by the native
// admission test. It has no agent/provider imports or networking capability.
package main

import (
	"io"
	"os"
	"time"
)

func main() {
	if os.Getenv("AO_ADMISSION_NATIVE_HELPER") != "1" {
		os.Exit(90)
	}
	ready, token := os.Getenv("AO_ADMISSION_HELPER_READY"), os.Getenv("AO_ADMISSION_HELPER_TOKEN")
	if ready == "" || token == "" {
		os.Exit(91)
	}
	if err := os.WriteFile(ready, []byte(token), 0600); err != nil {
		os.Exit(92)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
	}
}
