package ctx

// add a pool to new map[string]any for each request here

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

var varsPool = sync.Pool{
	New: func() any {
		return make(map[string]any)
	},
}

func newVars() map[string]any {
	return varsPool.Get().(map[string]any)
}

// newRequestVars gives each HTTP request its independent NGINX-style ID.
func newRequestVars() map[string]any {
	vars := newVars()
	var id [16]byte
	_, _ = rand.Read(id[:])
	vars["$request_id"] = hex.EncodeToString(id[:])
	vars["$apisix_request_id"] = vars["$request_id"]
	return vars
}

func ensureRequestVariables(state *RequestState) {
	if state.RequestVars == nil {
		state.RequestVars = newRequestVars()
	}
	if state.ApisixVars == nil {
		state.ApisixVars = newVars()
	}
	if value, exists := state.ApisixVars["$apisix_request_id"]; exists {
		state.RequestVars["$apisix_request_id"] = value
	} else {
		state.ApisixVars["$apisix_request_id"] = state.RequestVars["$apisix_request_id"]
	}
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
