/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

const header = `NOTICES AND INFORMATION
Do Not Translate or Localize

This software incorporates material from third parties.
Microsoft makes certain open source code available at https://3rdpartysource.microsoft.com,
or you may send a check or money order for US $5.00, including the product name,
the open source component name, platform, and version number, to:

Source Code Compliance Team
Microsoft Corporation
One Microsoft Way
Redmond, WA 98052
USA

Notwithstanding any other terms, you may reverse engineer this software to the extent
required to debug changes to any libraries licensed under the GNU Lesser General Public License.`

const separator = "\n----------------------------------------------------------\n"

func main() {
	// Read directory contents
	files, err := os.ReadDir(".")
	if err != nil {
		log.Fatal(err)
	}

	// Regex pattern to match license blocks with named groups
	licenseBlockRegex := regexp.MustCompile(`BEGIN_LICENSE (?P<name>.*@.*)\s(?P<license>[\s\S]*?)\sEND_LICENSE .*@.*`)

	licenses := make(map[string]string)
	var orderedKeys []string

	// Process files that start with "NOTICE."
	for _, file := range files {
		if file.IsDir() || !strings.HasPrefix(file.Name(), "NOTICE.") {
			continue
		}

		content, contentErr := os.ReadFile(file.Name())
		if contentErr != nil {
			log.Printf("Error reading file %s: %v", file.Name(), contentErr)
			continue
		}

		// Find all matches of the license block pattern
		matches := licenseBlockRegex.FindAllStringSubmatch(string(content), -1)

		for _, match := range matches {
			// match[0] is the full match, match[1] is "name", match[2] is "license"
			if len(match) >= 3 {
				name := match[1]
				license := match[2]

				// Add to licenses map and orderedKeys slice if not already present
				if _, exists := licenses[name]; !exists {
					orderedKeys = append(orderedKeys, name)
				}
				licenses[name] = license
			}
		}
	}

	// Build the notice content
	var licenseBlocks []string
	for _, key := range orderedKeys {
		license := licenses[key]
		licenseBlocks = append(licenseBlocks, "\n"+strings.TrimSpace(license)+"\n")
	}

	noticeContent := header + "\n" + separator + strings.Join(licenseBlocks, separator+separator)

	// Write the NOTICE file
	err = os.WriteFile("NOTICE", []byte(noticeContent), 0644)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("NOTICE file has been created.")
}
