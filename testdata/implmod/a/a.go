package a

type Runner interface {
	RunSomethingLong(n int) error
}

type Getter[T any] interface {
	GetValue() T
}

type Base struct{}

func (Base) RunSomethingLong(n int) error { return nil }

// NewBase returns a Base. The name is not used.
func NewBase(name string) Base { return Base{} }
