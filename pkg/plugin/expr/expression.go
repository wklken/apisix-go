package expr

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

type Resolver func(name string) any

type Expression struct {
	logic     string
	children  []*Expression
	condition *condition
}

type condition struct {
	variable string
	operator string
	right    any
	reverse  bool
	pattern  *regexp.Regexp
	prefixes []netip.Prefix
}

func Compile(value any) (*Expression, error) {
	rules, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("expression must be an array")
	}
	if len(rules) == 0 {
		return &Expression{logic: "AND"}, nil
	}
	if _, ok := rules[0].([]any); !ok {
		logic, isString := rules[0].(string)
		if !isString || !isLogicOperator(logic) {
			return nil, fmt.Errorf("expression rule should be wrapped inside brackets")
		}
	}
	return compile(rules)
}

func compile(value any) (*Expression, error) {
	rules, ok := value.([]any)
	if !ok || len(rules) == 0 {
		return nil, fmt.Errorf("expression must be a non-empty array")
	}

	if logic, ok := rules[0].(string); ok && isLogicOperator(logic) {
		if len(rules) < 3 {
			return nil, fmt.Errorf("logical expression %s requires at least two operands", logic)
		}
		children := make([]*Expression, 0, len(rules)-1)
		for _, child := range rules[1:] {
			compiled, err := compile(child)
			if err != nil {
				return nil, err
			}
			children = append(children, compiled)
		}
		return &Expression{logic: strings.ToUpper(logic), children: children}, nil
	}

	if len(rules) == 3 || len(rules) == 4 {
		if _, ok := rules[0].(string); ok {
			return compileCondition(rules)
		}
	}

	if _, ok := rules[0].([]any); !ok {
		return nil, fmt.Errorf("expression must contain conditions or a logical operator")
	}
	current, err := compile(rules[0])
	if err != nil {
		return nil, err
	}
	pending := "AND"
	wantsCondition := false
	for _, item := range rules[1:] {
		if logic, ok := item.(string); ok {
			logic = strings.ToUpper(logic)
			if logic != "AND" && logic != "OR" {
				return nil, fmt.Errorf("invalid infix logical operator %q", logic)
			}
			if wantsCondition {
				return nil, fmt.Errorf("logical operator %q requires a following condition", pending)
			}
			pending = logic
			wantsCondition = true
			continue
		}
		child, err := compile(item)
		if err != nil {
			return nil, err
		}
		current = &Expression{logic: pending, children: []*Expression{current, child}}
		pending = "AND"
		wantsCondition = false
	}
	if wantsCondition {
		return nil, fmt.Errorf("logical operator %q requires a following condition", pending)
	}
	return current, nil
}

func isLogicOperator(operator string) bool {
	switch strings.ToUpper(operator) {
	case "AND", "OR", "!AND", "!OR":
		return true
	default:
		return false
	}
}

func compileCondition(parts []any) (*Expression, error) {
	item := &condition{variable: fmt.Sprint(parts[0])}
	if len(parts) == 4 {
		if fmt.Sprint(parts[1]) != "!" {
			return nil, fmt.Errorf("invalid negated condition")
		}
		item.reverse = true
		item.operator = strings.ToLower(fmt.Sprint(parts[2]))
		item.right = parts[3]
	} else {
		item.operator = strings.ToLower(fmt.Sprint(parts[1]))
		item.right = parts[2]
	}
	switch item.operator {
	case "!=":
		item.operator = "~="
	case "~":
		item.operator = "~~"
	case "!~":
		item.operator = "~~"
		item.reverse = !item.reverse
	}

	switch item.operator {
	case "==", "~=", ">", ">=", "<", "<=", "has":
	case "in":
		if _, ok := values(item.right); !ok {
			return nil, fmt.Errorf("in operator requires an array")
		}
	case "~~", "~*":
		options := ""
		if item.operator == "~*" {
			options = "(?i)"
		}
		pattern, err := regexp.Compile(options + fmt.Sprint(item.right))
		if err != nil {
			return nil, fmt.Errorf("invalid expression regex: %w", err)
		}
		item.pattern = pattern
	case "ipmatch":
		items, ok := values(item.right)
		if !ok {
			items = []any{item.right}
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("ipmatch operator requires at least one address")
		}
		for _, value := range items {
			prefix, err := parseIPPrefix(fmt.Sprint(value))
			if err != nil {
				return nil, err
			}
			item.prefixes = append(item.prefixes, prefix)
		}
	default:
		return nil, fmt.Errorf("invalid operator %q", item.operator)
	}
	return &Expression{condition: item}, nil
}

func parseIPPrefix(value string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix, nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid ipmatch value %q", value)
	}
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func (e *Expression) Eval(resolve Resolver) bool {
	if e.condition != nil {
		actual := resolve(e.condition.variable)
		if e.condition.reverse && isNumericOperator(e.condition.operator) {
			if _, err := strconv.ParseFloat(stringValue(actual), 64); err != nil {
				return false
			}
		}
		matched := e.condition.eval(func(name string) any {
			if name == e.condition.variable {
				return actual
			}
			return resolve(name)
		})
		if e.condition.reverse {
			return !matched
		}
		return matched
	}

	matched := e.logic == "AND" || e.logic == "!AND"
	if e.logic == "OR" || e.logic == "!OR" {
		matched = false
	}
	for _, child := range e.children {
		if e.logic == "AND" || e.logic == "!AND" {
			matched = matched && child.Eval(resolve)
			if !matched {
				break
			}
		} else {
			matched = matched || child.Eval(resolve)
			if matched {
				break
			}
		}
	}
	if e.logic == "!AND" || e.logic == "!OR" {
		return !matched
	}
	return matched
}

func isNumericOperator(operator string) bool {
	switch operator {
	case ">", ">=", "<", "<=":
		return true
	default:
		return false
	}
}

func (c *condition) eval(resolve Resolver) bool {
	actual := resolve(c.variable)
	switch c.operator {
	case "==":
		return equal(actual, c.right)
	case "~=":
		return !equal(actual, c.right)
	case ">", ">=", "<", "<=":
		left, leftErr := strconv.ParseFloat(stringValue(actual), 64)
		right, rightErr := strconv.ParseFloat(stringValue(c.right), 64)
		if leftErr != nil || rightErr != nil {
			return false
		}
		switch c.operator {
		case ">":
			return left > right
		case ">=":
			return left >= right
		case "<":
			return left < right
		default:
			return left <= right
		}
	case "~~", "~*":
		return c.pattern.MatchString(stringValue(actual))
	case "in":
		items, _ := values(c.right)
		for _, value := range items {
			if equal(actual, value) {
				return true
			}
		}
		return false
	case "has":
		items, ok := values(actual)
		if !ok {
			return false
		}
		for _, value := range items {
			if equal(value, c.right) {
				return true
			}
		}
		return false
	case "ipmatch":
		address, err := netip.ParseAddr(stringValue(actual))
		if err != nil {
			return false
		}
		for _, prefix := range c.prefixes {
			if prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

func equal(left any, right any) bool {
	switch right.(type) {
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		leftNumber, leftErr := strconv.ParseFloat(stringValue(left), 64)
		rightNumber, rightErr := strconv.ParseFloat(stringValue(right), 64)
		return leftErr == nil && rightErr == nil && leftNumber == rightNumber
	default:
		return stringValue(left) == stringValue(right)
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(typed, ",")
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

func values(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		items := make([]any, len(typed))
		for i, value := range typed {
			items[i] = value
		}
		return items, true
	default:
		return nil, false
	}
}
