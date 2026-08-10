package plugin

// EnabledSet is the immutable membership snapshot used by a route build.
type EnabledSet struct {
	members map[string]struct{}
}

// NewEnabledSet clones names into a membership set.
func NewEnabledSet(names []string) EnabledSet {
	members := make(map[string]struct{}, len(names))
	for _, name := range names {
		members[name] = struct{}{}
	}
	return EnabledSet{members: members}
}

// Contains reports whether name is enabled in the set.
func (s EnabledSet) Contains(name string) bool {
	_, ok := s.members[name]
	return ok
}
