package b

import "example.com/implmod/a"

// Embedder implements a.Runner only through struct embedding.
type Embedder struct {
	a.Base
}

// ViaInterface implements a.Runner by embedding the interface.
type ViaInterface struct {
	a.Runner
}
