package graphql

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Parse parses a GraphQL query document.
func Parse(query string) (*ast.QueryDocument, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty graphql query")
	}
	return parser.ParseQuery(&ast.Source{Name: "request.graphql", Input: query})
}

// Operation selects the operation to execute. An empty name is only accepted
// when the document contains exactly one operation.
func Operation(doc *ast.QueryDocument, name string) (*ast.OperationDefinition, error) {
	if name != "" {
		for _, operation := range doc.Operations {
			if operation.Name == name {
				return operation, nil
			}
		}
		return nil, fmt.Errorf("operation %q is not defined", name)
	}
	if len(doc.Operations) != 1 {
		return nil, fmt.Errorf("operation name is required")
	}
	return doc.Operations[0], nil
}

// MaxDepth returns the maximum selection depth of the selected operation,
// expanding fragments and inline fragments without adding a level. Cyclic
// fragments are expanded at most once per branch.
func MaxDepth(doc *ast.QueryDocument, operationName string) (int, error) {
	operation, err := Operation(doc, operationName)
	if err != nil {
		return 0, err
	}
	return OperationDepth(doc, operation)
}

// OperationDepth returns the maximum selection depth of a single operation.
func OperationDepth(doc *ast.QueryDocument, operation *ast.OperationDefinition) (int, error) {
	return selectionDepth(doc, operation.SelectionSet, map[string]bool{})
}

func selectionDepth(doc *ast.QueryDocument, selections ast.SelectionSet, expanding map[string]bool) (int, error) {
	depth := 0
	for _, selection := range selections {
		item := 0
		var err error
		switch sel := selection.(type) {
		case *ast.Field:
			item = 1
			if len(sel.SelectionSet) > 0 {
				item, err = selectionDepth(doc, sel.SelectionSet, expanding)
				if err != nil {
					return 0, err
				}
				item++
			}
		case *ast.InlineFragment:
			item, err = selectionDepth(doc, sel.SelectionSet, expanding)
		case *ast.FragmentSpread:
			if expanding[sel.Name] {
				continue
			}
			fragment := doc.Fragments.ForName(sel.Name)
			if fragment == nil {
				return 0, fmt.Errorf("undefined graphql fragment %q", sel.Name)
			}
			expanding[sel.Name] = true
			item, err = selectionDepth(doc, fragment.SelectionSet, expanding)
			delete(expanding, sel.Name)
		}
		if err != nil {
			return 0, err
		}
		if item > depth {
			depth = item
		}
	}
	return depth, nil
}
