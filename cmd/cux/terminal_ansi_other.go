//go:build !windows

package main

func enableANSIOutput() bool { return true }
func enableUnicodeOutput()   {}
