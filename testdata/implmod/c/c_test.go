package c

type mockRunner struct{}

func (mockRunner) RunSomethingLong(n int) error { return nil }
