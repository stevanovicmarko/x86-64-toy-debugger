// Package test contains integration tests for the debugger.
package test

import (
	"testing"

	"x86-64-toy-debugger/debugger"
)

// TestValidateEnvironment validates that the build and test environment works.
func TestValidateEnvironment(t *testing.T) {
	// Verify the debugger package compiles and the Process type is accessible.
	var _ *debugger.Process
}
