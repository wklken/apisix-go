package config

type nodeKind uint8

const (
	nodeNull nodeKind = iota
	nodeScalar
	nodeMapping
	nodeSequence
)

type valueNode struct {
	kind     nodeKind
	scalar   any
	mapping  map[string]*valueNode
	sequence []*valueNode
	pathBase string
}

func nodeToAny(node *valueNode) any {
	if node == nil {
		return nil
	}

	switch node.kind {
	case nodeNull:
		return nil
	case nodeScalar:
		return node.scalar
	case nodeMapping:
		mapping := make(map[string]any, len(node.mapping))
		for key, child := range node.mapping {
			mapping[key] = nodeToAny(child)
		}
		return mapping
	case nodeSequence:
		sequence := make([]any, len(node.sequence))
		for index, child := range node.sequence {
			sequence[index] = nodeToAny(child)
		}
		return sequence
	default:
		return nil
	}
}

func cloneNode(node *valueNode) *valueNode {
	if node == nil {
		return nil
	}

	cloned := *node
	if node.mapping != nil {
		cloned.mapping = make(map[string]*valueNode, len(node.mapping))
		for key, child := range node.mapping {
			cloned.mapping[key] = cloneNode(child)
		}
	}
	if node.sequence != nil {
		cloned.sequence = make([]*valueNode, len(node.sequence))
		for index, child := range node.sequence {
			cloned.sequence[index] = cloneNode(child)
		}
	}
	return &cloned
}
