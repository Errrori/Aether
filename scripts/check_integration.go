//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var mockPattern = regexp.MustCompile(`\btype\s+[mM]ock[A-Z]`)

func main() {
	dirs, err := filepath.Glob("internal/*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var failures []string

	for _, dir := range dirs {
		testFiles, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil || len(testFiles) == 0 {
			continue
		}

		hasMock := false
		hasIntegration := false

		for _, f := range testFiles {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			content := string(data)
			if mockPattern.MatchString(content) {
				hasMock = true
			}
			if strings.Contains(content, "//go:build integration") {
				hasIntegration = true
			}
		}

		if hasMock && !hasIntegration {
			failures = append(failures, dir)
		}
	}

	if len(failures) == 0 {
		fmt.Println("OK: all packages with mock tests have paired integration tests")
		return
	}

	sort.Strings(failures)
	fmt.Fprintf(os.Stderr, "FAIL: %d package(s) have mock tests but lack integration tests:\n", len(failures))
	for _, d := range failures {
		fmt.Fprintf(os.Stderr, "  - %s\n", d)
	}
	os.Exit(1)
}
