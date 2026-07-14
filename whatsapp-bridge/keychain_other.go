//go:build !windows

package main

import "fmt"

// Stub so keychain.go's GOOS switch compiles on every platform; the Windows
// implementation lives in keychain_windows.go and this is never called
// off-Windows (runtime.GOOS gates the call site).
func getOrCreateDBKeyWindows(service, account string) (string, error) {
	return "", fmt.Errorf("windows credential store called on non-windows build (service=%s account=%s)", service, account)
}
