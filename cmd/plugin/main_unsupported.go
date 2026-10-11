//go:build !cgo || !linux

package main

func main() { panic("native plugin build requires Linux with CGO enabled") }
