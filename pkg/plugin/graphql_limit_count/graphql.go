package graphql_limit_count

import (
	"fmt"

	"github.com/wklken/apisix-go/pkg/plugin/graphql"
)

func queryDepth(query string) (int, error) {
	doc, err := graphql.Parse(query)
	if err != nil {
		return 0, err
	}
	if len(doc.Operations) == 0 {
		return 0, fmt.Errorf("empty graphql query")
	}
	depth := 0
	for _, operation := range doc.Operations {
		opDepth, err := graphql.OperationDepth(doc, operation)
		if err != nil {
			return 0, err
		}
		depth = max(depth, opDepth)
	}
	return max(depth, 1), nil
}
