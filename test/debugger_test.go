// Package test contains integration tests for the debugger.
package test

import (
	"testing"

	"x86-64-toy-debugger/debugger"
)

// TestValidateEnvironment validates that the build and test environment works.
func TestValidateEnvironment(t *testing.T) {
	// Equivalent to Catch2's TEST_CASE("validate environment") / REQUIRE(true)
	debugger.SayHello()
}
