/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import "fmt"

func main() {
	result := compute(10)
	fmt.Println("Result:", result) // dap-breakpoint
}

func compute(limit int) int {
	sum := 0
	for value := 1; value <= limit; value++ {
		sum += value
	}
	return sum
}
