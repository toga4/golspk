// Package other is a second module in the same workspace.
package other

import "errors"

// ErrOther is an error of package other.
var ErrOther = errors.New("other")
