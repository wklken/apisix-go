package ctx

// add a pool to new map[string]any for each request here

import "sync"

var varsPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]any)
	},
}

func newVars() map[string]any {
	return varsPool.Get().(map[string]any)
}

func putBack(vars map[string]any) {
	// Reset fields
	clear(vars)

	// Put back to the pool
	varsPool.Put(vars)
}
