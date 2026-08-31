//go:build !linux

package main

// confineIfAsked does nothing where nothing re-executes itself to confine.
//
// macOS confines through sandbox-exec, which is a program of its own, and
// every other platform confines nothing at all.
func confineIfAsked() {}
