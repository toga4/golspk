package d

import (
	"errors"

	"example.com/implmod/a"
)

// ErrD is an error of package d.
var ErrD = errors.New("d")

// Run creates a Base and runs it.
func Run() error {
	return a.NewBase("x").RunSomethingLong(1)
}
