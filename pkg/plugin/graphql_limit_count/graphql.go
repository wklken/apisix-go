package graphql_limit_count

import (
	"fmt"

	"github.com/wklken/apisix-go/pkg/plugin/graphql"
)

func isNameChar(ch byte) bool {
	return ch == '_' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

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

func templateVariables(template string) []string {
	var variables []string
	for i := 0; i < len(template); i++ {
		if template[i] != '$' {
			continue
		}
		start := i + 1
		end := start
		if start < len(template) && template[start] == '{' {
			start++
			end = start
			for end < len(template) && template[end] != '}' {
				end++
			}
			if end < len(template) {
				variables = append(variables, template[start:end])
				i = end
			}
			continue
		}
		for end < len(template) && isNameChar(template[end]) {
			end++
		}
		if end > start {
			variables = append(variables, template[start:end])
			i = end - 1
		}
	}
	return variables
}
