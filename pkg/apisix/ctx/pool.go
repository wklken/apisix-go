package ctx

// add a pool to new map[string]any for each request here

import "sync"

var varsPool = sync.Pool{
	New: func() any {
		return make(map[string]any)
	},
}

func newVars() map[string]any {
	return varsPool.Get().(map[string]any)
}

func putBack(vars map[string]any) {
	if vars == nil {
		return
	}
	clear(vars)
	varsPool.Put(vars)
}

func newRequestState() *RequestState {
	return new(RequestState)
}

func putRequestState(state *RequestState) {
	if state == nil || state.recycled.Swap(true) {
		return
	}
	putBack(state.ApisixVars)
	putBack(state.RequestVars)
	state.ApisixVars = nil
	state.RequestVars = nil
	clear(state.sensitiveQueryNames)
	state.sensitiveQueryNames = nil
}
