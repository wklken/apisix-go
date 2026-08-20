package proxy

import (
	"context"
	"net/http"
	"sort"
)

type priorityGroup struct {
	priority int
	targets  []string
	weights  map[string]int
	selector *RRLoadBalance
}

func newPriorityGroups(servers map[string]int, priorities map[string]int) []priorityGroup {
	if len(servers) == 0 {
		return nil
	}

	weights := make(map[int]map[string]int)
	for target, weight := range servers {
		if weight <= 0 {
			continue
		}
		priority := priorities[target]
		group, ok := weights[priority]
		if !ok {
			group = make(map[string]int)
			weights[priority] = group
		}
		group[target] = weight
	}

	values := make([]int, 0, len(weights))
	for priority := range weights {
		values = append(values, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(values)))

	groups := make([]priorityGroup, 0, len(values))
	for _, priority := range values {
		groupWeights := weights[priority]
		targets := make([]string, 0, len(groupWeights))
		for target := range groupWeights {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		groups = append(groups, priorityGroup{
			priority: priority,
			targets:  targets,
			weights:  groupWeights,
			selector: NewWeightedRRLoadBalance(groupWeights),
		})
	}
	return groups
}

type priorityLoadBalance struct {
	groups []priorityGroup
}

func (lb *priorityLoadBalance) Next() string {
	if len(lb.groups) == 0 {
		return ""
	}
	return lb.groups[0].selector.Next()
}

func (lb *priorityLoadBalance) NextForRequest(request *http.Request) string {
	state := priorityStateForRequest(request)
	state.finishPreviousAttempt()
	if target := lb.nextUntried(state.tried); target != "" {
		state.last = target
		return target
	}
	clear(state.tried)
	target := lb.nextUntried(state.tried)
	state.last = target
	return target
}

func (lb *priorityLoadBalance) nextUntried(tried map[string]struct{}) string {
	for _, group := range lb.groups {
		if target := group.nextUntried(tried, nil); target != "" {
			return target
		}
	}
	return ""
}

func (lb *priorityLoadBalance) RecordSelectedTarget(request *http.Request, target string) {
	recordPriorityTargetAttempt(request, target)
}

type priorityRequestState struct {
	last  string
	tried map[string]struct{}
}

type priorityRequestContextKey struct{}

func priorityStateForRequest(request *http.Request) *priorityRequestState {
	if request == nil {
		return &priorityRequestState{tried: make(map[string]struct{})}
	}
	if state, ok := request.Context().Value(priorityRequestContextKey{}).(*priorityRequestState); ok {
		return state
	}
	state := &priorityRequestState{tried: make(map[string]struct{})}
	*request = *request.WithContext(context.WithValue(request.Context(), priorityRequestContextKey{}, state))
	return state
}

func (state *priorityRequestState) finishPreviousAttempt() {
	if state.last == "" {
		return
	}
	state.tried[state.last] = struct{}{}
	state.last = ""
}

func recordPriorityTargetAttempt(request *http.Request, target string) {
	if target == "" {
		return
	}
	state := priorityStateForRequest(request)
	state.finishPreviousAttempt()
	state.tried[target] = struct{}{}
}

func (group priorityGroup) nextUntried(
	tried map[string]struct{},
	selectable func(string) bool,
) string {
	weights := make(map[string]int, len(group.weights))
	for target, weight := range group.weights {
		if weight <= 0 {
			continue
		}
		if _, alreadyTried := tried[target]; alreadyTried {
			continue
		}
		if selectable != nil && !selectable(target) {
			continue
		}
		weights[target] = weight
	}
	if len(weights) == 0 {
		return ""
	}
	if len(weights) == len(group.weights) {
		return group.selector.Next()
	}
	return NewWeightedRRLoadBalance(weights).Next()
}
