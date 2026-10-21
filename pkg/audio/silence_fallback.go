//go:build !unix

package audio

func suppressStderr(fn func()) {
	fn()
}

func withSuppressedStderr[T any](fn func() (T, error)) (T, error) {
	return fn()
}

func withSuppressedStderrErr(fn func() error) error {
	return fn()
}
