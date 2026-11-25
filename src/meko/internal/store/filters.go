package store

import (
	"fmt"
	"sort"
	"strings"
)

// Filter is a mem0 filter: field conditions and AND/OR/NOT lists of sub-filters, the grammar
// mem0's own vector stores accept.
//
//	{"user_id": "u"}                       equality ("*" matches anything)
//	{"role": ["user", "assistant"]}        a list means "in"
//	{"n": {"gte": 2}}                      eq ne gt gte lt lte in nin contains icontains
//	{"OR": [{...}, {...}]}                 also AND, NOT (none may match), and $or $and $not
//
// A filter that spells one operator both ways (OR and $or) is refused: mem0 keeps whichever came
// first, which a decoded JSON object no longer says.
type Filter map[string]any

// logicalOps maps mem0's spellings of the logical operators to one.
var logicalOps = map[string]string{"AND": "AND", "OR": "OR", "NOT": "NOT", "$and": "AND", "$or": "OR", "$not": "NOT"}

// filterSQL compiles f into a SQL condition (with ? placeholders) over a row whose payload keys map
// to SQL expressions through col. Values are always bound as parameters, never spliced in.
func filterSQL(f Filter, col func(key string) (expr string, args []any)) (string, []any, error) {
	if len(f) == 0 {
		return "1", nil, nil
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable SQL, so statements cache and tests can compare
	var parts []string
	var args []any
	seen := map[string]bool{}
	for _, k := range keys {
		v := f[k]
		if op, ok := logicalOps[k]; ok {
			if seen[op] {
				return "", nil, fmt.Errorf("the filter uses both spellings of %s; use one", op)
			}
			seen[op] = true
			subs, ok := v.([]any)
			if !ok {
				return "", nil, fmt.Errorf("%s needs a list of filters", k)
			}
			var subSQL []string
			for _, s := range subs {
				sm, ok := s.(map[string]any)
				if !ok {
					return "", nil, fmt.Errorf("%s needs a list of filters", k)
				}
				q, a, err := filterSQL(sm, col)
				if err != nil {
					return "", nil, err
				}
				subSQL = append(subSQL, "("+q+")")
				args = append(args, a...)
			}
			switch {
			case op == "AND" && len(subSQL) == 0, op == "NOT" && len(subSQL) == 0:
				parts = append(parts, "1")
			case op == "OR" && len(subSQL) == 0:
				parts = append(parts, "0")
			case op == "AND":
				parts = append(parts, strings.Join(subSQL, " AND "))
			case op == "OR":
				parts = append(parts, "("+strings.Join(subSQL, " OR ")+")")
			default:
				parts = append(parts, "NOT ("+strings.Join(subSQL, " OR ")+")")
			}
			continue
		}
		expr, exprArgs := col(k)
		q, a, err := condition(expr, exprArgs, v)
		if err != nil {
			return "", nil, fmt.Errorf("filter on %q: %w", k, err)
		}
		parts = append(parts, q)
		args = append(args, a...)
	}
	return strings.Join(parts, " AND "), args, nil
}

// condition is one field's test. expr may itself take arguments (a JSON path), which come first.
func condition(expr string, exprArgs []any, v any) (string, []any, error) {
	with := func(q string, a ...any) (string, []any, error) {
		return q, append(append([]any{}, exprArgs...), a...), nil
	}
	switch v := v.(type) {
	case nil:
		return with(expr + " IS NULL")
	case string:
		if v == "*" {
			return "1", nil, nil
		}
		return with(expr+" = ?", v)
	case []any:
		return inList(expr, exprArgs, v, false)
	case map[string]any:
		if len(v) != 1 {
			return "", nil, fmt.Errorf("an operator object needs exactly one operator")
		}
		for op, arg := range v {
			switch op {
			case "eq":
				return condition(expr, exprArgs, arg)
			case "ne":
				if arg == nil {
					return with(expr + " IS NOT NULL")
				}
				return with(expr+" IS NOT ?", arg)
			case "gt", "gte", "lt", "lte":
				sym := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
				return with(expr+" "+sym+" ?", arg)
			case "in", "nin":
				list, ok := arg.([]any)
				if !ok {
					return "", nil, fmt.Errorf("%s needs a list", op)
				}
				return inList(expr, exprArgs, list, op == "nin")
			case "contains":
				return with("(typeof("+expr+") = 'text' AND instr("+expr+", ?) > 0)", repeat(exprArgs, arg)...)
			case "icontains":
				return with("(typeof("+expr+") = 'text' AND instr(lower("+expr+"), lower(?)) > 0)",
					repeat(exprArgs, arg)...)
			}
			return "", nil, fmt.Errorf("unknown operator %q", op)
		}
	}
	return with(expr+" = ?", v) // numbers and booleans
}

// repeat is the arguments for a condition that names expr twice: expr's own arguments again,
// then arg. (with prepends the first copy.)
func repeat(exprArgs []any, arg any) []any {
	return append(append([]any{}, exprArgs...), arg)
}

func inList(expr string, exprArgs, list []any, negate bool) (string, []any, error) {
	if len(list) == 0 {
		if negate {
			return "1", nil, nil
		}
		return "0", nil, nil
	}
	q := strings.TrimSuffix(strings.Repeat("?, ", len(list)), ", ")
	if negate { // a missing key isn't in any list, as in mem0 (NULL NOT IN ... is NULL in SQL)
		args := append(append(append([]any{}, exprArgs...), exprArgs...), list...)
		return "(" + expr + " IS NULL OR " + expr + " NOT IN (" + q + "))", args, nil
	}
	return expr + " IN (" + q + ")", append(append([]any{}, exprArgs...), list...), nil
}
