package c

// Box is a generic type that implements a.Runner and a.Getter.
type Box[T any] struct{ v T }

func (b *Box[T]) RunSomethingLong(n int) error { return nil }

func (b *Box[T]) GetValue() T { return b.v }

// IntGetter implements a.Getter[int].
type IntGetter struct{}

func (IntGetter) GetValue() int { return 0 }
