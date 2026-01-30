/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package main provides a simple program for debugging tests.
// This program is used as a target for DAP proxy integration tests with Delve.
package main

import (
	"fmt"
	"os"
)

func main() {
	// This is a breakpoint target line - tests will set breakpoints here
	result := compute(10) // Line 18 - breakpoint target
	fmt.Printf("Result: %d\n", result)
	os.Exit(0)
}

// compute performs a simple computation that can be stepped through.
func compute(n int) int {
	sum := 0
	for i := 1; i <= n; i++ {
		sum += i // Line 26 - can step through loop iterations
	}
	return sum
}
