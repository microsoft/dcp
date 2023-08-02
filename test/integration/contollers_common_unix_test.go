//go:build !windows

package integration_test

func stopTestEnvironment() error {
	return testEnv.Stop()
}
